package policy

import (
	"errors"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type Store struct {
	mu        sync.RWMutex
	enabled   bool
	statePath string
	keys      map[string]*KeyConfig
	limiter   *RateLimiter
	usage     *usageLedger
	// flusher for periodically persisting the usage ledger to the state file.
	flusher *usageFlusher
}

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
}

func NewStore() *Store {
	return &Store{
		enabled: DefaultConfig().Enabled,
		keys:    make(map[string]*KeyConfig),
		limiter: NewRateLimiter(),
		usage:   newUsageLedger(time.Now),
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
	if err := normalizeConfig(&cfg); err != nil {
		return err
	}
	statePath, err := ResolveStatePath(cfg.StateFile)
	if err != nil {
		return err
	}
	keys := cfg.Keys
	var loadedUsage map[string]*UsageState
	if state, errLoad := LoadState(statePath); errLoad == nil {
		keys = state.Keys
		loadedUsage = state.Usage
		if errNorm := normalizeConfig(&Config{Enabled: cfg.Enabled, StateFile: cfg.StateFile, Keys: keys}); errNorm != nil {
			return fmt.Errorf("load state: %w", errNorm)
		}
	} else if !errors.Is(errLoad, os.ErrNotExist) {
		return fmt.Errorf("load state: %w", errLoad)
	}

	next := make(map[string]*KeyConfig, len(keys))
	now := time.Now().UTC()
	for i := range keys {
		item := keys[i]
		if item.CreatedAt.IsZero() {
			item.CreatedAt = now
		}
		if item.UpdatedAt.IsZero() {
			item.UpdatedAt = item.CreatedAt
		}
		next[item.ID] = &item
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Stop any prior flusher before rebuilding keys/state path.
	if s.flusher != nil {
		s.flusher.stop()
		s.flusher = nil
	}
	s.enabled = cfg.Enabled
	s.statePath = statePath
	s.keys = next
	if s.limiter == nil {
		s.limiter = NewRateLimiter()
	}
	// Re-load usage into the (clock-bound) ledger for restart recovery. The
	// clock is preserved when set via SetClock; otherwise default time.Now.
	clockNow := s.usage.now
	s.usage = newUsageLedger(clockNow)
	s.usage.loadFromState(loadedUsage)
	return nil
}

func (s *Store) Enabled() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.enabled
}

func (s *Store) StatePath() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.statePath
}

