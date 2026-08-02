package policy

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type Store struct {
	mu         sync.RWMutex
	updateMu   sync.Mutex
	persistMu  sync.Mutex
	enabled    bool
	statePath  string
	keys       map[string]*KeyConfig
	keysByHash map[string]*KeyConfig
	limiter    *RateLimiter
	usage      *usageLedger
	// flusher for periodically persisting the usage ledger to the state file.
	flusher *usageFlusher
	// aliases is the global alias mapping table from config.yaml. Used to
	// resolve KeyAliasRef → ModelRule for routing and billing.
	aliases map[string]*AliasMapping
	// classifyRules are user-defined credential classification rules.
	classifyRules []ClassifyRule
	// rrCounters tracks round-robin position per alias name (global, shared
	// across all keys). Reset on Configure/UpsertKey when aliases change.
	rrCounters map[string]int
	// pendingPicks remembers the multi-target selection made at Authenticate
	// so Route (and the group stamped into scheduler metadata) use the same
	// target for the same request. Without this, round-robin would advance
	// twice (auth + route) and the scheduler could filter by the wrong group.
	// Keyed by lower(keyID)+"\0"+lower(alias); FIFO queue per key.
	pendingPicks map[string][]pendingPick
	// onClassifyRulesChanged is called when classify rules change, so the
	// plugin can clear its classify cache. Set by the plugin App.
	onClassifyRulesChanged func()
}

// ErrInvalidUsageResetWindow reports an unsupported administrative usage
// reset window.
var ErrInvalidUsageResetWindow = errors.New("invalid usage reset window")

// pendingPick is one Authenticate-time target selection waiting for Route.
type pendingPick struct {
	rule ModelRule
	at   time.Time
}

// pendingPickTTL drops orphaned selections when the host never called Route
// (e.g. rejected after auth). Long enough for normal request setup, short
// enough not to pin stale groups across unrelated traffic.
const pendingPickTTL = 30 * time.Second

// pendingPickMaxQueue caps how many unconsumed picks we keep per (key,alias).
const pendingPickMaxQueue = 32

type AuthDecision struct {
	Known       bool
	Allowed     bool
	KeyID       string
	Principal   string
	Requested   string
	Rule        ModelRule
	Reason      string
	ModelList   bool
	RateLimited bool
	CostLimited bool
	// PreCharged reports that this request was billed at access time because
	// it targets an image/video endpoint whose per_call alias CPA cannot bill
	// via usage.handle (the XAI executor skips UsageReporter on those paths).
	// The charge is unconditional (no failure refund), so this is a deliberate
	// trade-off documented in the UI.
	PreCharged bool
}

func NewStore() *Store {
	return &Store{
		enabled:      DefaultConfig().Enabled,
		keys:         make(map[string]*KeyConfig),
		keysByHash:   make(map[string]*KeyConfig),
		limiter:      NewRateLimiter(),
		usage:        newUsageLedger(time.Now),
		rrCounters:   make(map[string]int),
		pendingPicks: make(map[string][]pendingPick),
	}
}

// SetClock injects a clock for testing (limiter + usage windows).
func (s *Store) SetClock(now func() time.Time) {
	if now == nil {
		return
	}
	s.mu.Lock()
	s.limiter = NewRateLimiterWithClock(now)
	s.usage = newUsageLedger(now)
	s.mu.Unlock()
}

func (s *Store) Configure(cfg Config) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	if err := normalizeConfig(&cfg); err != nil {
		return err
	}
	statePath, err := ResolveStatePath(cfg.StateFile)
	if err != nil {
		return err
	}

	// Bug 2 fix: flush any in-memory changes to the *old* state path BEFORE
	// loading the (possibly different) new state file. Without this, keys/usage
	// changed via the management API in the last <=15s window (or any abnormal
	// path that skipped persist) would be lost when LoadState reads a stale disk
	// snapshot. StopUsageFlusher stops the background loop and flushes once.
	s.StopUsageFlusher()

	keys := cfg.Keys
	var loadedUsage map[string]*UsageState
	firstBoot := false
	if state, errLoad := LoadState(statePath); errLoad == nil {
		keys = state.Keys
		loadedUsage = state.Usage
		// If config.yaml has no global alias table, fall back to the one
		// persisted in state (so state-only reloads resolve key alias refs).
		stateAliases := cfg.Aliases
		if len(stateAliases) == 0 && len(state.Aliases) > 0 {
			stateAliases = state.Aliases
		}
		stateRules := cfg.ClassifyRules
		if len(stateRules) == 0 && len(state.ClassifyRules) > 0 {
			stateRules = state.ClassifyRules
		}
		// Validate state keys against the global alias table. normalizeConfig
		// also auto-migrates any state keys still using per-key Models.
		merged := Config{Enabled: cfg.Enabled, StateFile: cfg.StateFile, Keys: keys, Aliases: stateAliases, ClassifyRules: stateRules}
		if errNorm := normalizeConfig(&merged); errNorm != nil {
			return fmt.Errorf("load state: %w", errNorm)
		}
		keys = merged.Keys
		// Propagate the resolved alias table back to cfg for downstream use.
		cfg.Aliases = merged.Aliases
		cfg.ClassifyRules = merged.ClassifyRules
	} else if !errors.Is(errLoad, os.ErrNotExist) {
		return fmt.Errorf("load state: %w", errLoad)
	} else {
		firstBoot = true
	}

	next := make(map[string]*KeyConfig, len(keys))
	now := time.Now().UTC()
	// Build the global alias lookup from the config (post-migration).
	aliasLookup := make(map[string]*AliasMapping, len(cfg.Aliases))
	for i := range cfg.Aliases {
		aliasLookup[strings.ToLower(cfg.Aliases[i].Alias)] = &cfg.Aliases[i]
	}

	for i := range keys {
		item := keys[i]
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		if item.UpdatedAt.IsZero() {
			item.UpdatedAt = item.CreatedAt
		}
		// If the key has Aliases refs, populate Models from the global table
		// so all downstream code (routing, billing, usage) works
		// unchanged. For round-robin aliases with multiple targets, we expand
		// to one ModelRule per target (the scheduler picks based on group).
		if len(item.Aliases) > 0 {
			item.Models = resolveAliasRefsToModels(item.Aliases, aliasLookup)
		}
		next[item.ID] = &item
	}

	s.mu.Lock()
	// Stop any prior flusher before rebuilding keys/state path. (StopUsageFlusher
	// above already handled the flush-then-stop for the old path; this guards
	// against a flusher that started after this point in a re-entrant call.)
	if s.flusher != nil {
		s.flusher.stop()
		s.flusher = nil
	}
	// The persisted state is authoritative during reconfigure. updateMu
	// serializes this replacement with management mutations, so a revoked or
	// deleted key cannot be resurrected from an older in-memory snapshot.
	s.enabled = cfg.Enabled
	s.statePath = statePath
	// Store the global alias table and classify rules for routing/billing.
	s.aliases = make(map[string]*AliasMapping, len(cfg.Aliases))
	for i := range cfg.Aliases {
		s.aliases[strings.ToLower(cfg.Aliases[i].Alias)] = &cfg.Aliases[i]
	}
	s.classifyRules = cfg.ClassifyRules
	s.keys = next
	s.rebuildKeysByHashLocked()
	s.rrCounters = make(map[string]int)
	s.pendingPicks = make(map[string][]pendingPick)
	if s.limiter == nil {
		s.limiter = NewRateLimiter()
	}
	// Re-load usage into the (clock-bound) ledger for restart recovery. The
	// clock is preserved when set via SetClock; otherwise default time.Now.
	clockNow := s.usage.now
	s.usage = newUsageLedger(clockNow)
	s.usage.loadFromState(loadedUsage)

	// First boot (no state file existed): persist a baseline state so that the
	// periodic usage flush (SaveUsageOnly) has keys to preserve on disk. Without
	// this, the first FlushUsage would LoadState, find nothing, and write a
	// state containing only usage (no keys) — then the next Configure would
	// load an empty key list. Keys come from next (cfg.Keys or disk), usage is
	// freshly loaded (empty on first boot).
	var baseKeys []KeyConfig
	var baseUsage map[string]*UsageState
	var baseAliases []AliasMapping
	var baseRules []ClassifyRule
	if firstBoot {
		baseKeys = s.keysSnapshotLocked()
		baseUsage = s.usageSnapshotLocked()
		baseAliases = s.aliasesSnapshotLocked()
		baseRules = s.classifyRulesSnapshotLocked()
	}
	s.mu.Unlock()
	if firstBoot {
		if errSave := s.saveState(statePath, baseKeys, baseUsage, baseAliases, baseRules); errSave != nil {
			return fmt.Errorf("seed state: %w", errSave)
		}
	}
	return nil
}

