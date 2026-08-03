package policy

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Enabled   bool        `yaml:"enabled" json:"enabled"`
	// Mode controls the plugin's identity model. "legacy" preserves the
	// historical plugin-owned keys and model mappings. "native-access" uses
	// CPA's api-keys as the only key source and stores authorization policy
	// only; it never creates, copies, rotates, or deletes keys.
	Mode       string      `yaml:"mode,omitempty" json:"mode,omitempty"`
	StateFile  string      `yaml:"state_file" json:"state_file"`
	// NativeKeysFile is the CPA config.yaml read by native-access mode.
	NativeKeysFile string  `yaml:"native_keys_file,omitempty" json:"native_keys_file,omitempty"`
	// NativeStateFile contains hashes, grants, quotas, and counters only.
	NativeStateFile string `yaml:"native_state_file,omitempty" json:"native_state_file,omitempty"`
	Keys      []KeyConfig `yaml:"keys" json:"keys"`
	// Aliases is the global alias mapping table. Each entry maps a downstream
	// alias name to one or more (provider, model, group) targets with a shared
	// pricing config. Keys reference aliases by name via KeyAliasRef.
	Aliases []AliasMapping `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	// ClassifyRules are user-defined credential classification rules. They run
	// BEFORE the built-in plan_type/tier detection and can override it. Built-in
	// rules (always present, read-only) handle unrecognized credentials.
	ClassifyRules []ClassifyRule `yaml:"classify_rules,omitempty" json:"classify_rules,omitempty"`
}

func acquireStateFileLock(path string) (func(), error) {
	lockPath := filepath.Clean(path) + ".lock"
	deadline := time.Now().Add(30 * time.Second)
	for {
		if err := os.Mkdir(lockPath, 0o700); err == nil {
			return func() { _ = os.Remove(lockPath) }, nil
		} else if !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if info, err := os.Stat(lockPath); err == nil && time.Since(info.ModTime()) > time.Minute {
			_ = os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("timed out waiting for state file lock %q", lockPath)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

type KeyConfig struct {
	ID         string      `yaml:"id" json:"id"`
	Name       string      `yaml:"name" json:"name"`
	Enabled    bool        `yaml:"enabled" json:"enabled"`
	KeyHash    string      `yaml:"key_hash" json:"key_hash"`
	KeyPreview string      `yaml:"key_preview" json:"key_preview"`
	PlainKey   string      `yaml:"plain_key,omitempty" json:"plain_key,omitempty"`
	RPM        int         `yaml:"rpm" json:"rpm"`
	Models     []ModelRule `yaml:"models" json:"models"`
	// Aliases references global alias mappings by name. When non-empty, the key
	// uses these aliases for routing and billing. Per-key price overrides are
	// optional (nil = use global alias pricing). This field coexists with
	// Models during migration: if Aliases is empty and Models is non-empty,
	// normalizeConfig auto-migrates Models → global Aliases + KeyAliasRef.
	Aliases []KeyAliasRef `yaml:"aliases,omitempty" json:"aliases,omitempty"`
	// AllowModelsEndpoint lets this key reach GET /v1/models. CPA has no way
	// for a plugin to filter the model list per downstream key (OpenAIModels
	// returns the global registry and never goes through an executor or
	// response interceptor, and the access hook can only 401/allow — not
	// rewrite the body). So the only per-key control we can enforce at the
	// plugin layer is the binary choice: 401 (hide the list entirely) or
	// allow (client sees the full global list). Default false = 401.
	AllowModelsEndpoint bool    `yaml:"allow_models_endpoint,omitempty" json:"allow_models_endpoint,omitempty"`
	DailyLimitUSD       float64 `yaml:"daily_limit_usd,omitempty" json:"daily_limit_usd,omitempty"`
	// WeeklyLimitUSD caps the dollar usage over a rolling 7-day window. 0 = unlimited.
	WeeklyLimitUSD float64   `yaml:"weekly_limit_usd,omitempty" json:"weekly_limit_usd,omitempty"`
	CreatedAt      time.Time `yaml:"created_at,omitempty" json:"created_at,omitempty"`
	UpdatedAt      time.Time `yaml:"updated_at,omitempty" json:"updated_at,omitempty"`
}

type ModelRule struct {
	Alias       string `yaml:"alias" json:"alias"`
	Provider    string `yaml:"provider" json:"provider"`
	TargetModel string `yaml:"target_model" json:"target_model"`
	// Group optionally narrows which auth files serve this alias. Empty means
	// "any file for the provider" (legacy behavior). The planner sets it for
	// providers whose auth files carry a tier/plan identity (codex plan_type,
	// antigravity tier) so the plugin's Scheduler can filter candidates by that
	// attribute. Format: "<plan>" (e.g. "free", "team", "plus") or "supported"
	// when codex lacks an id_token claim and the row must NOT be tier-filtered.
	// UI groups reflect this: codex.free / codex.team / codex.supported / codex.unknown.
	Group string `yaml:"group,omitempty" json:"group,omitempty"`
	// InputPricePerMillion is the USD price per 1M prompt tokens for this alias.
	InputPricePerMillion float64 `yaml:"input_price_per_million,omitempty" json:"input_price_per_million,omitempty"`
	// OutputPricePerMillion is the USD price per 1M completion tokens for this alias.
	OutputPricePerMillion float64 `yaml:"output_price_per_million,omitempty" json:"output_price_per_million,omitempty"`
	// CacheReadPricePerMillion is the USD price per 1M cache-hit input tokens
	// (prompt-caching read). 0 = treat cache hits at the regular input price.
	// Provider semantics differ (see ComputeCacheCost): for Anthropic, cache-read
	// tokens are reported separately from input; for OpenAI/Gemini/Codex they are
	// a subset already counted inside input. This price applies to cache-hit
	// tokens in both cases, replacing the input price for that subset.
	// Only used when BillingMode == "" or "tokens"; ignored under "per_call".
	CacheReadPricePerMillion float64 `yaml:"cache_read_price_per_million,omitempty" json:"cache_read_price_per_million,omitempty"`
	// BillingMode selects how this alias is billed per successful request:
	//   - "" or "tokens" (default): bill by token counts using the three prices
	//     above (existing behavior).
	//   - "per_call": bill a fixed PerCallUSD per successful request, ignoring
	//     token counts. The token-price fields are preserved but dormant; switching
	//     back to "tokens" reuses them.
	BillingMode string `yaml:"billing_mode,omitempty" json:"billing_mode,omitempty"`
	// PerCallUSD is the fixed USD charge per successful request when
	// BillingMode == "per_call". 0 is allowed (free calls; CallCount still
	// increments for reporting). Negative is rejected by normalizeConfig. Only
	// meaningful under "per_call"; ignored under "tokens".
	PerCallUSD float64 `yaml:"per_call_usd,omitempty" json:"per_call_usd,omitempty"`
}

// AliasMapping is one entry in the global alias mapping table. It maps a
// downstream alias name to one or more targets (provider+model+group) with a
// shared pricing config. When a key references this alias, requests for the
// alias are routed to one of the targets based on the dispatch mode.
type AliasMapping struct {
	Alias       string        `yaml:"alias" json:"alias"`
	Targets     []AliasTarget `yaml:"targets" json:"targets"`
	Dispatch    string        `yaml:"dispatch,omitempty" json:"dispatch,omitempty"` // "round-robin" (default) | "priority"
	BillingMode string        `yaml:"billing_mode,omitempty" json:"billing_mode,omitempty"`
	// Pricing fields (same semantics as ModelRule). When BillingMode == "tokens",
	// InputPricePerMillion / OutputPricePerMillion / CacheReadPricePerMillion
	// are used. When BillingMode == "per_call", PerCallUSD is used.
	InputPricePerMillion     float64 `yaml:"input_price_per_million,omitempty" json:"input_price_per_million,omitempty"`
	OutputPricePerMillion    float64 `yaml:"output_price_per_million,omitempty" json:"output_price_per_million,omitempty"`
	CacheReadPricePerMillion float64 `yaml:"cache_read_price_per_million,omitempty" json:"cache_read_price_per_million,omitempty"`
	PerCallUSD               float64 `yaml:"per_call_usd,omitempty" json:"per_call_usd,omitempty"`
}

// AliasTarget is one selectable destination for an alias. Group optionally
// narrows which auth files serve this target (codex plan_type, antigravity
// tier, or a custom group from ClassifyRules). Empty = any file for the
// provider (legacy behavior).
type AliasTarget struct {
	Provider    string `yaml:"provider" json:"provider"`
	TargetModel string `yaml:"target_model" json:"target_model"`
	Group       string `yaml:"group,omitempty" json:"group,omitempty"`
}

// ClassifyRule is a user-defined credential classification rule. It matches
// a field on the auth candidate (filename, provider, plan_type, tier, or any
// custom attribute) against a regex pattern, and assigns matching candidates
// to a named group. Rules run in order; the first match wins (a credential can
// belong to multiple groups when multiple rules match — multi-group semantics).
// Custom rules run BEFORE the built-in plan_type/tier detection, so they can
// override the default classification.
type ClassifyRule struct {
	Name    string `yaml:"name" json:"name"`
	Field   string `yaml:"field" json:"field"`     // "filename" | "provider" | "plan_type" | "tier" | custom attribute name
	Pattern string `yaml:"pattern" json:"pattern"` // regex (Go syntax)
	Group   string `yaml:"group" json:"group"`     // target group name
	Enabled bool   `yaml:"enabled" json:"enabled"`
	// compiled is the pre-compiled regex, set by normalizeConfig for fast
	// evaluation. Not serialized.
	compiled *regexp.Regexp `yaml:"-" json:"-"`
}

// Compiled returns the pre-compiled regex, or nil if not yet compiled.
func (r *ClassifyRule) Compiled() *regexp.Regexp {
	return r.compiled
}

// KeyAliasRef is a key's reference to a global alias. The key uses the alias's
// targets and dispatch mode, but can optionally override pricing per-key.
// nil pointer fields mean "use the global default".
type KeyAliasRef struct {
	Alias string `yaml:"alias" json:"alias"`
	// Optional per-key price overrides. nil = use global alias pricing.
	InputPricePerMillion     *float64 `yaml:"input_price_per_million,omitempty" json:"input_price_per_million,omitempty"`
	OutputPricePerMillion    *float64 `yaml:"output_price_per_million,omitempty" json:"output_price_per_million,omitempty"`
	CacheReadPricePerMillion *float64 `yaml:"cache_read_price_per_million,omitempty" json:"cache_read_price_per_million,omitempty"`
	PerCallUSD               *float64 `yaml:"per_call_usd,omitempty" json:"per_call_usd,omitempty"`
}

// UsageState holds per-key dollar usage accounting persisted in the state JSON.
// It carries rolling daily/weekly windows plus a per-alias breakdown
// (ByAlias). The per-alias breakdown tracks BOTH a daily and a weekly window
// (see AliasUsageWindows) so the key detail page can show per-alias today /
// rolling-week figures.
//
// Legacy state files stored ByAlias as map[string]UsageWindow (a single
// window per alias). UsageState.UnmarshalJSON auto-migrates that shape into
// the dual-window form (old value → Daily; Weekly zeroed).
type UsageState struct {
	Daily   UsageWindow                  `json:"daily"`
	Weekly  UsageWindow                  `json:"weekly"`
	ByAlias map[string]AliasUsageWindows `json:"by_alias,omitempty"`
}

// AliasUsageWindows holds the daily and rolling-weekly usage windows for a
// single alias under a key. Replaces the legacy single-window ByAlias map;
// old state files are auto-migrated on load (see UsageState.UnmarshalJSON).
type AliasUsageWindows struct {
	Daily  UsageWindow `json:"daily"`
	Weekly UsageWindow `json:"weekly"`
}

// UnmarshalJSON migrates the legacy ByAlias shape (map[string]UsageWindow,
// a single window per alias) into the current dual-window form
// (map[string]AliasUsageWindows). Detection is per-entry: an entry carrying a
// "daily" or "weekly" key is read as the new form; otherwise it is read as a
// bare UsageWindow and placed into Daily (Weekly zeroed). Unknown shapes are
// skipped rather than failing the whole load.
func (s *UsageState) UnmarshalJSON(raw []byte) error {
	var p struct {
		Daily   UsageWindow     `json:"daily"`
		Weekly  UsageWindow     `json:"weekly"`
		ByAlias json.RawMessage `json:"by_alias,omitempty"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	s.Daily = p.Daily
	s.Weekly = p.Weekly
	s.ByAlias = make(map[string]AliasUsageWindows)
	if len(p.ByAlias) == 0 || string(p.ByAlias) == "null" {
		return nil
	}
	var entries map[string]json.RawMessage
	if err := json.Unmarshal(p.ByAlias, &entries); err != nil {
		return err
	}
	for alias, rawEntry := range entries {
		if len(rawEntry) == 0 || string(rawEntry) == "null" {
			continue
		}
		if hasJSONKey(rawEntry, "daily") || hasJSONKey(rawEntry, "weekly") {
			var w AliasUsageWindows
			if err := json.Unmarshal(rawEntry, &w); err == nil {
				s.ByAlias[alias] = w
			}
			continue
		}
		// Legacy single-window format: migrate into Daily (weekly zeroed).
		var w UsageWindow
		if err := json.Unmarshal(rawEntry, &w); err == nil {
			s.ByAlias[alias] = AliasUsageWindows{Daily: w}
		}
	}
	return nil
}