func (s *Store) Authenticate(method, path string, headers http.Header, query map[string][]string, body []byte) AuthDecision {
	if !s.Enabled() {
		return AuthDecision{Known: false, Reason: "plugin_disabled"}
	}
	rawKey := ExtractAPIKey(headers, query)
	key := s.findBySecret(rawKey)
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
		decision.Reason = "models_endpoint_disabled"
		return decision
	}
	requested := ExtractRequestedModel(path, query, body)
	decision.Requested = requested
	if requested != "" {
		rule, ok := key.ModelForAlias(requested)
		if !ok {
			decision.Reason = "model_not_allowed"
			return decision
		}
		decision.Rule = rule
	}
	if s.limiter != nil && !s.limiter.Allow(key.ID, key.RPM) {
		decision.RateLimited = true
		decision.Reason = "rpm_exceeded"
		return decision
	}
	// Dollar usage limit check (daily / weekly). Only enforced when a limit is
	// set (>0). This is a pre-request gate; the request that pushes usage over
	// the limit is allowed through, and the next request is rejected — matching
	// the RPM limiter's "off-by-one" semantics.
	if s.usage != nil {
		if reason, _ := s.usage.OverLimit(*key); reason != "" {
			decision.CostLimited = true
			decision.Reason = reason
			return decision
		}
	}
	decision.Allowed = true
	decision.Reason = "allowed"
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
	rule, ok := key.ModelForAlias(requested)
	if !ok {
		return ModelRule{}, key.ID, false
	}
	return rule, key.ID, true
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
	alias := strings.TrimSpace(requested)
	if alias == "" {
		return 0
	}
	usage := ParseTokenUsage(body)
	if !usage.Found {
		return 0
	}
	inputPerMillion, outputPerMillion, _, priced := key.PriceForAlias(alias)
	cost := ComputeCost(inputPerMillion, outputPerMillion, priced, usage)
	if cost > 0 && s.usage != nil {
		// The response-body path sees only prompt/completion counts (no cache
		// breakdown), so cache counters stay 0 here; cache-aware accounting
		// happens in RecordUsage via ComputeCacheCostBreakdown. We still record
		// input tokens for hit-rate denominator parity (treat all prompt tokens
		// as non-cache input on this path, since we can't tell otherwise).
		// callCount=1: this was a successful, token-billed request.
		s.usage.RecordCost(key.ID, alias, cost, 0, 0, int64(usage.PromptTokens), int64(usage.CompletionTokens), 1)
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
	rule, _ := key.ModelForAlias(resolved)

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
		if s.usage != nil {
			// callCount=1 regardless of cost (even free calls count toward volume).
			s.usage.RecordCost(key.ID, resolved, cost, 0, 0, 0, 0, 1)
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
	provider := ""
	if rule.Alias != "" {
		provider = rule.Provider
	}
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
	if cost > 0 && s.usage != nil {
		// callCount=1: this was a successful, token-billed request.
		s.usage.RecordCost(key.ID, resolved, cost, cacheCost, cacheReadTokens, nonCacheInput, int64(detail.OutputTokens), 1)
	}
	return cost
}

// UsageSummaryFor returns the current daily/weekly usage + limits for a key
// (for the keys-list management API).
func (s *Store) UsageSummaryFor(key KeyConfig) UsageSummary {
	if s.usage == nil {
		return UsageSummary{DailyLimitUSD: key.DailyLimitUSD, WeeklyLimitUSD: key.WeeklyLimitUSD}
	}
	return s.usage.Summary(key)
}

// ResetUsage clears in-memory usage for a key (manual quota unlock).
func (s *Store) ResetUsage(id string) {
	if s.usage != nil {
		s.usage.resetUsage(id)
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
	if s.usage == nil {
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
	return *key, s.usage.AliasUsage(*key), true
}

func (s *Store) findBySecret(raw string) *KeyConfig {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	for _, key := range s.keys {
		if MatchHash(raw, key.KeyHash) {
			copy := *key
			copy.Models = append([]ModelRule(nil), key.Models...)
			return &copy
		}
	}
	return nil
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
	for _, key := range s.keys {
		if strings.EqualFold(key.ID, id) {
			copy := *key
			copy.Models = append([]ModelRule(nil), key.Models...)
			return &copy
		}
	}
	return nil
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
		keys = append(keys, copy)
	}
	return keys
}

func (s *Store) UpsertKey(input KeyConfig, persist bool) error {
	cfg := Config{Enabled: true, StateFile: s.StatePath(), Keys: []KeyConfig{input}}
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
	s.keys[key.ID] = &key
	keys := s.keysSnapshotLocked()
	path := s.statePath
	usage := s.usageSnapshotLocked()
	s.mu.Unlock()
	if persist {
		return SaveState(path, keys, usage)
	}
	return nil
}

func (s *Store) DeleteKey(id string) error {
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
	keys := s.keysSnapshotLocked()
	usage := s.usageSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	if s.limiter != nil {
		s.limiter.Reset(id)
	}
	if s.usage != nil {
		s.usage.resetUsage(id)
	}
	return SaveState(path, keys, usage)
}

func (s *Store) RotateKey(id string) (string, KeyConfig, error) {
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
	keys := s.keysSnapshotLocked()
	usage := s.usageSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	if s.limiter != nil {
		s.limiter.Reset(id)
	}
	if err := SaveState(path, keys, usage); err != nil {
		return "", KeyConfig{}, err
	}
	return plain, copy, nil
}

func (s *Store) ResetRPM(id string) error {
	if strings.TrimSpace(id) == "" {
		return errors.New("id is required")
	}
	if s.limiter != nil {
		s.limiter.Reset(id)
	}
	return nil
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
	s.mu.Lock()
	keys := s.keysSnapshotLocked()
	usage := s.usageSnapshotLocked()
	path := s.statePath
	s.mu.Unlock()
	if path == "" {
		return nil
	}
	return SaveState(path, keys, usage)
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
	var stopOnce sync.Once
	stop := func() { stopOnce.Do(func() { close(stopCh) }) }
	f := &usageFlusher{stop: stop, stopCh: stopCh, store: s}
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
	if f == nil {
		return
	}
	f.stop()
	_ = s.FlushUsage()
}

type usageFlusher struct {
	stop   func()
	stopCh chan struct{}
	store  *Store
}

func (f *usageFlusher) loop() {
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
	keyCount := len(s.keys)
	s.mu.RUnlock()
	return map[string]any{
		"enabled":    enabled,
		"state_file": statePath,
		"key_count":  keyCount,
		"rpm_usage":  s.limiter.Snapshot(),
		"usage":      s.usageUsageLocked(),
	}
}

// usageUsageLocked returns a summary map of all keys' usage (for status).
func (s *Store) usageUsageLocked() map[string]UsageSummary {
	if s.usage == nil {
		return map[string]UsageSummary{}
	}
	out := make(map[string]UsageSummary, len(s.keys))
	for id, key := range s.keys {
		out[id] = s.usage.Summary(*key)
	}
	return out
}