func (s *Store) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

func (s *Store) runtimeComponents() (*RateLimiter, *usageLedger) {
	s.mu.RLock()
	limiter := s.limiter
	usage := s.usage
	s.mu.RUnlock()
	return limiter, usage
}

func (s *Store) StatePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statePath
}

func (s *Store) Authenticate(method, path string, headers http.Header, query map[string][]string, body []byte) AuthDecision {
	rawKey := ExtractAPIKey(headers, query)
	key, enabled := s.findBySecretWhenEnabled(rawKey)
	if !enabled {
		return AuthDecision{Known: false, Reason: "plugin_disabled"}
	}
	if key == nil {
		return AuthDecision{Known: false, Reason: "unknown_key"}
	}
	decision := AuthDecision{
		Known:     true,
		KeyID:     key.ID,
		Principal: key.ID,
		ModelList: IsModelsEndpoint(path),
	}
	if !key.Enabled {
		decision.Reason = "key_disabled"
		return decision
	}
	if decision.ModelList {
		// Per-key override: a key with AllowModelsEndpoint=true may reach the
		// global /v1/models list; otherwise it's 401. We still cannot filter the
		// list contents per key (CPA limitation above), only hide/show it.
		if key.AllowModelsEndpoint {
			decision.Allowed = true
			decision.Reason = "models_endpoint_allowed"
			return decision
		}
		decision.Reason = "models_endpoint_disabled"
		return decision
	}
	requested := ExtractRequestedModel(path, query, body)
	decision.Requested = requested
	if requested != "" {
		// Must use the same multi-target selection as Route (priority /
		// round-robin), not ModelForAlias which always returns the first
		// match. Otherwise metadata["group"] can pin the wrong tier while
		// model.route forwards a different target.
		rule, ok := s.resolveRuleForAlias(key, requested)
		if !ok {
			decision.Reason = "model_not_allowed"
			return decision
		}
		decision.Rule = rule
	}
	limiter, usageLedger := s.runtimeComponents()
	if limiter != nil && !limiter.Allow(key.ID, key.RPM) {
		decision.RateLimited = true
		decision.Reason = "rpm_exceeded"
		return decision
	}
	// Dollar usage limit check (daily / weekly). Only enforced when a limit is
	// set (>0). This is a pre-request gate; the request that pushes usage over
	// the limit is allowed through, and the next request is rejected — matching
	// the RPM limiter's "off-by-one" semantics.
	if usageLedger != nil {
		if reason, _ := usageLedger.OverLimit(*key); reason != "" {
			decision.CostLimited = true
			decision.Reason = reason
			return decision
		}
	}
	decision.Allowed = true
	decision.Reason = "allowed"

	// Remember this request's selected target so Route reuses it (same group /
	// provider / model). Only stash when the request is actually allowed — a
	// rate/cost-limited request never reaches model.route.
	if requested != "" {
		s.rememberPick(key.ID, requested, decision.Rule)
	}

	// Per-call image/video pre-charge workaround. CPA's XAI executor does not
	// emit usage records for /v1/images/* and /v1/videos/* (executeImages and
	// executeVideos lack a UsageReporter), so usage.handle never fires and the
	// plugin would never bill these. When the matched rule is per_call and the
	// path is an image/video endpoint, charge now, at access time. This is
	// unconditional (we cannot observe the upstream outcome here), so failed
	// requests are also charged — a known trade-off surfaced in the UI.
	if decision.Rule.BillingMode == "per_call" && IsImageVideoEndpoint(path) {
		alias := decision.Rule.Alias
		if alias == "" {
			alias = decision.Requested
		}
		model := decision.Rule.TargetModel
		if model == "" {
			model = alias
		}
		// failed=false so the per_call branch charges PerCallUSD. This is the
		// intended behavior for this workaround (no refund on upstream failure).
		s.RecordUsage(key.ID, alias, model, false, UsageDetail{})
		decision.PreCharged = true
	}

	return decision
}