// hasJSONKey reports whether a JSON object literal contains the given object
// key. It is a cheap substring check on the quoted key form, sufficient for
// migration-time shape detection (not a full parse).
func hasJSONKey(raw json.RawMessage, key string) bool {
	return bytes.Contains(raw, []byte(`"`+key+`"`))
}

// UsageWindow tracks a dollar total bound to a window-start timestamp, plus
// cache-specific counters for reporting (not used for limit enforcement).
//
// Cache fields are reported alongside the daily/weekly usage so the UI can show
// cache spend and hit-rate without re-deriving it. They accumulate only cache
// HITS — cache-creation (write) tokens are intentionally excluded, since their
// pricing and meaning differ across providers and they are not "reads".
//
//   - CacheReadTokens: cache-hit input tokens billed in this window.
//   - CacheCostUSD:    the dollar portion billed at the cache-read price
//     (only when a cache price was explicitly configured; 0
//     when cache hits were folded into the input-price line).
//   - InputTokens:     non-cache input tokens billed in this window, i.e. the
//     prompt tokens charged at the regular input price. For
//     subset providers this is InputTokens - cacheRead; for
//     additive providers it is InputTokens + cacheCreation.
//     Used as the denominator of hit-rate = cacheRead /
//     (cacheRead + InputTokens).
type UsageWindow struct {
	TotalUSD        float64   `json:"total_usd"`
	WindowStart     time.Time `json:"window_start,omitempty"`
	CacheReadTokens int64     `json:"cache_read_tokens,omitempty"`
	CacheCostUSD    float64   `json:"cache_cost_usd,omitempty"`
	InputTokens     int64     `json:"input_tokens,omitempty"`
	// OutputTokens is the non-cache completion-token count billed in this
	// window (tokens charged at the output price). Reported for display on the
	// per-alias detail page; not used for limit enforcement.
	OutputTokens int64 `json:"output_tokens,omitempty"`
	// CallCount is the number of successful requests billed into this window —
	// both token-billed and per-call-billed requests increment it. Failed
	// requests do NOT increment it (a per-call charge only applies to HTTP-200
	// outcomes). Reported for display only; not used for limit enforcement.
	CallCount int64 `json:"call_count,omitempty"`
}

