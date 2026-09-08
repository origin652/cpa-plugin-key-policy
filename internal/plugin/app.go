package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"

	"cpa-key-policy/internal/plugin/web"
	"cpa-key-policy/internal/policy"
)

type App struct {
	store         *policy.Store
	classifyMu    sync.RWMutex
	classifyCache map[string][]string
	schedulerMu   sync.Mutex
	schedulerRR   map[string]*smoothWeightedState
	schedulerCursor map[string]string
}

const classifyCacheCapacity = 4096

func NewApp() *App {
	store := policy.NewStore()
	_ = store.Configure(policy.DefaultConfig())
	return &App{
		store:         store,
		classifyCache: make(map[string][]string),
		schedulerRR:   make(map[string]*smoothWeightedState),
		schedulerCursor: make(map[string]string),
	}
}

func (a *App) HandleMethod(method string, request []byte) ([]byte, error) {
	return safePluginCall(func() ([]byte, error) {
		return a.handleMethod(method, request)
	})
}

func (a *App) handleMethod(method string, request []byte) ([]byte, error) {
	switch method {
	case MethodPluginRegister, MethodPluginReconfigure:
		if err := a.configure(request); err != nil {
			return nil, err
		}
		return OKEnvelope(a.registration())
	case MethodFrontendAuthIdentifier:
		return OKEnvelope(IdentifierResponse{Identifier: PluginID})
	case MethodFrontendAuthAuthenticate:
		return a.authenticate(request)
	case MethodModelRoute:
		return a.routeModel(request)
	case MethodRequestInterceptBefore:
		return a.interceptRequestBefore(request)
	case MethodRequestInterceptAfter:
		return OKEnvelope(RequestInterceptResponse{})
	case MethodSchedulerPick:
		return a.pickScheduler(request)
	case MethodResponseInterceptAfter:
		return a.interceptResponse(request)
	case MethodUsageHandle:
		return a.handleUsage(request)
	case MethodManagementRegister:
		return OKEnvelope(a.managementRegistration())
	case MethodManagementHandle:
		return a.handleManagement(request)
	default:
		return ErrorEnvelope("unknown_method", "unknown method: "+method, http.StatusNotFound), nil
	}
}

func safePluginCall(call func() ([]byte, error)) (response []byte, err error) {
	defer func() {
		if recover() != nil {
			response = nil
			err = errors.New("plugin panic recovered")
		}
	}()
	return call()
}

func (a *App) configure(raw []byte) error {
	var req LifecycleRequest
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &req); err != nil {
			return err
		}
	}
	cfg, err := policy.DecodeConfig(req.ConfigYAML)
	if err != nil {
		return err
	}
	if err := a.store.Configure(cfg); err != nil {
		return err
	}
	// Register the classify cache clear callback, then clear once for safety.
	a.store.SetOnClassifyRulesChanged(func() {
		a.clearClassifyCache()
		a.clearSchedulerState()
	})
	a.clearClassifyCache()
	a.clearSchedulerState()
	a.store.StartUsageFlusher()
	return nil
}

// Shutdown flushes usage. Host calls this on plugin unload.
func (a *App) Shutdown() {
	a.store.StopUsageFlusher()
}

func (a *App) registration() Registration {
	return Registration{
		SchemaVersion: SchemaVersion,
		Metadata: Metadata{
			Name:             PluginName,
			Version:          Version,
			Author:           "cpa-key-policy",
			GitHubRepository: "https://github.com/router-for-me/CLIProxyAPI",
			ConfigFields: []ConfigField{
				{Name: "enabled", Type: "boolean", Description: "Enable or disable this plugin without unloading it."},
				{Name: "state_file", Type: "string", Description: "JSON state file used for key policy changes made through the Management API."},
				{Name: "global_weighted_round_robin", Type: "boolean", Description: "忽略别名目标的 group，对当前 provider/model 的全部候选凭证执行全局加权轮询。"},
				{Name: "keys", Type: "array", Description: "Downstream key policies, including optional fail-closed account_binding allow globs. State file wins after it exists."},
			},
		},
		Capabilities: Capabilities{
			FrontendAuthProvider:          true,
			FrontendAuthProviderExclusive: false,
			ModelRouter:                   true,
			Scheduler:                     true,
			RequestInterceptor:            true,
			ResponseInterceptor:           true,
			UsagePlugin:                   true,
			ManagementAPI:                 true,
		},
	}
}

func (a *App) authenticate(raw []byte) ([]byte, error) {
	var req FrontendAuthRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	decision := a.store.Authenticate(req.Method, req.Path, req.Headers, req.Query, req.Body)
	if !decision.Known || !decision.Allowed {
		return OKEnvelope(FrontendAuthResponse{Authenticated: false})
	}
	meta := map[string]string{
		"provider":        PluginID,
		"key_id":          decision.KeyID,
		"requested_model": decision.Requested,
	}
	if decision.Rule.Alias != "" {
		meta["alias"] = decision.Rule.Alias
		meta["target_provider"] = decision.Rule.Provider
		meta["target_model"] = decision.Rule.TargetModel
		if decision.Rule.Group != "" {
			// Group lets our Scheduler (scheduler.pick) restrict auth-file
			// selection to a tier/plan (codex plan_type, antigravity tier).
			// Empty = legacy "any file for the provider" behavior.
			meta["group"] = decision.Rule.Group
		}
	}
	return OKEnvelope(FrontendAuthResponse{
		Authenticated: true,
		Principal:     decision.Principal,
		Metadata:      meta,
	})
}