func (s *Store) Route(headers http.Header, query map[string][]string, requested string) (ModelRule, string, bool) {
	if !s.Enabled() {
		return ModelRule{}, "", false
	}
	key := s.findBySecret(ExtractAPIKey(headers, query))
	if key == nil || !key.Enabled {
		return ModelRule{}, "", false
	}
	// Prefer the selection Authenticate already made for this request so the
	// routed provider/model and the group stamped into scheduler metadata stay
	// aligned (critical for multi-target aliases with different groups).
	if rule, ok := s.takePick(key.ID, requested); ok {
		return rule, key.ID, true
	}
	rule, ok := s.resolveRuleForAlias(key, requested)
	if !ok {
		return ModelRule{}, key.ID, false
	}
	return rule, key.ID, true
}

// resolveRuleForAlias selects the ModelRule for the requested alias, applying
// the global alias's dispatch mode. For "round-robin" with multiple targets,
// it rotates through the targets using a global counter (shared across all
// keys). For "priority", it always returns the first target.
func (s *Store) resolveRuleForAlias(key *KeyConfig, requested string) (ModelRule, bool) {
	// Collect all ModelRules matching the alias (a multi-target alias expands
	// to multiple rules with the same alias but different provider/model/group).
	var matches []ModelRule
	for _, rule := range key.Models {
		if strings.EqualFold(rule.Alias, requested) {
			matches = append(matches, rule)
		}
	}
	if len(matches) == 0 {
		return ModelRule{}, false
	}
	if len(matches) == 1 {
		return matches[0], true
	}
	// Multiple targets: dispatch mode + RR counter need the store lock.
	s.mu.Lock()
	defer s.mu.Unlock()
	aliasName := strings.ToLower(strings.TrimSpace(requested))
	alias := s.aliases[aliasName]
	if alias != nil && strings.EqualFold(alias.Dispatch, "priority") {
		return matches[0], true // static priority: always first
	}
	// Round-robin (default): rotate using a global counter.
	idx := s.rrCounters[aliasName]
	if idx >= len(matches) {
		idx = 0
	}
	s.rrCounters[aliasName] = (idx + 1) % len(matches)
	return matches[idx], true
}

func pendingPickKey(keyID, alias string) string {
	return strings.ToLower(strings.TrimSpace(keyID)) + "\x00" + strings.ToLower(strings.TrimSpace(alias))
}

func (s *Store) clearPendingPicksForKeyLocked(keyID string) {
	prefix := strings.ToLower(strings.TrimSpace(keyID)) + "\x00"
	if prefix == "\x00" {
		return
	}
	for key := range s.pendingPicks {
		if strings.HasPrefix(key, prefix) {
			delete(s.pendingPicks, key)
		}
	}
}

// rememberPick stores Authenticate's selected rule for a later Route call.
func (s *Store) rememberPick(keyID, alias string, rule ModelRule) {
	if strings.TrimSpace(keyID) == "" || strings.TrimSpace(alias) == "" {
		return
	}
	k := pendingPickKey(keyID, alias)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pendingPicks == nil {
		s.pendingPicks = make(map[string][]pendingPick)
	}
	q := s.prunePendingLocked(s.pendingPicks[k], now)
	q = append(q, pendingPick{rule: rule, at: now})
	if len(q) > pendingPickMaxQueue {
		q = q[len(q)-pendingPickMaxQueue:]
	}
	s.pendingPicks[k] = q
}

// takePick consumes the oldest non-expired selection for this key+alias.
// Returns false when nothing is pending (Route-only callers / tests).
func (s *Store) takePick(keyID, alias string) (ModelRule, bool) {
	if strings.TrimSpace(keyID) == "" || strings.TrimSpace(alias) == "" {
		return ModelRule{}, false
	}
	k := pendingPickKey(keyID, alias)
	now := time.Now()
	s.mu.Lock()
	defer s.mu.Unlock()
	q := s.prunePendingLocked(s.pendingPicks[k], now)
	if len(q) == 0 {
		delete(s.pendingPicks, k)
		return ModelRule{}, false
	}
	pick := q[0]
	q = q[1:]
	if len(q) == 0 {
		delete(s.pendingPicks, k)
	} else {
		s.pendingPicks[k] = q
	}
	return pick.rule, true
}

// prunePendingLocked drops expired entries. Caller must hold s.mu.
func (s *Store) prunePendingLocked(q []pendingPick, now time.Time) []pendingPick {
	if len(q) == 0 {
		return q
	}
	i := 0
	for i < len(q) && now.Sub(q[i].at) > pendingPickTTL {
		i++
	}
	if i == 0 {
		return q
	}
	if i >= len(q) {
		return nil
	}
	return q[i:]
}

func (s *Store) ResponseAlias(headers http.Header, query map[string][]string, requested string) (string, bool) {
	rule, _, ok := s.Route(headers, query, requested)
	if !ok {
		return "", false
	}
	return rule.Alias, true
}

// RecordResponseCost bills a non-streaming response for the key that owns the
// requested alias. It parses the usage tokens from the response body, looks up
// the alias's configured per-million prices, records the dollar cost, and
// returns it. Streaming responses or unparseable bodies cost nothing.
// This is best-effort: parse failures are silently zero-cost (never panic a
// response path).
func (s *Store) RecordResponseCost(headers http.Header, query map[string][]string, requested string, body []byte) float64 {
	if !s.Enabled() {
		return 0
	}
	key := s.findBySecret(ExtractAPIKey(headers, query))
	if key == nil || !key.Enabled {
		return 0
	}
	_, usageLedger := s.runtimeComponents()
	requestedAlias := strings.TrimSpace(requested)
	if requestedAlias == "" {
		return 0
	}
	// Resolve to the configured canonical alias spelling so pure case variants
	// share one ByAlias bucket. Unknown aliases keep zero-cost semantics and
	// must not create forged empty ledger rows.
	rule, ok := key.ModelForAlias(requestedAlias)
	if !ok {
		return 0
	}
	alias := rule.Alias
	usage := ParseTokenUsage(body)
	if !usage.Found {
		return 0
	}
	inputPerMillion, outputPerMillion, _, priced := key.PriceForAlias(alias)
	cost := ComputeCost(inputPerMillion, outputPerMillion, priced, usage)
	if priced && usage.Found && usageLedger != nil {
		// Record even when cost == 0 (a priced-but-free alias: input/output/cache
		// prices all configured as 0). Token / call counters must still advance so
		// the UI can report usage volume and hit-rate; the USD just stays 0.
		// Previously `cost > 0` dropped free-but-priced requests entirely.
		// The response-body path sees only prompt/completion counts (no cache
		// breakdown), so cache counters stay 0 here; cache-aware accounting
		// happens in RecordUsage via ComputeCacheCostBreakdown. We still record
		// input tokens for hit-rate denominator parity (treat all prompt tokens
		// as non-cache input on this path, since we can't tell otherwise).
		// callCount=1: this was a successful, token-billed request.
		usageLedger.RecordCost(key.ID, alias, cost, 0, 0, int64(usage.PromptTokens), int64(usage.CompletionTokens), 1)
	}
	return cost
}