type State struct {
	Version   int                    `json:"version"`
	Keys      []KeyConfig            `json:"keys"`
	Usage     map[string]*UsageState `json:"usage,omitempty"`
	UpdatedAt time.Time              `json:"updated_at"`
	// Aliases is the global alias mapping table, persisted so that key alias
	// references survive restarts even when config.yaml is not re-read. On
	// Configure, the config.yaml Aliases take precedence; state Aliases are a
	// fallback for the state-only reload path (e.g. FlushUsage recovery).
	Aliases       []AliasMapping `json:"aliases,omitempty"`
	ClassifyRules []ClassifyRule `json:"classify_rules,omitempty"`
}

func DefaultConfig() Config {
	return Config{
		Enabled:         true,
		Mode:            "legacy",
		StateFile:       "cpa-key-policy-state.json",
		NativeKeysFile:  "/CLIProxyAPI/config.yaml",
		NativeStateFile: "cpa-key-access-policy-state.json",
	}
}

func DecodeConfig(raw []byte) (Config, error) {
	cfg := DefaultConfig()
	if len(strings.TrimSpace(string(raw))) == 0 {
		return cfg, nil
	}
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return Config{}, err
	}
	if strings.TrimSpace(cfg.StateFile) == "" {
		cfg.StateFile = DefaultConfig().StateFile
	}
	cfg.Mode = strings.ToLower(strings.TrimSpace(cfg.Mode))
	if cfg.Mode == "" {
		cfg.Mode = DefaultConfig().Mode
	}
	if cfg.Mode != "legacy" && cfg.Mode != "native-access" {
		return Config{}, fmt.Errorf("mode must be \"legacy\" or \"native-access\"")
	}
	if strings.TrimSpace(cfg.NativeKeysFile) == "" {
		cfg.NativeKeysFile = DefaultConfig().NativeKeysFile
	}
	if strings.TrimSpace(cfg.NativeStateFile) == "" {
		cfg.NativeStateFile = DefaultConfig().NativeStateFile
	}
	if err := normalizeConfig(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// migrateModelsToAliases promotes per-key ModelRule entries to the global
// AliasMapping table. For each key that has Models but no Aliases, each
// ModelRule is looked up by ALIAS NAME in the global table (not the
// full alias+provider+target_model tuple, so a multi-target alias already
// defined in the global table is reused wholesale rather than split into
// duplicate global aliases). If the alias name is unknown, a new single-
// target alias is created from this ModelRule's target. Per-key refs are
// deduped by alias name (a key referencing a multi-target alias gets ONE
// ref, so resolveAliasRefsToModels expands it to all the alias's targets).
// After migration, the key's Models slice is cleared (the canonical source
// becomes Aliases). Keys that already have Aliases are left untouched.
func migrateModelsToAliases(cfg *Config) {
	if len(cfg.Keys) == 0 {
		return
	}
	// Index existing global aliases by ALIAS NAME (lowercased) so multi-
	// target aliases already in the table are reused, not split.
	existing := make(map[string]int) // alias name -> index in cfg.Aliases
	for i, a := range cfg.Aliases {
		existing[strings.ToLower(a.Alias)] = i
	}
	for i := range cfg.Keys {
		key := &cfg.Keys[i]
		if len(key.Models) == 0 {
			// No Models to migrate. Leave existing Aliases intact — this is the
			// normal reload path where Models is a derived field (stripped from
			// disk by SaveState and repopulated in memory by Configure). Only the
			// PATCH path sends non-nil Models, and an explicit clear sends Models
			// as an empty non-nil slice which still falls through to the
			// reconciliation below and correctly drops all alias refs.
			continue
		}
		// Build the set of alias names present in this key's Models — this is
		// the authoritative list the user wants. Reconcile the key's existing
		// Aliases against it: keep refs whose alias still has a Model entry,
		// drop refs whose alias was removed, and add refs for newly added
		// alias names. Price overrides on surviving refs are preserved.
		modelAliasNames := make(map[string]struct{}, len(key.Models))
		for _, m := range key.Models {
			modelAliasNames[strings.ToLower(m.Alias)] = struct{}{}
		}
		var reconciled []KeyAliasRef
		refSeen := map[string]struct{}{} // dedup reconciled by alias name
		for _, ref := range key.Aliases {
			lk := strings.ToLower(ref.Alias)
			if _, ok := modelAliasNames[lk]; !ok {
				continue // this alias was removed from the key's models
			}
			if _, dup := refSeen[lk]; dup {
				continue // dedup
			}
			refSeen[lk] = struct{}{}
			reconciled = append(reconciled, ref) // preserve price overrides
		}
		for _, m := range key.Models {
			al := strings.ToLower(m.Alias)
			target := AliasTarget{Provider: m.Provider, TargetModel: m.TargetModel, Group: m.Group}
			var ai int
			if idx, ok := existing[al]; ok {
				// Reuse the existing global alias. Merge this ModelRule's
				// target into it if not already present — a multi-target
				// alias has multiple ModelRules with the same alias name
				// but different provider/target, and all targets must be
				// collected into one global AliasMapping.
				ai = idx
				mergeAliasTarget(&cfg.Aliases[idx], target)
			} else {
				// Create a new global alias with this target.
				ai = len(cfg.Aliases)
				cfg.Aliases = append(cfg.Aliases, AliasMapping{
					Alias:                    m.Alias,
					Targets:                  []AliasTarget{target},
					Dispatch:                 "round-robin",
					BillingMode:              m.BillingMode,
					InputPricePerMillion:     m.InputPricePerMillion,
					OutputPricePerMillion:    m.OutputPricePerMillion,
					CacheReadPricePerMillion: m.CacheReadPricePerMillion,
					PerCallUSD:               m.PerCallUSD,
				})
				existing[al] = ai
			}
			_ = ai // ai is the alias index; we don't need it below since the ref
			//      carries the name and the key's resolveAliasRefsToModels
			//      re-looks-up the alias by name at Configure time.
			// Add a KeyAliasRef only if this alias isn't already in reconciled.
			if _, dup := refSeen[al]; dup {
				continue
			}
			refSeen[al] = struct{}{}
			// We don't set price overrides here — the global entry's pricing was
			// taken from the ModelRule that first created (or defined) it, which
			// is good enough for migration. Future per-key overrides are a manual
			// UI action.
			reconciled = append(reconciled, KeyAliasRef{Alias: m.Alias})
		}
		key.Aliases = reconciled
		// Clear Models — the canonical source is now Aliases.
		key.Models = nil
	}
}

// mergeAliasTarget appends target to a.Targets if an identical target
// (provider + target_model + group) is not already present. This lets
// multi-target aliases — submitted as multiple ModelRules sharing one
// alias name — accumulate all their targets into a single global
// AliasMapping during migration.
func mergeAliasTarget(a *AliasMapping, target AliasTarget) {
	for _, t := range a.Targets {
		if strings.EqualFold(t.Provider, target.Provider) &&
			strings.EqualFold(t.TargetModel, target.TargetModel) &&
			strings.EqualFold(t.Group, target.Group) {
			return
		}
	}
	a.Targets = append(a.Targets, target)
}

func normalizeConfig(cfg *Config) error {
	// Auto-migrate: when a key has per-key Models but no Aliases, promote
	// Models to the global alias table and convert the key to reference aliases.
	// This runs on every normalizeConfig call (DecodeConfig, Configure, state
	// load) so old configs and old state files are always migrated.
	migrateModelsToAliases(cfg)
	seen := map[string]struct{}{}
	for i := range cfg.Keys {
		key := &cfg.Keys[i]
		key.ID = strings.TrimSpace(key.ID)
		key.Name = strings.TrimSpace(key.Name)
		key.KeyHash = strings.TrimSpace(key.KeyHash)
		key.KeyPreview = strings.TrimSpace(key.KeyPreview)
		if key.ID == "" {
			return errors.New("key id is required")
		}
		if _, exists := seen[key.ID]; exists {
			return fmt.Errorf("duplicate key id %q", key.ID)
		}
		seen[key.ID] = struct{}{}
		if key.Name == "" {
			key.Name = key.ID
		}
		if key.RPM < 0 {
			return fmt.Errorf("key %q rpm cannot be negative", key.ID)
		}
		if key.DailyLimitUSD < 0 {
			return fmt.Errorf("key %q daily_limit_usd cannot be negative", key.ID)
		}
		if key.WeeklyLimitUSD < 0 {
			return fmt.Errorf("key %q weekly_limit_usd cannot be negative", key.ID)
		}
		for j := range key.Models {
			model := &key.Models[j]
			model.Alias = strings.TrimSpace(model.Alias)
			model.Provider = strings.ToLower(strings.TrimSpace(model.Provider))
			model.TargetModel = strings.TrimSpace(model.TargetModel)
			model.Group = strings.ToLower(strings.TrimSpace(model.Group))
			if model.Alias == "" || model.Provider == "" || model.TargetModel == "" {
				return fmt.Errorf("key %q model entries require alias, provider, and target_model", key.ID)
			}
			// NOTE: duplicate alias names within a key's Models are LEGITIMATE
			// for multi-target global aliases (resolveAliasRefsToModels emits
			// one ModelRule per target, all sharing the alias name). Do not
			// reject them; the pricing is unified at the alias level.
			if model.InputPricePerMillion < 0 || model.OutputPricePerMillion < 0 || model.CacheReadPricePerMillion < 0 {
				return fmt.Errorf("key %q model %q prices cannot be negative", key.ID, model.Alias)
			}
			// BillingMode: normalize empty to "tokens"; reject unknown modes.
			switch strings.ToLower(strings.TrimSpace(model.BillingMode)) {
			case "", "tokens":
				model.BillingMode = "tokens"
			case "per_call":
				model.BillingMode = "per_call"
			default:
				return fmt.Errorf("key %q model %q billing_mode %q must be \"tokens\" or \"per_call\"", key.ID, model.Alias, model.BillingMode)
			}
			if model.PerCallUSD < 0 {
				return fmt.Errorf("key %q model %q per_call_usd cannot be negative", key.ID, model.Alias)
			}
		}
	}

	// --- Global alias mapping table validation ---
	aliasSeen := map[string]struct{}{}
	for i := range cfg.Aliases {
		a := &cfg.Aliases[i]
		a.Alias = strings.TrimSpace(a.Alias)
		if a.Alias == "" {
			return fmt.Errorf("alias entry %d: alias name is required", i)
		}
		if _, exists := aliasSeen[strings.ToLower(a.Alias)]; exists {
			return fmt.Errorf("duplicate alias name %q", a.Alias)
		}
		aliasSeen[strings.ToLower(a.Alias)] = struct{}{}
		if len(a.Targets) == 0 {
			return fmt.Errorf("alias %q must have at least one target", a.Alias)
		}
		for j := range a.Targets {
			t := &a.Targets[j]
			t.Provider = strings.ToLower(strings.TrimSpace(t.Provider))
			t.TargetModel = strings.TrimSpace(t.TargetModel)
			t.Group = strings.ToLower(strings.TrimSpace(t.Group))
			if t.Provider == "" || t.TargetModel == "" {
				return fmt.Errorf("alias %q target %d: provider and target_model are required", a.Alias, j)
			}
		}
		switch strings.ToLower(strings.TrimSpace(a.Dispatch)) {
		case "", "round-robin":
			a.Dispatch = "round-robin"
		case "priority":
			a.Dispatch = "priority"
		default:
			return fmt.Errorf("alias %q dispatch %q must be \"round-robin\" or \"priority\"", a.Alias, a.Dispatch)
		}
		switch strings.ToLower(strings.TrimSpace(a.BillingMode)) {
		case "", "tokens":
			a.BillingMode = "tokens"
		case "per_call":
			a.BillingMode = "per_call"
		default:
			return fmt.Errorf("alias %q billing_mode %q must be \"tokens\" or \"per_call\"", a.Alias, a.BillingMode)
		}
		if a.InputPricePerMillion < 0 || a.OutputPricePerMillion < 0 || a.CacheReadPricePerMillion < 0 {
			return fmt.Errorf("alias %q prices cannot be negative", a.Alias)
		}
		if a.PerCallUSD < 0 {
			return fmt.Errorf("alias %q per_call_usd cannot be negative", a.Alias)
		}
	}

	// --- Classification rules validation ---
	ruleSeen := map[string]struct{}{}
	for i := range cfg.ClassifyRules {
		r := &cfg.ClassifyRules[i]
		r.Name = strings.TrimSpace(r.Name)
		r.Field = strings.TrimSpace(r.Field)
		r.Pattern = strings.TrimSpace(r.Pattern)
		r.Group = strings.TrimSpace(r.Group)
		if r.Name == "" {
			return fmt.Errorf("classify rule %d: name is required", i)
		}
		if _, exists := ruleSeen[strings.ToLower(r.Name)]; exists {
			return fmt.Errorf("duplicate classify rule name %q", r.Name)
		}
		ruleSeen[strings.ToLower(r.Name)] = struct{}{}
		if r.Field == "" {
			return fmt.Errorf("classify rule %q: field is required", r.Name)
		}
		if r.Pattern == "" {
			return fmt.Errorf("classify rule %q: pattern is required", r.Name)
		}
		if r.Group == "" {
			return fmt.Errorf("classify rule %q: group is required", r.Name)
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return fmt.Errorf("classify rule %q: invalid regex %q: %w", r.Name, r.Pattern, err)
		}
		r.compiled = re
	}

	// --- Key alias references validation ---
	aliasMap := make(map[string]*AliasMapping, len(cfg.Aliases))
	for i := range cfg.Aliases {
		aliasMap[strings.ToLower(cfg.Aliases[i].Alias)] = &cfg.Aliases[i]
	}
	for i := range cfg.Keys {
		key := &cfg.Keys[i]
		refSeen := map[string]int{} // alias name -> index in key.Aliases (dedup)
		refs := key.Aliases[:0]
		for j := range key.Aliases {
			ref := &key.Aliases[j]
			ref.Alias = strings.TrimSpace(ref.Alias)
			if ref.Alias == "" {
				return fmt.Errorf("key %q alias ref %d: alias name is required", key.ID, j)
			}
			lk := strings.ToLower(ref.Alias)
			if _, exists := aliasMap[lk]; !exists {
				return fmt.Errorf("key %q references unknown alias %q", key.ID, ref.Alias)
			}
			// Self-heal duplicate refs (from a pre-fix migration bug): keep the
			// first, drop later duplicates, no error.
			if firstIdx, dupLast := refSeen[lk]; dupLast {
				// Preserve price overrides from the later ref if they exist.
				if refs[firstIdx].InputPricePerMillion == nil && ref.InputPricePerMillion != nil {
					refs[firstIdx].InputPricePerMillion = ref.InputPricePerMillion
				}
				if refs[firstIdx].OutputPricePerMillion == nil && ref.OutputPricePerMillion != nil {
					refs[firstIdx].OutputPricePerMillion = ref.OutputPricePerMillion
				}
				if refs[firstIdx].CacheReadPricePerMillion == nil && ref.CacheReadPricePerMillion != nil {
					refs[firstIdx].CacheReadPricePerMillion = ref.CacheReadPricePerMillion
				}
				if refs[firstIdx].PerCallUSD == nil && ref.PerCallUSD != nil {
					refs[firstIdx].PerCallUSD = ref.PerCallUSD
				}
				continue
			}
			refSeen[lk] = len(refs)
			refs = append(refs, *ref)
		}
		key.Aliases = refs
		for k := range key.Aliases {
			ref := &key.Aliases[k]
			if ref.InputPricePerMillion != nil && *ref.InputPricePerMillion < 0 {
				return fmt.Errorf("key %q alias %q input_price override cannot be negative", key.ID, ref.Alias)
			}
			if ref.OutputPricePerMillion != nil && *ref.OutputPricePerMillion < 0 {
				return fmt.Errorf("key %q alias %q output_price override cannot be negative", key.ID, ref.Alias)
			}
			if ref.CacheReadPricePerMillion != nil && *ref.CacheReadPricePerMillion < 0 {
				return fmt.Errorf("key %q alias %q cache_read_price override cannot be negative", key.ID, ref.Alias)
			}
			if ref.PerCallUSD != nil && *ref.PerCallUSD < 0 {
				return fmt.Errorf("key %q alias %q per_call_usd override cannot be negative", key.ID, ref.Alias)
			}
		}
	}

	return nil
}

func ResolveStatePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		path = DefaultConfig().StateFile
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return abs, nil
}

