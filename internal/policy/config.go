package policy

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Enabled   bool        `yaml:"enabled" json:"enabled"`
	StateFile string      `yaml:"state_file" json:"state_file"`
	Keys      []KeyConfig `yaml:"keys" json:"keys"`
}

type KeyConfig struct {
	ID         string      `yaml:"id" json:"id"`
	Name       string      `yaml:"name" json:"name"`
	Enabled    bool        `yaml:"enabled" json:"enabled"`
	KeyHash    string      `yaml:"key_hash" json:"key_hash"`
	KeyPreview string      `yaml:"key_preview" json:"key_preview"`
	RPM        int         `yaml:"rpm" json:"rpm"`
	Models     []ModelRule `yaml:"models" json:"models"`
	// DailyLimitUSD caps the dollar usage per UTC calendar day. 0 = unlimited.
	DailyLimitUSD float64 `yaml:"daily_limit_usd,omitempty" json:"daily_limit_usd,omitempty"`
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
	CacheReadPricePerMillion float64 `yaml:"cache_read_price_per_million,omitempty" json:"cache_read_price_per_million,omitempty"`
}

// UsageState holds per-key dollar usage accounting persisted in the state JSON.
// It is keyed by alias for breakdown, plus rolling daily/weekly windows.
type UsageState struct {
	Daily   UsageWindow            `json:"daily"`
	Weekly  UsageWindow            `json:"weekly"`
	ByAlias map[string]UsageWindow `json:"by_alias,omitempty"`
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
}

type State struct {
	Version   int                    `json:"version"`
	Keys      []KeyConfig            `json:"keys"`
	Usage     map[string]*UsageState `json:"usage,omitempty"`
	UpdatedAt time.Time              `json:"updated_at"`
}

func DefaultConfig() Config {
	return Config{
		Enabled:   true,
		StateFile: "cpa-key-policy-state.json",
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
	if err := normalizeConfig(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func normalizeConfig(cfg *Config) error {
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
		aliases := map[string]struct{}{}
		for j := range key.Models {
			model := &key.Models[j]
			model.Alias = strings.TrimSpace(model.Alias)
			model.Provider = strings.ToLower(strings.TrimSpace(model.Provider))
			model.TargetModel = strings.TrimSpace(model.TargetModel)
			model.Group = strings.ToLower(strings.TrimSpace(model.Group))
			if model.Alias == "" || model.Provider == "" || model.TargetModel == "" {
				return fmt.Errorf("key %q model entries require alias, provider, and target_model", key.ID)
			}
			aliasKey := strings.ToLower(model.Alias)
			if _, exists := aliases[aliasKey]; exists {
				return fmt.Errorf("key %q has duplicate model alias %q", key.ID, model.Alias)
			}
			aliases[aliasKey] = struct{}{}
			if model.InputPricePerMillion < 0 || model.OutputPricePerMillion < 0 || model.CacheReadPricePerMillion < 0 {
				return fmt.Errorf("key %q model %q prices cannot be negative", key.ID, model.Alias)
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
	return &state, nil
}

// SaveState atomically writes the key list plus usage ledger to the state file.
func SaveState(path string, keys []KeyConfig, usage map[string]*UsageState) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	state := State{Version: 1, Keys: keys, Usage: usage, UpdatedAt: time.Now().UTC()}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