// RecordUsage bills a finalized usage record delivered by the host via the
// usage.handle plugin call. CPA parses the token counts itself (including the
// final usage frame of a streaming response) before invoking us, so we receive
// ready-made Input/Output token counts rather than a body to parse. This is
// the billing entry point that covers streaming responses — the host never
// invokes response.intercept_after on the streaming path, so RecordResponseCost
// alone cannot bill streams. Best-effort: unknown keys or aliases cost nothing.
//
// failed reports whether the upstream request failed (non-2xx). Per-call
// billing only charges on success (failed=false); token billing is implicitly
// zero on failure (no tokens reported). Failed requests never increment
// CallCount.
//
// key resolution: the host's UsageRecord.APIKey is NOT the client's plaintext
// secret — CPA stores our auth result's Principal (set to key.ID) into the
// request context as "userApiKey" and forwards that. So we match by key.ID
// first, then fall back to a plaintext-secret match for forward compatibility
// (in case a future CPA build forwards the raw secret).
//
// alias resolution: prefer the client-requested Alias (what the caller put in
// the request body's "model" field); fall back to the resolved upstream Model.
func (s *Store) RecordUsage(apiKeyOrID, alias, model string, failed bool, detail UsageDetail) float64 {
	if !s.Enabled() {
		return 0
	}
	// Match by ID first (the documented wire value), then by plaintext secret.
	key := s.findByID(apiKeyOrID)
	if key == nil || !key.Enabled {
		key = s.findBySecret(apiKeyOrID)
	}
	if key == nil || !key.Enabled {
		return 0
	}
	_, usageLedger := s.runtimeComponents()
	// Resolve the alias to price against. Prefer the client-requested alias
	// (matches what the user configured prices for); fall back to the upstream
	// model id, which equals the alias for this plugin (alias == target_model).
	resolved := strings.TrimSpace(alias)
	if resolved == "" {
		resolved = strings.TrimSpace(model)
	}
	if resolved == "" {
		return 0
	}
	// Canonicalize to the configured rule.Alias spelling so pure case variants
	// share one ByAlias bucket. Unknown aliases keep existing zero-cost /
	// reject-at-auth semantics and must not create forged empty ledger rows.
	rule, ok := key.ModelForAlias(resolved)
	if !ok {
		return 0
	}
	resolved = rule.Alias

	// Per-call billing: a fixed USD charge per SUCCESSFUL request, independent
	// of token counts. Failed requests are not charged and don't count. A
	// PerCallUSD of 0 is allowed (free calls); CallCount still increments so the
	// UI can report call volume. The token-price fields on the rule are dormant
	// under this mode.
	if strings.EqualFold(rule.BillingMode, "per_call") {
		if failed {
			return 0
		}
		cost := rule.PerCallUSD
		if cost < 0 {
			cost = 0
		}
		if usageLedger != nil {
			// callCount=1 regardless of cost (even free calls count toward volume).
			usageLedger.RecordCost(key.ID, resolved, cost, 0, 0, 0, 0, 1)
		}
		return cost
	}

	usage := TokenUsage{
		PromptTokens:     int(detail.InputTokens),
		CompletionTokens: int(detail.OutputTokens),
		Found:            detail.InputTokens > 0 || detail.OutputTokens > 0,
	}
	if !usage.Found {
		return 0
	}
	// Cache-aware billing: the usage.handle detail carries cache-read / cached
	// token counts. We price cache-hit input tokens at the alias's cache-read
	// price (falling back to the input price when none is configured), with
	// provider-specific semantics for whether cache hits sit inside or outside
	// InputTokens. The owning rule's provider selects the semantics.
	provider := rule.Provider
	inputPerMillion, outputPerMillion, cacheReadPerMillion, priced := key.PriceForAlias(resolved)
	cost, cacheCost, cacheReadTokens := ComputeCacheCostBreakdown(provider, inputPerMillion, outputPerMillion, cacheReadPerMillion, priced, detail)
	// Non-cache input tokens billed at the input price — the denominator partner
	// for hit-rate = cacheRead / (cacheRead + input). Must mirror the biller's
	// internal split so the reported rate matches the actual pricing.
	var nonCacheInput int64
	if priced && (detail.InputTokens > 0 || detail.OutputTokens > 0) {
		if isCacheAdditiveProvider(provider) {
			nonCacheInput = detail.InputTokens + detail.CacheCreationTokens
		} else {
			cr := detail.CacheReadTokens
			if cr == 0 {
				cr = detail.CachedTokens
			}
			if cr > detail.InputTokens {
				cr = detail.InputTokens
			}
			nonCacheInput = detail.InputTokens - cr
		}
	}
	if priced && usage.Found && usageLedger != nil {
		// Record even when cost == 0 (priced-but-free alias: all token prices 0).
		// Token (input/output/cache) + call counters must advance so the UI
		// reports usage volume and hit-rate; USD stays 0. Previously `cost > 0`
		// dropped free-but-priced requests entirely, hiding their volume.
		// callCount=1: this was a successful, token-billed request.
		usageLedger.RecordCost(key.ID, resolved, cost, cacheCost, cacheReadTokens, nonCacheInput, int64(detail.OutputTokens), 1)
	}
	return cost
}