func LoadState(path string) (*State, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var state State
	if err := json.Unmarshal(raw, &state); err != nil {
		return nil, err
	}
	if state.Version == 0 {
		state.Version = 1
	}
	if state.Usage == nil {
		state.Usage = make(map[string]*UsageState)
	}
	// Usage is persisted to a sidecar so periodic flushes never rewrite the
	// management-owned key/alias state. Prefer the sidecar when present.
	if usageRaw, usageErr := os.ReadFile(path + ".usage"); usageErr == nil {
		var usageState struct {
			Usage     map[string]*UsageState `json:"usage"`
			UpdatedAt time.Time              `json:"updated_at"`
		}
		if errUnmarshal := json.Unmarshal(usageRaw, &usageState); errUnmarshal != nil {
			return nil, fmt.Errorf("load usage sidecar: %w", errUnmarshal)
		}
		if usageState.Usage != nil && !usageState.UpdatedAt.Before(state.UpdatedAt) {
			state.Usage = usageState.Usage
		}
	} else if !errors.Is(usageErr, os.ErrNotExist) {
		return nil, usageErr
	}
	return &state, nil
}

// SaveState atomically writes the key list plus usage ledger to the state file.
func SaveState(path string, keys []KeyConfig, usage map[string]*UsageState, aliases []AliasMapping, rules []ClassifyRule) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := acquireStateFileLock(path)
	if err != nil {
		return err
	}
	defer unlock()
	// Models is a DERIVED field (resolved from Aliases × global table via
	// resolveAliasRefsToModels); the canonical source is Aliases. Persisting
	// it would (a) make the on-disk state drift from the live in-memory copy
	// when the global alias table is edited, and (b) re-trigger validation
	// errors on reload — multi-target aliases expand to multiple ModelRules
	// sharing one alias name, which legacy validation could not tolerate.
	// Strip Models before marshalling; Configure repopulates it on load.
	cleanKeys := make([]KeyConfig, len(keys))
	for i := range keys {
		cleanKeys[i] = keys[i]
		cleanKeys[i].Models = nil
	}
	state := State{Version: 1, Keys: cleanKeys, Usage: usage, UpdatedAt: time.Now().UTC(), Aliases: aliases, ClassifyRules: rules}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteStateFile(path, raw)
}