func (a *App) routeModel(raw []byte) ([]byte, error) {
	var req ModelRouteRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	rule, keyID, ok := a.store.Route(req.Headers, req.Query, req.RequestedModel)
	if !ok {
		return OKEnvelope(ModelRouteResponse{Handled: false})
	}
	return OKEnvelope(ModelRouteResponse{
		Handled:     true,
		TargetKind:  "provider",
		Target:      resolveProviderKey(rule.Provider, req.AvailableProviders),
		TargetModel: rule.TargetModel,
		Reason:      "cpa-key-policy:" + keyID,
	})
}

// interceptRequestBefore rejects unsafe credential presentation for an
// explicitly account-bound key before the selected upstream executor runs.
// Frontend-auth cannot express a terminal rejection: Authenticated=false is
// treated by CPA as "try the next provider". The request interceptor can stop
// query-only or conflicting protected credentials without changing the host.
func (a *App) interceptRequestBefore(raw []byte) ([]byte, error) {
	var req RequestInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if !a.store.Enabled() {
		return OKEnvelope(RequestInterceptResponse{})
	}
	resolution := a.store.ResolveRequestKey(req.Headers, req.Metadata)
	if resolution.Key == nil || resolution.Key.AccountBinding == nil {
		return OKEnvelope(RequestInterceptResponse{})
	}
	if !resolution.Key.Enabled {
		return OKEnvelope(requestRejection(http.StatusUnauthorized, "key_disabled", "cpa-key-policy: account-bound key is disabled"))
	}
	if resolution.Conflict {
		return OKEnvelope(requestRejection(http.StatusBadRequest, "credential_conflict", "cpa-key-policy: protected requests must not contain conflicting credentials"))
	}
	if !resolution.HeaderPresent || !resolution.HeaderMatches {
		return OKEnvelope(requestRejection(http.StatusUnauthorized, "header_key_required", "cpa-key-policy: account-bound requests require the configured key in a request header"))
	}
	return OKEnvelope(RequestInterceptResponse{})
}

func requestRejection(status int, code, message string) RequestInterceptResponse {
	body, _ := json.Marshal(map[string]any{
		"error": map[string]any{
			"type":    "authentication_error",
			"code":    code,
			"message": message,
		},
	})
	return RequestInterceptResponse{
		Terminate:       true,
		StatusCode:      status,
		ResponseHeaders: http.Header{"Content-Type": []string{"application/json"}},
		ResponseBody:    body,
	}
}

// resolveProviderKey maps a ModelRule's provider to the provider key CPA's
// auth manager uses, so HasBuiltinProvider(target) succeeds.
//
// OpenAI-compatibility providers are registered with auth.Provider
// prefixed as "openai-compatible-<name>" (see synthesizer.config:
// auth.Provider = OpenAICompatibleProviderKey(name)). The plugin's
// ModelRule.Provider field carries the bare name (e.g. "nvidia",
// "opencode"). Returning the bare name makes CPA skip the router
// ("model router returned unavailable provider") and fall back to the
// native path, which fails for non-native alias names like "test1".
//
// We pick the key present in AvailableProviders, trying the bare name
// first (for built-in providers like codex/claude/gemini) then the
// openai-compatible- prefixed form. If neither matches we return the
// bare name and let CPA's availability check skip us (the native path
// still resolves true model names).
func resolveProviderKey(provider string, availableProviders []string) string {
	p := strings.ToLower(strings.TrimSpace(provider))
	if p == "" {
		return p
	}
	if len(availableProviders) == 0 {
		return p
	}
	for _, candidate := range []string{p, "openai-compatible-" + p} {
		for _, avail := range availableProviders {
			if strings.EqualFold(candidate, avail) {
				return candidate
			}
		}
	}
	return p
}

func (a *App) interceptResponse(raw []byte) ([]byte, error) {
	var req ResponseInterceptRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	// NOTE: billing is NOT done here. The host only invokes
	// response.intercept_after for non-streaming responses (the streaming path
	// goes through response.intercept_stream_chunk, which we don't handle).
	// Billing for both paths is centralized in usage.handle (handleUsage),
	// which the host fires after every request completes with already-parsed
	// token counts. Doing it here too would double-bill non-streaming requests.
	if req.Stream {
		// Streaming responses are not safe to rewrite (SSE framing) — return as-is.
		return OKEnvelope(ResponseInterceptResponse{})
	}
	alias, ok := a.store.ResponseAlias(req.RequestHeaders, nil, req.RequestedModel)
	if !ok {
		return OKEnvelope(ResponseInterceptResponse{})
	}
	body, changed := policy.RewriteTopLevelModel(req.Body, alias)
	if !changed {
		return OKEnvelope(ResponseInterceptResponse{})
	}
	return OKEnvelope(ResponseInterceptResponse{Body: body})
}