// UsageSummaryFor returns the current daily/weekly usage + limits for a key
// (for the keys-list management API).
func (s *Store) UsageSummaryFor(key KeyConfig) UsageSummary {
	_, usage := s.runtimeComponents()
	if usage == nil {
		return UsageSummary{DailyLimitUSD: key.DailyLimitUSD, WeeklyLimitUSD: key.WeeklyLimitUSD}
	}
	return usage.Summary(key)
}

// ResetUsage clears in-memory usage for a key (manual quota unlock).
func (s *Store) ResetUsage(id string) {
	_, usage := s.runtimeComponents()
	if usage != nil {
		usage.resetUsage(id)
	}
}

// AliasUsageFor returns a per-alias usage breakdown for the key with the given
// id, for the key detail management API. Returns the key config, the alias
// rows, and whether the key was found. Configured-but-unused aliases appear
// with zero values; ledger residuals for aliases no longer in the key's config
// appear with InConfig=false. Rows are sorted by alias.
func (s *Store) AliasUsageFor(keyID string) (KeyConfig, []AliasUsageEntry, bool) {
	key := s.findByID(keyID)
	if key == nil {
		return KeyConfig{}, nil, false
	}
	_, usage := s.runtimeComponents()
	if usage == nil {
		rows := make([]AliasUsageEntry, 0, len(key.Models))
		for _, r := range key.Models {
			rows = append(rows, AliasUsageEntry{
				Alias:       r.Alias,
				Provider:    r.Provider,
				TargetModel: r.TargetModel,
				BillingMode: r.BillingMode,
				PerCallUSD:  r.PerCallUSD,
				InConfig:    true,
			})
		}
		return *key, rows, true
	}
	return *key, usage.AliasUsage(*key), true
}

// FindByAPIKey resolves a downstream plain key to policy (copy). Returns nil when unknown.
func (s *Store) FindByAPIKey(raw string) *KeyConfig {
	return s.findBySecret(raw)
}

func (s *Store) findBySecret(raw string) *KeyConfig {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	hash, err := HashKey(raw)
	if err != nil {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := s.keysByHash[strings.ToLower(strings.TrimSpace(hash))]
	if key == nil {
		return nil
	}
	copy := *key
	copy.Models = append([]ModelRule(nil), key.Models...)
	copy.Aliases = append([]KeyAliasRef(nil), key.Aliases...)
	return &copy
}

func (s *Store) findBySecretWhenEnabled(raw string) (*KeyConfig, bool) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		s.mu.RLock()
		enabled := s.enabled
		s.mu.RUnlock()
		return nil, enabled
	}
	hash, err := HashKey(raw)
	if err != nil {
		return nil, true
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if !s.enabled {
		return nil, false
	}
	key := s.keysByHash[strings.ToLower(strings.TrimSpace(hash))]
	if key == nil {
		return nil, true
	}
	copy := *key
	copy.Models = append([]ModelRule(nil), key.Models...)
	copy.Aliases = append([]KeyAliasRef(nil), key.Aliases...)
	return &copy, true
}

// findByID resolves a key config by its ID. The host's usage.handle call does
// NOT carry the client's plaintext key — CPA stores the plugin auth result's
// Principal (which THIS plugin sets to key.ID at store.go Authenticate) into
// the request context as "userApiKey", then forwards that as the UsageRecord's
// APIKey field. So the value we receive in usage.handle is key.ID, not the
// secret. Matching must therefore be ID-based, not hash-based.
func (s *Store) findByID(id string) *KeyConfig {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	key := s.keys[id]
	if key == nil {
		for candidateID, candidate := range s.keys {
			if strings.EqualFold(candidateID, id) {
				key = candidate
				break
			}
		}
	}
	if key == nil {
		return nil
	}
	copy := *key
	copy.Models = append([]ModelRule(nil), key.Models...)
	copy.Aliases = append([]KeyAliasRef(nil), key.Aliases...)
	return &copy
}

func (s *Store) rebuildKeysByHashLocked() {
	ids := make([]string, 0, len(s.keys))
	for id := range s.keys {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	byHash := make(map[string]*KeyConfig, len(ids))
	for _, id := range ids {
		key := s.keys[id]
		if key == nil {
			continue
		}
		hash := strings.ToLower(strings.TrimSpace(key.KeyHash))
		if hash == "" {
			continue
		}
		if _, exists := byHash[hash]; !exists {
			byHash[hash] = key
		}
	}
	s.keysByHash = byHash
}

func (k *KeyConfig) ModelForAlias(alias string) (ModelRule, bool) {
	alias = strings.TrimSpace(alias)
	if alias == "" {
		return ModelRule{}, false
	}
	for _, rule := range k.Models {
		if strings.EqualFold(rule.Alias, alias) {
			return rule, true
		}
	}
	return ModelRule{}, false
}

// resolveAliasRefsToModels expands a key's Alias refs into concrete ModelRule
// entries using the global alias table. For round-robin aliases with multiple
// targets, each target becomes a separate ModelRule (same alias, different
// provider/model/group) — the routing layer selects one at request time. Per-key
// price overrides on KeyAliasRef take precedence over the global alias pricing.
func resolveAliasRefsToModels(refs []KeyAliasRef, aliases map[string]*AliasMapping) []ModelRule {
	var out []ModelRule
	for _, ref := range refs {
		a, ok := aliases[strings.ToLower(ref.Alias)]
		if !ok {
			continue
		}
		for _, t := range a.Targets {
			rule := ModelRule{
				Alias:       a.Alias,
				Provider:    t.Provider,
				TargetModel: t.TargetModel,
				Group:       t.Group,
				BillingMode: a.BillingMode,
			}
			// Apply per-key price overrides (nil = use global default).
			if ref.InputPricePerMillion != nil {
				rule.InputPricePerMillion = *ref.InputPricePerMillion
			} else {
				rule.InputPricePerMillion = a.InputPricePerMillion
			}
			if ref.OutputPricePerMillion != nil {
				rule.OutputPricePerMillion = *ref.OutputPricePerMillion
			} else {
				rule.OutputPricePerMillion = a.OutputPricePerMillion
			}
			if ref.CacheReadPricePerMillion != nil {
				rule.CacheReadPricePerMillion = *ref.CacheReadPricePerMillion
			} else {
				rule.CacheReadPricePerMillion = a.CacheReadPricePerMillion
			}
			if ref.PerCallUSD != nil {
				rule.PerCallUSD = *ref.PerCallUSD
			} else {
				rule.PerCallUSD = a.PerCallUSD
			}
			out = append(out, rule)
		}
	}
	return out
}

func (s *Store) Keys() []KeyConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.keysSnapshotLocked()
}