// SaveKeyState transactionally merges one management-owned key into the latest
// on-disk state. This is safe across independently scheduled plugin callbacks:
// unrelated keys written by another callback are preserved.
func SaveKeyState(path string, key KeyConfig, aliases []AliasMapping, rules []ClassifyRule) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	unlock, err := acquireStateFileLock(path)
	if err != nil {
		return err
	}
	defer unlock()
	state, err := LoadState(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if state == nil {
		state = &State{Version: 1, Usage: make(map[string]*UsageState)}
	}
	key.Models = nil
	replaced := false
	for i := range state.Keys {
		if state.Keys[i].ID == key.ID {
			state.Keys[i] = key
			replaced = true
			break
		}
	}
	if !replaced {
		state.Keys = append(state.Keys, key)
	}
	if len(aliases) > 0 {
		state.Aliases = aliases
	}
	if len(rules) > 0 {
		state.ClassifyRules = rules
	}
	state.Version = 1
	state.UpdatedAt = time.Now().UTC()
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteStateFile(path, raw)
}

// SaveUsageOnly writes usage to a dedicated sidecar. Periodic usage flushes
// must never rewrite the management-owned key, alias, or classification state:
// CPA may load multiple isolated plugin instances, and any instance can hold a
// stale in-memory configuration snapshot.
func SaveUsageOnly(path string, usage map[string]*UsageState) error {
	usagePath := path + ".usage"
	if err := os.MkdirAll(filepath.Dir(usagePath), 0o700); err != nil {
		return err
	}
	unlock, err := acquireStateFileLock(usagePath)
	if err != nil {
		return err
	}
	defer unlock()
	state := struct {
		Version   int                    `json:"version"`
		Usage     map[string]*UsageState `json:"usage"`
		UpdatedAt time.Time              `json:"updated_at"`
	}{Version: 1, Usage: usage, UpdatedAt: time.Now().UTC()}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteStateFile(usagePath, raw)
}

func atomicWriteStateFile(path string, raw []byte) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	tempName := temp.Name()
	defer func() { _ = os.Remove(tempName) }()
	if err := temp.Chmod(0o600); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(raw); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempName, path)
}
