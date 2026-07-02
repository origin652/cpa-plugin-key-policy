package plugin

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"cpa-key-policy/internal/plugin/web"
	"cpa-key-policy/internal/policy"
)

type App struct {
	store *policy.Store
}

func NewApp() *App {
	store := policy.NewStore()
	_ = store.Configure(policy.DefaultConfig())
	return &App{store: store}
}

func (a *App) HandleMethod(method string, request []byte) ([]byte, error) {
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
	// (Re)start the periodic usage-ledger flusher for the configured state path.
	a.store.StartUsageFlusher()
	return nil
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
				{Name: "keys", Type: "array", Description: "Initial downstream key policy list. State file wins after it exists."},
			},
		},
		Capabilities: Capabilities{
			FrontendAuthProvider:          true,
			FrontendAuthProviderExclusive: false,
			ModelRouter:                   true,
			Scheduler:                     true,
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
		Target:      rule.Provider,
		TargetModel: rule.TargetModel,
		Reason:      "cpa-key-policy:" + keyID,
	})
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

// pickScheduler implements the scheduler.pick host->plugin call. When the
// routed ModelRule had a Group (codex plan_type / antigravity tier), restrict
// candidate auths to those whose Attributes carry a matching identity. Any
// Group "" or a group we can't recognize → defer to the host scheduler
// (Handled=false), preserving legacy "any auth for the provider" behavior.
//
// The plugin never sees the downstream ModelRule directly here; the group was
// stamped into request metadata by authenticate(), and the host forwards it as
// Options.Metadata["group"]. We read it defensively as either string or any.
//
// Candidate filtering, in order:
//  1. Keep candidates whose Attributes["plan_type"] (codex) equals the group.
//     Also accept Attributes["tier"] (antigravity) to match the same group.
//  2. A group of "supported" means "codex without an id_token plan" — match
//     candidates whose plan_type we cannot read (treat unknown plan as that
//     bucket), so a supported-but-untiered auth file serves them rather than
//     any tiered one.
//
// Among filtered candidates, pick the host's highest-priority one (ties broken
// by lowest ID for determinism). We do not have access to the model-capability
// registry here (it's a separate pluginapi capability), so the host still owns
// the final "is this auth able to serve this model" check via delegate; if a
// chosen candidate can't serve the model the host falls back. This is the same
// trust boundary the built-in scheduler operates under.
func (a *App) pickScheduler(raw []byte) ([]byte, error) {
	var req SchedulerPickRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	group := schedulerGroupFromMetadata(req.Options.Metadata)
	if group == "" {
		// No tier narrowed by this downstream key → let the host pick freely.
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}
	if len(req.Candidates) == 0 {
		return OKEnvelope(SchedulerPickResponse{Handled: false})
	}

	matched := make([]SchedulerAuthCandidate, 0, len(req.Candidates))
	for _, cand := range req.Candidates {
		if schedulerCandidateMatchesGroup(cand, group) {
			matched = append(matched, cand)
		}
	}
	if len(matched) == 0 {
		// No candidate of this tier is available: do not silently degrade to a
		// different tier (that would break the isolation guarantee). Returning
		// Handled=false would let the host pick ANY auth including other tiers.
		// Instead we report an explicit "auth_not_found" so the caller sees the
		// intent honored (no available tier-matching auth) rather than a leak.
		return OKEnvelope(SchedulerPickResponse{
			Handled: true,
			AuthID:  "",
		})
	}

	best := matched[0]
	for _, cand := range matched[1:] {
		if cand.Priority > best.Priority ||
			(cand.Priority == best.Priority && cand.ID < best.ID) {
			best = cand
		}
	}
	return OKEnvelope(SchedulerPickResponse{Handled: true, AuthID: best.ID})
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

// schedulerCandidateMatchesGroup reports whether a candidate auth belongs to
// the requested tier. The codex planner sets Attributes["plan_type"] inside the
// host; antigravity uses a tier concept. We treat an empty/absent plan_type as
// the "supported-untiered" bucket, which matches group "supported" only —
// ensuring a downstream key pinned to a real tier never falls onto an untiered
// file (and vice versa).
func schedulerCandidateMatchesGroup(cand SchedulerAuthCandidate, group string) bool {
	if cand.Attributes == nil {
		return group == "supported" || group == "unknown"
	}
	plan := strings.ToLower(strings.TrimSpace(cand.Attributes["plan_type"]))
	tier := strings.ToLower(strings.TrimSpace(cand.Attributes["tier"]))
	switch group {
	case "supported", "unknown":
		// Untiered bucket: matches candidates with no recognizable plan/tier.
		return plan == "" && tier == ""
	default:
		if plan == group {
			return true
		}
		return tier == group
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
	default:
		return OKEnvelope(jsonError(http.StatusNotFound, "not_found", "unknown management route"))
	}
}

type keyWriteRequest struct {
	ID             string             `json:"id"`
	Name           *string            `json:"name,omitempty"`
	Enabled        *bool              `json:"enabled,omitempty"`
	Key            string             `json:"key,omitempty"`
	RPM            *int               `json:"rpm,omitempty"`
	Models         []policy.ModelRule `json:"models,omitempty"`
	DailyLimitUSD  *float64           `json:"daily_limit_usd,omitempty"`
	WeeklyLimitUSD *float64           `json:"weekly_limit_usd,omitempty"`
}

type publicKey struct {
	ID             string              `json:"id"`
	Name           string              `json:"name"`
	Enabled        bool                `json:"enabled"`
	KeyPreview     string              `json:"key_preview"`
	RPM            int                 `json:"rpm"`
	Models         []policy.ModelRule  `json:"models"`
	DailyLimitUSD  float64             `json:"daily_limit_usd"`
	WeeklyLimitUSD float64             `json:"weekly_limit_usd"`
	Usage          policy.UsageSummary `json:"usage"`
	CreatedAt      string              `json:"created_at,omitempty"`
	UpdatedAt      string              `json:"updated_at,omitempty"`
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
	generated := false
	var err error
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
		ID:             req.ID,
		Name:           name,
		Enabled:        enabled,
		KeyHash:        hash,
		KeyPreview:     policy.PreviewKey(plain),
		RPM:            rpm,
		Models:         req.Models,
		DailyLimitUSD:  applyFloat64(req.DailyLimitUSD, 0),
		WeeklyLimitUSD: applyFloat64(req.WeeklyLimitUSD, 0),
	}
	if err := a.store.UpsertKey(item, true); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_policy", err.Error())
	}
	bodyMap := map[string]any{
		"key":       a.publicKeyFromConfig(item),
		"plain_key": plain,
		"generated": generated,
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
	if req.RPM != nil {
		current.RPM = *req.RPM
	}
	if req.DailyLimitUSD != nil {
		current.DailyLimitUSD = *req.DailyLimitUSD
	}
	if req.WeeklyLimitUSD != nil {
		current.WeeklyLimitUSD = *req.WeeklyLimitUSD
	}
	if req.Models != nil {
		current.Models = req.Models
	}
	if strings.TrimSpace(req.Key) != "" {
		hash, err := policy.HashKey(req.Key)
		if err != nil {
			return jsonError(http.StatusBadRequest, "invalid_key", err.Error())
		}
		current.KeyHash = hash
		current.KeyPreview = policy.PreviewKey(req.Key)
	}
	if err := a.store.UpsertKey(*current, true); err != nil {
		return jsonError(http.StatusBadRequest, "invalid_policy", err.Error())
	}
	return jsonResponse(http.StatusOK, map[string]any{"key": a.publicKeyFromConfig(*current)})
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
		"key_id":   key.ID,
		"key_name": key.Name,
		"aliases":  aliases,
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
	out := publicKey{
		ID:         key.ID,
		Name:       key.Name,
		Enabled:    key.Enabled,
		KeyPreview: key.KeyPreview,
		RPM:        key.RPM,
		// Ensure models always serializes as [] (never null). A nil slice would
		// marshal to JSON null, which the UI accesses as .length and crashes on.
		Models:         append([]policy.ModelRule{}, key.Models...),
		DailyLimitUSD:  key.DailyLimitUSD,
		WeeklyLimitUSD: key.WeeklyLimitUSD,
		Usage:          a.store.UsageSummaryFor(key),
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