func (s *Store) keysSnapshotLocked() []KeyConfig {
	keys := make([]KeyConfig, 0, len(s.keys))
	for _, key := range s.keys {
		copy := *key
		copy.Models = append([]ModelRule(nil), key.Models...)
		copy.Aliases = append([]KeyAliasRef(nil), key.Aliases...)
		keys = append(keys, copy)
	}
	// Bug 5 fix: stable order by ID so list APIs and frontend rendering are
	// deterministic (Go map iteration is randomized). Doesn't affect which key
	// a button targets (bound by id), but prevents rows from jumping around.
	sort.Slice(keys, func(i, j int) bool { return keys[i].ID < keys[j].ID })
	return keys
}

// aliasesSnapshotLocked returns a copy of the global alias table.
// Caller must hold s.mu.
func (s *Store) aliasesSnapshotLocked() []AliasMapping {
	out := make([]AliasMapping, 0, len(s.aliases))
	for _, a := range s.aliases {
		copy := *a
		copy.Targets = append([]AliasTarget(nil), a.Targets...)
		out = append(out, copy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Alias < out[j].Alias })
	return out
}

// AliasesSnapshot returns a copy of the global alias table (thread-safe).
func (s *Store) AliasesSnapshot() []AliasMapping {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.aliasesSnapshotLocked()
}

// classifyRulesSnapshotLocked returns a copy of the classify rules.
// Caller must hold s.mu.
func (s *Store) classifyRulesSnapshotLocked() []ClassifyRule {
	return append([]ClassifyRule(nil), s.classifyRules...)
}

// ClassifyRulesSnapshot returns a copy of the classify rules (thread-safe).
func (s *Store) ClassifyRulesSnapshot() []ClassifyRule {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.classifyRulesSnapshotLocked()
}

// updateAliasesLocked replaces the store's global alias table.
func (s *Store) updateAliasesLocked(aliases []AliasMapping) {
	s.aliases = make(map[string]*AliasMapping, len(aliases))
	for i := range aliases {
		s.aliases[strings.ToLower(aliases[i].Alias)] = &aliases[i]
	}
}

func (s *Store) UpsertKey(input KeyConfig, persist bool) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	// Build a config that includes the store's current global alias table
	// and classify rules, so normalizeConfig can validate the key's alias
	// references and migrate any per-key Models into existing or new aliases.
	s.mu.RLock()
	existingAliases := s.aliasesSnapshotLocked()
	existingRules := s.classifyRulesSnapshotLocked()
	s.mu.RUnlock()
	cfg := Config{Enabled: true, StateFile: s.StatePath(), Keys: []KeyConfig{input}, Aliases: existingAliases, ClassifyRules: existingRules}
	if err := normalizeConfig(&cfg); err != nil {
		return err
	}
	key := cfg.Keys[0]
	now := time.Now().UTC()
	s.mu.Lock()
	if old := s.keys[key.ID]; old != nil && !old.CreatedAt.IsZero() {
		key.CreatedAt = old.CreatedAt
	} else if key.CreatedAt.IsZero() {
		key.CreatedAt = now
	}
	key.UpdatedAt = now
	// Populate Models from the key's Alias refs + global table for downstream use.
	if len(key.Aliases) > 0 {
		aliasLookup := make(map[string]*AliasMapping, len(cfg.Aliases))
		for i := range cfg.Aliases {
			aliasLookup[strings.ToLower(cfg.Aliases[i].Alias)] = &cfg.Aliases[i]
		}
		key.Models = resolveAliasRefsToModels(key.Aliases, aliasLookup)
	}
	s.keys[key.ID] = &key
	s.rebuildKeysByHashLocked()
	s.clearPendingPicksForKeyLocked(key.ID)
	// Update the store's global alias table if migration added new aliases.
	s.updateAliasesLocked(cfg.Aliases)
	keys := s.keysSnapshotLocked()
	path := s.statePath
	usage := s.usageSnapshotLocked()
	aliases := s.aliasesSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	s.mu.Unlock()
	if persist {
		return s.saveState(path, keys, usage, aliases, rules)
	}
	return nil
}

func (s *Store) DeleteKey(id string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return errors.New("id is required")
	}
	s.mu.Lock()
	if _, ok := s.keys[id]; !ok {
		s.mu.Unlock()
		return ErrUnknownKey
	}
	delete(s.keys, id)
	s.rebuildKeysByHashLocked()
	s.clearPendingPicksForKeyLocked(id)
	keys := s.keysSnapshotLocked()
	usage := s.usageSnapshotLocked()
	path := s.statePath
	limiter := s.limiter
	usageLedger := s.usage
	s.mu.Unlock()
	if limiter != nil {
		limiter.Reset(id)
	}
	if usageLedger != nil {
		usageLedger.resetUsage(id)
	}
	return s.saveState(path, keys, usage, s.AliasesSnapshot(), s.ClassifyRulesSnapshot())
}

func (s *Store) RotateKey(id string) (string, KeyConfig, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	id = strings.TrimSpace(id)
	if id == "" {
		return "", KeyConfig{}, errors.New("id is required")
	}
	plain, err := GenerateKey()
	if err != nil {
		return "", KeyConfig{}, err
	}
	hash, err := HashKey(plain)
	if err != nil {
		return "", KeyConfig{}, err
	}
	s.mu.Lock()
	key := s.keys[id]
	if key == nil {
		s.mu.Unlock()
		return "", KeyConfig{}, ErrUnknownKey
	}
	key.KeyHash = hash
	key.KeyPreview = PreviewKey(plain)
	key.UpdatedAt = time.Now().UTC()
	copy := *key
	copy.Models = append([]ModelRule(nil), key.Models...)
	s.rebuildKeysByHashLocked()
	s.clearPendingPicksForKeyLocked(id)
	keys := s.keysSnapshotLocked()
	usage := s.usageSnapshotLocked()
	path := s.statePath
	limiter := s.limiter
	s.mu.Unlock()
	if limiter != nil {
		limiter.Reset(id)
	}
	if err := s.saveState(path, keys, usage, s.AliasesSnapshot(), s.ClassifyRulesSnapshot()); err != nil {
		return "", KeyConfig{}, err
	}
	return plain, copy, nil
}