// pickScheduler enforces every applicable account constraint before selecting
// a candidate. Once a configured key is recognized, an empty intersection is
// a hard error: Handled=false would return the request to CPA's global pool.
func (a *App) pickScheduler(raw []byte) ([]byte, error) {
	var req SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	if !a.store.Enabled() {
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}
	resolution := a.store.ResolveRequestKey(http.Header(req.Options.Headers), req.Options.Metadata)
	globalMode := a.store.GlobalWeightedRoundRobin()
	key := resolution.Key
	group := schedulerGroupFromMetadata(req.Options.Metadata)
	owner := "metadata:" + group
	var binding *policy.AccountBinding
	if key == nil {
		if group == "" || globalMode || resolution.HeaderPresent {
			// The key is not managed by this plugin. Preserve native CPA routing,
			// including host-owned session affinity.
			return OKEnvelope(SchedulerPickResponse{Handled: false})
		}
	} else {
		if !key.Enabled {
			return ErrorEnvelope("key_disabled", "cpa-key-policy: configured key is disabled", http.StatusUnauthorized), nil
		}
		owner = key.ID
		binding = key.AccountBinding
		if !globalMode || binding != nil {
			var err error
			group, err = policy.SchedulerGroupForKey(key, schedulerRequestedModel(req.Options.Metadata), schedulerRequestProvider(req), req.Model)
			if err != nil {
				return ErrorEnvelope("account_constraint_ambiguous", "cpa-key-policy: cannot determine a unique account constraint: "+err.Error(), http.StatusForbidden), nil
			}
		} else {
			group = ""
		}
	}
	protected := key != nil && (binding != nil || group != "")
	if protected {
		if resolution.Conflict {
			return ErrorEnvelope("credential_conflict", "cpa-key-policy: protected requests must not contain conflicting credentials", http.StatusBadRequest), nil
		}
		if !resolution.HeaderPresent || !resolution.HeaderMatches {
			return ErrorEnvelope("header_key_required", "cpa-key-policy: protected requests require the configured key in a request header", http.StatusUnauthorized), nil
		}
	}
	if len(req.Candidates) == 0 {
		return ErrorEnvelope("auth_not_found", "cpa-key-policy: the host supplied no credential candidates", http.StatusServiceUnavailable), nil
	}

	matched := make([]SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, cand := range req.Candidates {
		if !schedulerCandidateUsable(cand.Status) {
			continue
		}
		if !candidateMatchesRequestedProvider(cand, req) {
			continue
		}
		if binding != nil && !binding.Matches(cand.ID) {
			continue
		}
		if group != "" && !a.candidateMatchesGroup(cand, group) {
			continue
		}
		matched = append(matched, cand)
	}
	if len(matched) == 0 {
		code := "auth_not_found"
		if binding != nil {
			code = "auth_not_bound"
		}
		return ErrorEnvelope(code, "cpa-key-policy: no host candidate satisfies the key's account constraints", http.StatusServiceUnavailable), nil
	}

	maxPriority := 0
	prioritySet := false
	weighted := make([]SchedulerAuthCandidate, 0, len(matched))
	for _, cand := range matched {
		if schedulerCandidateWeight(cand) <= 0 {
			continue
		}
		if !prioritySet || cand.Priority > maxPriority {
			maxPriority = cand.Priority
			prioritySet = true
			weighted = weighted[:0]
		}
		if cand.Priority == maxPriority {
			weighted = append(weighted, cand)
		}
	}
	if len(weighted) == 0 {
		return ErrorEnvelope("auth_not_found", "cpa-key-policy: 匹配的凭证均没有正权重", http.StatusServiceUnavailable), nil
	}

	strategy := policy.BindingStrategyWeightedRoundRobin
	if binding != nil {
		strategy = binding.Strategy
	}
	poolKey := schedulerPoolKey(req, owner, group, maxPriority)
	var picked SchedulerAuthCandidate
	switch strategy {
	case policy.BindingStrategyRoundRobin:
		picked = a.pickRoundRobin(poolKey, weighted)
	case policy.BindingStrategyFillFirst:
		picked = pickFillFirst(req, weighted)
	default:
		picked = a.pickSmoothWeighted(req, owner, group, maxPriority, weighted)
	}
	return OKEnvelope(SchedulerPickResponse{Handled: true, AuthID: picked.ID})
}

func schedulerRequestedModel(metadata map[string]any) string {
	if metadata == nil {
		return ""
	}
	for key, value := range metadata {
		if strings.EqualFold(strings.TrimSpace(key), "requested_model") {
			return strings.TrimSpace(fmt.Sprint(value))
		}
	}
	return ""
}

func schedulerRequestProvider(req SchedulerPickRequest) string {
	if provider := strings.TrimSpace(req.Provider); provider != "" {
		return provider
	}
	if len(req.Providers) == 1 {
		return strings.TrimSpace(req.Providers[0])
	}
	return ""
}

func candidateMatchesRequestedProvider(candidate SchedulerAuthCandidate, req SchedulerPickRequest) bool {
	provider := strings.ToLower(strings.TrimSpace(candidate.Provider))
	if requested := strings.ToLower(strings.TrimSpace(req.Provider)); requested != "" && requested != "mixed" {
		return provider == requested
	}
	if len(req.Providers) == 0 {
		return true
	}
	for _, requested := range req.Providers {
		if provider == strings.ToLower(strings.TrimSpace(requested)) {
			return true
		}
	}
	return false
}

func schedulerCandidateUsable(status string) bool {
	status = strings.ToLower(strings.TrimSpace(status))
	status = strings.NewReplacer("-", "_", " ", "_").Replace(status)
	switch status {
	case "disabled", "error", "expired", "revoked", "invalid", "unavailable", "cooldown", "cooling_down", "quota_exhausted", "exhausted", "blocked":
		return false
	default:
		return true
	}
}

// candidateMatchesGroup reports whether a candidate auth belongs to the
// requested group. It first evaluates user-defined ClassifyRules (which can
// override built-in detection — a candidate may belong to multiple groups).
// If no custom rule matches, it falls back to the built-in plan_type/tier
// detection. Uses an ID-level cache for performance with large auth-file sets.
func (a *App) candidateMatchesGroup(cand SchedulerAuthCandidate, group string) bool {
	groups := a.candidateGroups(cand)
	for _, g := range groups {
		if g == group {
			return true
		}
	}
	return false
}

// candidateGroups returns all groups a candidate belongs to. Custom rules are
// evaluated first (multi-group: a candidate can match multiple rules). If no
// custom rule matches, the built-in plan_type/tier detection runs. Results are
// cached by candidate ID; the cache is cleared on reconfigure.
func (a *App) candidateGroups(cand SchedulerAuthCandidate) []string {
	cacheKey := candidateClassifyCacheKey(cand)
	// Check cache.
	a.classifyMu.RLock()
	if cached, ok := a.classifyCache[cacheKey]; ok {
		a.classifyMu.RUnlock()
		return cached
	}
	a.classifyMu.RUnlock()

	var groups []string
	// 1. Evaluate custom classify rules (multi-group: collect all matches).
	// Group names are stored bare on the rule but stamped/matched with the
	// classify: prefix so they never collide with built-in plan_type values.
	for _, rule := range a.store.ClassifyRulesSnapshot() {
		if !rule.Enabled || rule.Compiled() == nil {
			continue
		}
		val := candidateFieldValue(cand, rule.Field)
		if val != "" && rule.Compiled().MatchString(val) {
			if g := policy.FormatClassifyGroup(rule.Group); g != "" {
				groups = append(groups, g)
			}
		}
	}
	// 2. If no custom rule matched, fall back to built-in plan_type/tier.
	if len(groups) == 0 {
		if g := builtInGroup(cand); g != "" {
			groups = append(groups, g)
		}
	}

	// Cache the result.
	a.classifyMu.Lock()
	if a.classifyCache == nil || len(a.classifyCache) >= classifyCacheCapacity {
		a.classifyCache = make(map[string][]string)
	}
	a.classifyCache[cacheKey] = groups
	a.classifyMu.Unlock()
	return groups
}

func (a *App) clearClassifyCache() {
	a.classifyMu.Lock()
	a.classifyCache = make(map[string][]string)
	a.classifyMu.Unlock()
}

func candidateClassifyCacheKey(cand SchedulerAuthCandidate) string {
	keys := make([]string, 0, len(cand.Attributes))
	for key := range cand.Attributes {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var builder strings.Builder
	builder.WriteString(cand.ID)
	builder.WriteByte(0)
	builder.WriteString(cand.Provider)
	for _, key := range keys {
		builder.WriteByte(0)
		builder.WriteString(key)
		builder.WriteByte('=')
		builder.WriteString(cand.Attributes[key])
	}
	return builder.String()
}

// candidateFieldValue extracts the value of a named field from the candidate.
// Supported fields: "filename" (cand.ID), "provider" (cand.Provider),
// "plan_type" (cand.Attributes["plan_type"]), "tier" (cand.Attributes["tier"]),
// or any custom attribute key.
func candidateFieldValue(cand SchedulerAuthCandidate, field string) string {
	field = strings.ToLower(strings.TrimSpace(field))
	switch field {
	case "filename", "id":
		return cand.ID
	case "provider":
		return cand.Provider
	default:
		if cand.Attributes != nil {
			return cand.Attributes[field]
		}
	}
	return ""
}

// builtInGroup returns the built-in plan_type/tier group for a candidate,
// or "supported" if no recognizable claim is present (untiered bucket).
func builtInGroup(cand SchedulerAuthCandidate) string {
	if cand.Attributes == nil {
		return "supported"
	}
	plan := strings.ToLower(strings.TrimSpace(cand.Attributes["plan_type"]))
	tier := strings.ToLower(strings.TrimSpace(cand.Attributes["tier"]))
	if plan != "" {
		return plan
	}
	if tier != "" {
		return tier
	}
	return "supported"
}

// schedulerGroupFromMetadata reads the group stamped at authenticate time out
// of request-provided scheduler options. Tolerates string or any-typed values.
func schedulerGroupFromMetadata(meta map[string]any) string {
	if meta == nil {
		return ""
	}
	raw, ok := meta["group"]
	if !ok || raw == nil {
		return ""
	}
	switch v := raw.(type) {
	case string:
		return strings.ToLower(strings.TrimSpace(v))
	default:
		return strings.ToLower(strings.TrimSpace(fmt.Sprintf("%v", v)))
	}
}

// finalized, already-parsed token record here after every request completes —
// streaming and non-streaming alike. This is the billing path that covers
// streaming (the host never invokes response.intercept_after on streams).
// Fire-and-forget: we always return an empty success envelope regardless of
// whether we actually billed (best-effort; unknown keys/aliases cost nothing).
func (a *App) handleUsage(raw []byte) ([]byte, error) {
	var req UsageHandleRequest
	// A malformed record must never break the request path: bill nothing.
	if err := json.Unmarshal(raw, &req); err != nil {
		return OKEnvelope(UsageHandleResponse{})
	}
	_ = a.store.RecordUsage(req.APIKey, req.Alias, req.Model, req.Failed, policy.UsageDetail{
		InputTokens:         req.Detail.InputTokens,
		OutputTokens:        req.Detail.OutputTokens,
		ReasoningTokens:     req.Detail.ReasoningTokens,
		CachedTokens:        req.Detail.CachedTokens,
		CacheReadTokens:     req.Detail.CacheReadTokens,
		CacheCreationTokens: req.Detail.CacheCreationTokens,
		TotalTokens:         req.Detail.TotalTokens,
	})
	return OKEnvelope(UsageHandleResponse{})
}

func (a *App) managementRegistration() ManagementRegistrationResponse {
	base := "/plugins/" + PluginID
	return ManagementRegistrationResponse{
		Routes: []ManagementRoute{
			{Method: http.MethodGet, Path: base + "/keys", Description: "List downstream CPA key policies."},
			{Method: http.MethodPost, Path: base + "/keys", Description: "Create a downstream CPA key policy."},
			{Method: http.MethodPatch, Path: base + "/keys", Description: "Update a downstream CPA key policy by id."},
			{Method: http.MethodDelete, Path: base + "/keys", Description: "Delete a downstream CPA key policy by id."},
			{Method: http.MethodPost, Path: base + "/keys/rotate", Description: "Rotate one downstream CPA key by id."},
			{Method: http.MethodPost, Path: base + "/keys/reset-rpm", Description: "Reset one downstream CPA key RPM counter by id."},
			{Method: http.MethodGet, Path: base + "/keys/usage", Description: "Per-alias usage breakdown for one downstream CPA key by id."},
			{Method: http.MethodGet, Path: base + "/status", Description: "Show cpa-key-policy runtime status."},
			{Method: http.MethodGet, Path: base + "/settings", Description: "Show scheduler settings."},
			{Method: http.MethodPatch, Path: base + "/settings", Description: "Update scheduler settings."},
			{Method: http.MethodGet, Path: base + "/aliases", Description: "List the global alias mapping table."},
			{Method: http.MethodPost, Path: base + "/aliases", Description: "Create or update a global alias mapping."},
			{Method: http.MethodDelete, Path: base + "/aliases", Description: "Delete a global alias mapping by name."},
			{Method: http.MethodGet, Path: base + "/classify-rules", Description: "List credential classification rules."},
			{Method: http.MethodPost, Path: base + "/classify-rules", Description: "Create or update a classification rule."},
			{Method: http.MethodDelete, Path: base + "/classify-rules", Description: "Delete a classification rule by name."},
			{Method: http.MethodPost, Path: base + "/classify-rules/reorder", Description: "Reorder classification rules."},
			{Method: http.MethodPost, Path: base + "/classify-preview", Description: "Preview credential classification results for given descriptors."},
			{Method: http.MethodPost, Path: base + "/catalog", Description: "Build auth-file model picker catalog with classify + built-in groups."},
		},
		Resources: []ResourceRoute{
			{Path: web.IndexPath, Menu: "Key Policy", Description: "Web UI for managing downstream CPA key policies (create keys, pick models)."},
		},
	}
}

func (a *App) handleManagement(raw []byte) ([]byte, error) {
	var req ManagementRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	path := strings.TrimRight(req.Path, "/")

	// Plugin resource GETs (unauthenticated browser UI) are dispatched through
	// the same management.handle method by CPA's ServeResourceHTTP.
	resourcePrefix := "/v0/resource/plugins/" + PluginID
	if req.Method == http.MethodGet && strings.HasPrefix(path, resourcePrefix) {
		status, headers, body := web.Serve(strings.TrimPrefix(path, resourcePrefix))
		return OKEnvelope(ManagementResponse{StatusCode: status, Headers: headers, Body: body})
	}

	base := "/v0/management/plugins/" + PluginID
	switch {
	case req.Method == http.MethodGet && path == base+"/keys":
		return OKEnvelope(jsonResponse(http.StatusOK, map[string]any{"keys": a.publicKeys(a.store.Keys())}))
	case req.Method == http.MethodPost && path == base+"/keys":
		return OKEnvelope(a.createKey(req.Body))
	case req.Method == http.MethodPatch && path == base+"/keys":
		return OKEnvelope(a.patchKey(req.Body))
	case req.Method == http.MethodDelete && path == base+"/keys":
		return OKEnvelope(a.deleteKey(idFromRequest(req.Query, req.Body)))
	case req.Method == http.MethodPost && path == base+"/keys/rotate":
		return OKEnvelope(a.rotateKey(idFromRequest(req.Query, req.Body)))
	case req.Method == http.MethodPost && path == base+"/keys/reset-rpm":
		return OKEnvelope(a.resetRPM(idFromRequest(req.Query, req.Body)))
	case req.Method == http.MethodGet && path == base+"/keys/usage":
		return OKEnvelope(a.keyUsage(idFromRequest(req.Query, req.Body)))
	case req.Method == http.MethodGet && path == base+"/status":
		return OKEnvelope(jsonResponse(http.StatusOK, a.store.Status()))
	case req.Method == http.MethodGet && path == base+"/settings":
		return OKEnvelope(a.schedulerSettings())
	case req.Method == http.MethodPatch && path == base+"/settings":
		return OKEnvelope(a.updateSchedulerSettings(req.Body))
	case req.Method == http.MethodGet && path == base+"/aliases":
		return OKEnvelope(jsonResponse(http.StatusOK, map[string]any{"aliases": a.store.AliasesSnapshot()}))
	case req.Method == http.MethodPost && path == base+"/aliases":
		return OKEnvelope(a.upsertAlias(req.Body))
	case req.Method == http.MethodDelete && path == base+"/aliases":
		return OKEnvelope(a.deleteAlias(req.Body))
	case req.Method == http.MethodGet && path == base+"/classify-rules":
		return OKEnvelope(jsonResponse(http.StatusOK, map[string]any{"rules": a.store.ClassifyRulesSnapshot()}))
	case req.Method == http.MethodPost && path == base+"/classify-rules":
		return OKEnvelope(a.upsertClassifyRule(req.Body))
	case req.Method == http.MethodDelete && path == base+"/classify-rules":
		return OKEnvelope(a.deleteClassifyRule(req.Body))
	case req.Method == http.MethodPost && path == base+"/classify-rules/reorder":
		return OKEnvelope(a.reorderClassifyRules(req.Body))
	case req.Method == http.MethodPost && path == base+"/classify-preview":
		return OKEnvelope(a.classifyPreview(req.Body))
	case req.Method == http.MethodPost && path == base+"/catalog":
		return OKEnvelope(a.buildCatalog(req.Body))
	default:
		return OKEnvelope(jsonError(http.StatusNotFound, "not_found", "unknown management route"))
	}
}

type schedulerSettingsRequest struct {
	GlobalWeightedRoundRobin *bool `json:"global_weighted_round_robin"`
}

func (a *App) schedulerSettings() ManagementResponse {
	return jsonResponse(http.StatusOK, map[string]any{
		"global_weighted_round_robin": a.store.GlobalWeightedRoundRobin(),
	})
}

func (a *App) updateSchedulerSettings(body []byte) ManagementResponse {
	var request schedulerSettingsRequest
	if err := json.Unmarshal(body, &request); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	if request.GlobalWeightedRoundRobin == nil {
		return jsonError(http.StatusBadRequest, "missing_setting", "缺少 global_weighted_round_robin 设置")
	}
	if err := a.store.SetGlobalWeightedRoundRobin(*request.GlobalWeightedRoundRobin); err != nil {
		return jsonError(http.StatusInternalServerError, "settings_persist_failed", "保存调度设置失败: "+err.Error())
	}
	a.clearSchedulerState()
	return a.schedulerSettings()
}

type keyWriteRequest struct {
	ID                  string               `json:"id"`
	Name                *string              `json:"name,omitempty"`
	Enabled             *bool                `json:"enabled,omitempty"`
	Native              *bool                `json:"native,omitempty"`
	Key                 string               `json:"key,omitempty"`
	AccountBinding      *policy.AccountBinding `json:"account_binding,omitempty"`
	ClearAccountBinding bool                 `json:"clear_account_binding,omitempty"`
	RPM                 *int                 `json:"rpm,omitempty"`
	Models              []policy.ModelRule   `json:"models,omitempty"`
	Aliases             []policy.KeyAliasRef `json:"aliases,omitempty"`
	DailyLimitUSD       *float64             `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD      *float64             `json:"weekly_limit_usd,omitempty"`
	AllowModelsEndpoint *bool                `json:"allow_models_endpoint,omitempty"`
}

type publicKey struct {
	ID                  string               `json:"id"`
	Name                string               `json:"name"`
	Enabled             bool                 `json:"enabled"`
	Native              bool                 `json:"native,omitempty"`
	KeyPreview          string               `json:"key_preview"`
	AccountBinding      *policy.AccountBinding `json:"account_binding,omitempty"`
	RPM                 int                  `json:"rpm"`
	Models              []policy.ModelRule   `json:"models"`
	Aliases             []policy.KeyAliasRef `json:"aliases"`
	DailyLimitUSD       float64              `json:"daily_limit_usd"`
	WeeklyLimitUSD      float64              `json:"weekly_limit_usd"`
	AllowModelsEndpoint bool                 `json:"allow_models_endpoint,omitempty"`
	Usage               policy.UsageSummary  `json:"usage"`
	CreatedAt           string               `json:"created_at,omitempty"`
	UpdatedAt           string               `json:"updated_at,omitempty"`
}

func (a *App) createKey(body []byte) ManagementResponse {
	var req keyWriteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	req.ID = strings.TrimSpace(req.ID)
	if req.ID == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	plain := strings.TrimSpace(req.Key)
	native := req.Native != nil && *req.Native
	generated := false
	var err error
	if plain == "" && native {
		return jsonError(http.StatusBadRequest, "native_key_required", "importing a native CPA key requires key")
	}
	if plain == "" {
		plain, err = policy.GenerateKey()
		if err != nil {
			return jsonError(http.StatusInternalServerError, "key_generation_failed", err.Error())
		}
		generated = true
	}
	hash, err := policy.HashKey(plain)
	if err != nil {
		return jsonError(http.StatusBadRequest, "invalid_key", err.Error())
	}
	enabled := true
	if req.Enabled != nil {
		enabled = *req.Enabled
	}
	rpm := 0
	if req.RPM != nil {
		rpm = *req.RPM
	}
	name := req.ID
	if req.Name != nil && strings.TrimSpace(*req.Name) != "" {
		name = strings.TrimSpace(*req.Name)
	}
	item := policy.KeyConfig{
		ID:                  req.ID,
		Name:                name,
		Enabled:             enabled,
		Native:              native,
		KeyHash:             hash,
		KeyPreview:          policy.PreviewKey(plain),
		CallerScope:         policy.CallerScopeForKey(req.ID),
		AccountBinding:      req.AccountBinding,
		RPM:                 rpm,
		Models:              req.Models,
		Aliases:             req.Aliases,
		DailyLimitUSD:       applyFloat64(req.DailyLimitUSD, 0),
		WeeklyLimitUSD:      applyFloat64(req.WeeklyLimitUSD, 0),
		AllowModelsEndpoint: applyBool(req.AllowModelsEndpoint, false),
	}
	if native {
		item.CallerScope = policy.CallerScopeForKey(plain)
		item.KeyPreview = "native"
	}
	if err := a.store.UpsertKey(item, true); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_policy", err.Error())
	}
	saved, _ := a.keyConfigByID(item.ID)
	bodyMap := map[string]any{
		"key":       a.publicKeyFromConfig(saved),
		"generated": generated,
	}
	if !native {
		bodyMap["plain_key"] = plain
	}
	return jsonResponse(http.StatusCreated, bodyMap)
}

func (a *App) patchKey(body []byte) ManagementResponse {
	var req keyWriteRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_json", err.Error())
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	keys := a.store.Keys()
	var current *policy.KeyConfig
	for i := range keys {
		if keys[i].ID == id {
			copy := keys[i]
			current = &copy
			break
		}
	}
	if current == nil {
		return jsonError(http.StatusNotFound, "not_found", "key not found")
	}
	if req.Name != nil {
		current.Name = strings.TrimSpace(*req.Name)
	}
	if req.Enabled != nil {
		current.Enabled = *req.Enabled
	}
	if req.Native != nil && *req.Native != current.Native {
		return jsonError(http.StatusBadRequest, "native_immutable", "native cannot be changed after key creation")
	}
	if req.RPM != nil {
		current.RPM = *req.RPM
	}
	if req.DailyLimitUSD != nil {
		current.DailyLimitUSD = *req.DailyLimitUSD
	}
	if req.WeeklyLimitUSD != nil {
		current.WeeklyLimitUSD = *req.WeeklyLimitUSD
	}
	if req.AllowModelsEndpoint != nil {
		current.AllowModelsEndpoint = *req.AllowModelsEndpoint
	}
	if req.Models != nil {
		current.Models = req.Models
	}
	if req.Aliases != nil {
		current.Aliases = req.Aliases
	}
	if req.ClearAccountBinding {
		current.AccountBinding = nil
	} else if req.AccountBinding != nil {
		current.AccountBinding = req.AccountBinding
	}
	if strings.TrimSpace(req.Key) != "" {
		hash, err := policy.HashKey(req.Key)
		if err != nil {
			return jsonError(http.StatusBadRequest, "invalid_key", err.Error())
		}
		current.KeyHash = hash
		current.KeyPreview = policy.PreviewKey(req.Key)
		if current.Native {
			current.CallerScope = policy.CallerScopeForKey(req.Key)
			current.KeyPreview = "native"
		}
	}
	if err := a.store.UpsertKey(*current, true); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_policy", err.Error())
	}
	saved, _ := a.keyConfigByID(current.ID)
	return jsonResponse(http.StatusOK, map[string]any{"key": a.publicKeyFromConfig(saved)})
}

func (a *App) keyConfigByID(id string) (policy.KeyConfig, bool) {
	for _, key := range a.store.Keys() {
		if key.ID == id {
			return key, true
		}
	}
	return policy.KeyConfig{}, false
}

func (a *App) deleteKey(id string) ManagementResponse {
	if err := a.store.DeleteKey(id); err != nil {
		return storeError(err)
	}
	return jsonResponse(http.StatusOK, map[string]any{"deleted": true, "id": strings.TrimSpace(id)})
}

func (a *App) rotateKey(id string) ManagementResponse {
	plain, item, err := a.store.RotateKey(id)
	if err != nil {
		return storeError(err)
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"key":       a.publicKeyFromConfig(item),
		"plain_key": plain,
		"generated": true,
	})
}

func (a *App) resetRPM(id string) ManagementResponse {
	if err := a.store.ResetRPM(id); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_request", err.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"reset": true, "id": strings.TrimSpace(id)})
}

// keyUsage returns the per-alias usage breakdown for one downstream key (the
// key detail subpage data source). id is taken from the query string (or body),
// matching the rotate/reset-rpm/delete convention.
func (a *App) keyUsage(id string) ManagementResponse {
	id = strings.TrimSpace(id)
	if id == "" {
		return jsonError(http.StatusBadRequest, "missing_id", "id is required")
	}
	key, aliases, ok := a.store.AliasUsageFor(id)
	if !ok {
		return jsonError(http.StatusNotFound, "not_found", "key not found")
	}
	return jsonResponse(http.StatusOK, map[string]any{
		"key_id":           key.ID,
		"key_name":         key.Name,
		"daily_limit_usd":  key.DailyLimitUSD,
		"weekly_limit_usd": key.WeeklyLimitUSD,
		"aliases":          aliases,
	})
}

func storeError(err error) ManagementResponse {
	if errors.Is(err, policy.ErrUnknownKey) {
		return jsonError(http.StatusNotFound, "not_found", "key not found")
	}
	return jsonError(http.StatusBadRequest, "invalid_request", err.Error())
}

func idFromRequest(query map[string][]string, body []byte) string {
	if query != nil {
		for _, name := range []string{"id", "key_id"} {
			if values := query[name]; len(values) > 0 && strings.TrimSpace(values[0]) != "" {
				return strings.TrimSpace(values[0])
			}
		}
	}
	var payload struct {
		ID    string `json:"id"`
		KeyID string `json:"key_id"`
	}
	if len(body) > 0 && json.Unmarshal(body, &payload) == nil {
		if strings.TrimSpace(payload.ID) != "" {
			return strings.TrimSpace(payload.ID)
		}
		return strings.TrimSpace(payload.KeyID)
	}
	return ""
}

func (a *App) publicKeys(keys []policy.KeyConfig) []publicKey {
	out := make([]publicKey, 0, len(keys))
	for _, key := range keys {
		out = append(out, a.publicKeyFromConfig(key))
	}
	return out
}

func (a *App) publicKeyFromConfig(key policy.KeyConfig) publicKey {
	var binding *policy.AccountBinding
	if key.AccountBinding != nil {
		copy := *key.AccountBinding
		copy.Allow = append([]string(nil), key.AccountBinding.Allow...)
		binding = &copy
	}
	out := publicKey{
		ID:         key.ID,
		Name:       key.Name,
		Enabled:    key.Enabled,
		Native:     key.Native,
		KeyPreview: key.KeyPreview,
		AccountBinding: binding,
		RPM:        key.RPM,
		// Ensure models/aliases always serialize as [] (never null). A nil slice
		// would marshal to JSON null, which the UI accesses as .length and
		// crashes on. Models is derived (resolved from Aliases × global table);
		// Aliases is the canonical source.
		Models:              append([]policy.ModelRule{}, key.Models...),
		Aliases:             append([]policy.KeyAliasRef{}, key.Aliases...),
		DailyLimitUSD:       key.DailyLimitUSD,
		WeeklyLimitUSD:      key.WeeklyLimitUSD,
		AllowModelsEndpoint: key.AllowModelsEndpoint,
		Usage:               a.store.UsageSummaryFor(key),
	}
	if !key.CreatedAt.IsZero() {
		out.CreatedAt = key.CreatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	if !key.UpdatedAt.IsZero() {
		out.UpdatedAt = key.UpdatedAt.UTC().Format("2006-01-02T15:04:05Z07:00")
	}
	return out
}

func applyFloat64(v *float64, def float64) float64 {
	if v == nil {
		return def
	}
	return *v
}

func applyBool(v *bool, def bool) bool {
	if v == nil {
		return def
	}
	return *v
}

func jsonResponse(status int, payload any) ManagementResponse {
	body, err := json.Marshal(payload)
	if err != nil {
		return jsonError(http.StatusInternalServerError, "json_error", err.Error())
	}
	return ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func jsonError(status int, code, message string) ManagementResponse {
	if status <= 0 {
		status = http.StatusInternalServerError
	}
	body, _ := json.Marshal(map[string]any{
		"error": map[string]string{
			"code":    strings.TrimSpace(code),
			"message": strings.TrimSpace(message),
		},
	})
	return ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{"application/json; charset=utf-8"}},
		Body:       body,
	}
}

func (a *App) Store() *policy.Store {
	if a == nil {
		return nil
	}
	return a.store
}

func DebugEnvelope(raw []byte) string {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Sprintf("invalid envelope: %v", err)
	}
	if env.Error != nil {
		return env.Error.Message
	}
	return string(env.Result)
}