func (s *Store) ResetRPM(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("id is required")
	}
	limiter, _ := s.runtimeComponents()
	if limiter != nil {
		limiter.Reset(id)
	}
	return nil
}

// ResetUsageWindow clears one key's daily or weekly usage and persists the
// change before returning. A weekly reset also clears the daily window.
func (s *Store) ResetUsageWindow(id string, window UsageResetWindow) (UsageResetResult, error) {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()

	id = strings.TrimSpace(id)
	if id == "" {
		return UsageResetResult{}, errors.New("id is required")
	}
	if window != UsageResetDaily && window != UsageResetWeekly {
		return UsageResetResult{}, fmt.Errorf("%w: %q", ErrInvalidUsageResetWindow, window)
	}

	s.mu.RLock()
	_, exists := s.keys[id]
	usage := s.usage
	path := s.statePath
	s.mu.RUnlock()
	if !exists {
		return UsageResetResult{}, ErrUnknownKey
	}
	if usage == nil {
		return UsageResetResult{KeyID: id, Window: window}, nil
	}

	// The flusher takes persistMu before snapshotting the ledger, so take the
	// same lock order here. This prevents a pre-reset flush snapshot from being
	// written after the reset has already been persisted. Keep usage.mu through
	// the atomic state-file write as well: this establishes a single reset cut
	// and lets a failed write restore the exact pre-reset ledger without
	// overwriting usage recorded concurrently. Resets are low-frequency admin
	// operations, so the short accounting-path stall is an intentional tradeoff.
	s.persistMu.Lock()
	usage.mu.Lock()
	previous, hadPrevious := usage.entries[id]
	previous = cloneUsageState(previous)
	result := usage.resetWindowLocked(id, window, usage.now())
	snapshot := usage.snapshotLocked()
	if err := SaveUsageOnly(path, snapshot); err != nil {
		if hadPrevious {
			usage.entries[id] = previous
		} else {
			delete(usage.entries, id)
		}
		usage.mu.Unlock()
		s.persistMu.Unlock()
		return UsageResetResult{}, fmt.Errorf("persist usage reset: %w", err)
	}
	usage.mu.Unlock()
	s.persistMu.Unlock()
	return result, nil
}

// --- Global alias mapping table management ---

// UpsertAlias adds or replaces an alias in the global table. Validates the
// alias (non-empty name, at least one target, valid dispatch/billing mode).
// Persists the full state to disk.
func (s *Store) UpsertAlias(alias AliasMapping) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	// Build a temp config to validate the single alias.
	existing := s.AliasesSnapshot()
	// Replace or append.
	found := false
	for i, a := range existing {
		if strings.EqualFold(a.Alias, alias.Alias) {
			existing[i] = alias
			found = true
			break
		}
	}
	if !found {
		existing = append(existing, alias)
	}
	tmp := Config{Enabled: true, StateFile: s.StatePath(), Aliases: existing}
	if err := normalizeConfig(&tmp); err != nil {
		return err
	}
	s.mu.Lock()
	s.updateAliasesLocked(tmp.Aliases)
	// Re-resolve all keys' Models from the updated alias table.
	s.resolveAllModelsLocked()
	keys := s.keysSnapshotLocked()
	usage := s.usageSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	return s.saveState(path, keys, usage, s.AliasesSnapshot(), s.ClassifyRulesSnapshot())
}

// DeleteAlias removes an alias from the global table. Returns an error if any
// key still references it (must remove references first).
func (s *Store) DeleteAlias(aliasName string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	aliasName = strings.TrimSpace(aliasName)
	if aliasName == "" {
		return errors.New("alias name is required")
	}
	// Check for key references.
	s.mu.RLock()
	refCount := 0
	for _, key := range s.keys {
		for _, ref := range key.Aliases {
			if strings.EqualFold(ref.Alias, aliasName) {
				refCount++
			}
		}
	}
	s.mu.RUnlock()
	if refCount > 0 {
		return errors.New("alias is referenced by " + itoa(refCount) + " key(s); remove references first")
	}
	existing := s.AliasesSnapshot()
	filtered := existing[:0]
	for _, a := range existing {
		if !strings.EqualFold(a.Alias, aliasName) {
			filtered = append(filtered, a)
		}
	}
	s.mu.Lock()
	s.updateAliasesLocked(filtered)
	keys := s.keysSnapshotLocked()
	usage := s.usageSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	return s.saveState(path, keys, usage, s.AliasesSnapshot(), s.ClassifyRulesSnapshot())
}

// --- Classification rule management ---

// UpsertClassifyRule adds or replaces a classification rule. Validates the
// regex pattern. Persists the full state to disk.
func (s *Store) UpsertClassifyRule(rule ClassifyRule) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	existing := s.ClassifyRulesSnapshot()
	found := false
	for i, r := range existing {
		if strings.EqualFold(r.Name, rule.Name) {
			existing[i] = rule
			found = true
			break
		}
	}
	if !found {
		existing = append(existing, rule)
	}
	tmp := Config{Enabled: true, StateFile: s.StatePath(), ClassifyRules: existing}
	if err := normalizeConfig(&tmp); err != nil {
		return err
	}
	s.mu.Lock()
	s.classifyRules = tmp.ClassifyRules
	onChanged := s.onClassifyRulesChanged
	keys := s.keysSnapshotLocked()
	usage := s.usageSnapshotLocked()
	aliases := s.aliasesSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	callClassifyRulesChanged(onChanged)
	return s.saveState(path, keys, usage, aliases, rules)
}

// DeleteClassifyRule removes a classification rule by name.
func (s *Store) DeleteClassifyRule(name string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	name = strings.TrimSpace(name)
	if name == "" {
		return errors.New("rule name is required")
	}
	existing := s.ClassifyRulesSnapshot()
	filtered := existing[:0]
	for _, r := range existing {
		if !strings.EqualFold(r.Name, name) {
			filtered = append(filtered, r)
		}
	}
	s.mu.Lock()
	s.classifyRules = filtered
	onChanged := s.onClassifyRulesChanged
	keys := s.keysSnapshotLocked()
	usage := s.usageSnapshotLocked()
	aliases := s.aliasesSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	callClassifyRulesChanged(onChanged)
	return s.saveState(path, keys, usage, aliases, rules)
}

// ReorderClassifyRules reorders the classification rules to match the given
// name order. Rules not in the list keep their relative order at the end.
func (s *Store) ReorderClassifyRules(names []string) error {
	s.updateMu.Lock()
	defer s.updateMu.Unlock()
	existing := s.ClassifyRulesSnapshot()
	byName := make(map[string]ClassifyRule)
	for _, r := range existing {
		byName[strings.ToLower(r.Name)] = r
	}
	var reordered []ClassifyRule
	used := make(map[string]bool)
	for _, name := range names {
		if r, ok := byName[strings.ToLower(name)]; ok {
			reordered = append(reordered, r)
			used[strings.ToLower(name)] = true
		}
	}
	for _, r := range existing {
		if !used[strings.ToLower(r.Name)] {
			reordered = append(reordered, r)
		}
	}
	s.mu.Lock()
	s.classifyRules = reordered
	onChanged := s.onClassifyRulesChanged
	keys := s.keysSnapshotLocked()
	usage := s.usageSnapshotLocked()
	aliases := s.aliasesSnapshotLocked()
	rules := s.classifyRulesSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	callClassifyRulesChanged(onChanged)
	return s.saveState(path, keys, usage, aliases, rules)
}

// resolveAllModelsLocked re-populates every key's Models from its Aliases
// refs + the global alias table. Caller must hold s.mu.
func (s *Store) resolveAllModelsLocked() {
	for _, key := range s.keys {
		if len(key.Aliases) > 0 {
			key.Models = resolveAliasRefsToModels(key.Aliases, s.aliases)
		}
	}
}

// itoa is a minimal int→string to avoid importing strconv in this file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

// SetOnClassifyRulesChanged registers a callback fired when classify rules
// change, so the plugin can clear its classify cache.
func (s *Store) SetOnClassifyRulesChanged(fn func()) {
	s.mu.Lock()
	s.onClassifyRulesChanged = fn
	s.mu.Unlock()
}

// callClassifyRulesChanged invokes a callback captured while holding s.mu.
// It must be called after releasing s.mu because callbacks may re-enter Store.
func callClassifyRulesChanged(fn func()) {
	if fn != nil {
		fn()
	}
}

// usageSnapshotLocked returns a deep copy of the usage ledger. Caller must
// hold s.mu (write or read) — the ledger has its own mutex but we snapshot
// keys + usage together under s.mu so SaveState writes a consistent pair.
func (s *Store) usageSnapshotLocked() map[string]*UsageState {
	if s.usage == nil {
		return nil
	}
	return s.usage.snapshot()
}

// FlushUsage persists the current usage ledger to the state file alongside the
// current key list. Called by the background flusher and at lifecycle points
// (reconfigure / shutdown).
func (s *Store) FlushUsage() error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	s.mu.Lock()
	usage := s.usageSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	if path == "" {
		return nil
	}
	// Bug 3 fix: persist only the usage ledger, preserving the key list already
	// on disk. Keys are mutated synchronously via SaveState in the management
	// API (UpsertKey/DeleteKey/RotateKey), so the periodic flush must not
	// overwrite them with an in-memory snapshot that could be stale or
	// truncated.
	return SaveUsageOnly(path, usage)
}

func (s *Store) saveState(path string, keys []KeyConfig, usage map[string]*UsageState, aliases []AliasMapping, rules []ClassifyRule) error {
	s.persistMu.Lock()
	defer s.persistMu.Unlock()
	return SaveState(path, keys, usage, aliases, rules)
}

// StartUsageFlusher launches a goroutine that periodically persists the usage
// ledger to the state file. Idempotent. Returns a stop function; the plugin
// host should call it (or FlushUsage) at reconfigure/shutdown.
func (s *Store) StartUsageFlusher() func() {
	s.mu.Lock()
	if s.flusher != nil {
		stop := s.flusher.stop
		s.mu.Unlock()
		return stop
	}
	stopCh := make(chan struct{})
	doneCh := make(chan struct{})
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }
	f := &usageFlusher{stop: stop, stopCh: stopCh, doneCh: doneCh, store: s}
	s.flusher = f
	s.mu.Unlock()
	go f.loop()
	return stop
}

// StopUsageFlusher stops the background flusher and flushes once more.
func (s *Store) StopUsageFlusher() {
	s.mu.Lock()
	f := s.flusher
	s.flusher = nil
	s.mu.Unlock()
	if f != nil {
		f.stop()
		<-f.doneCh
	}
	_ = s.FlushUsage()
}

type usageFlusher struct {
	stop   func()
	stopCh chan struct{}
	doneCh chan struct{}
	store  *Store
}

func (f *usageFlusher) loop() {
	defer close(f.doneCh)
	t := time.NewTicker(usageFlushInterval)
	defer t.Stop()
	for {
		select {
		case <-f.stopCh:
			return
		case <-t.C:
			_ = f.store.FlushUsage()
		}
	}
}

func (s *Store) Status() map[string]any {
	s.mu.RLock()
	enabled := s.enabled
	statePath := s.statePath
	keys := s.keysSnapshotLocked()
	limiter := s.limiter
	usage := s.usage
	s.mu.RUnlock()
	rpmUsage := map[string]int{}
	if limiter != nil {
		rpmUsage = limiter.Snapshot()
	}
	out := map[string]any{
		"enabled":    enabled,
		"state_file": statePath,
		"key_count":  len(keys),
		"rpm_usage":  rpmUsage,
		"usage":      usageSummaryForKeys(usage, keys),
	}
	return out
}

func usageSummaryForKeys(usage *usageLedger, keys []KeyConfig) map[string]UsageSummary {
	if usage == nil {
		return map[string]UsageSummary{}
	}
	out := make(map[string]UsageSummary, len(keys))
	for _, key := range keys {
		out[key.ID] = usage.Summary(key)
	}
	return out
}
