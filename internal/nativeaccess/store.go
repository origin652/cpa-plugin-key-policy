package nativeaccess

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const hashPrefix = "sha256:"

type Grant struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

type Policy struct {
	KeyHash      string  `json:"key_hash"`
	Enabled      bool    `json:"enabled"`
	Grants       []Grant `json:"grants"`
	RPM          int     `json:"rpm,omitempty"`
	DailyCalls   int64   `json:"daily_calls,omitempty"`
	WeeklyCalls  int64   `json:"weekly_calls,omitempty"`
	DailyTokens  int64   `json:"daily_tokens,omitempty"`
	WeeklyTokens int64   `json:"weekly_tokens,omitempty"`
}

type window struct {
	Start  time.Time `json:"start"`
	Calls  int64     `json:"calls"`
	Tokens int64     `json:"tokens"`
}

type usage struct {
	Daily  window `json:"daily"`
	Weekly window `json:"weekly"`
}

type state struct {
	Version  int               `json:"version"`
	Policies []Policy          `json:"policies"`
	Usage    map[string]*usage `json:"usage,omitempty"`
}

type Identity struct {
	KeyHash string `json:"key_hash"`
	Preview string `json:"key_preview"`
	Managed bool   `json:"managed"`
}

type Decision struct {
	Known     bool
	Allowed   bool
	Principal string
	Provider  string
	Model     string
	Reason    string
}

type nativeConfig struct {
	APIKeys []string `yaml:"api-keys"`
}

type Store struct {
	mu             sync.Mutex
	keysFile       string
	stateFile      string
	keysModTime    int64
	keysSize       int64
	activeByHash   map[string]string
	policiesByHash map[string]Policy
	usageByHash    map[string]*usage
	rpm            map[string][]time.Time
	now            func() time.Time
}

func New(keysFile, stateFile string) (*Store, error) {
	keysFile, err := absolutePath(keysFile)
	if err != nil {
		return nil, err
	}
	stateFile, err = absolutePath(stateFile)
	if err != nil {
		return nil, err
	}
	s := &Store{
		keysFile:       keysFile,
		stateFile:      stateFile,
		activeByHash:   make(map[string]string),
		policiesByHash: make(map[string]Policy),
		usageByHash:    make(map[string]*usage),
		rpm:            make(map[string][]time.Time),
		now:            time.Now,
	}
	if err := s.loadState(); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := s.refreshKeysLocked(true); err != nil {
		return nil, err
	}
	return s, nil
}

func absolutePath(path string) (string, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	return filepath.Abs(path)
}

func HashKey(key string) string {
	sum := sha256.Sum256([]byte(strings.TrimSpace(key)))
	return hashPrefix + hex.EncodeToString(sum[:])
}

func PreviewKey(key string) string {
	key = strings.TrimSpace(key)
	if len(key) <= 12 {
		return key
	}
	return key[:7] + "..." + key[len(key)-5:]
}

func normalizePolicy(input Policy) (Policy, error) {
	input.KeyHash = strings.ToLower(strings.TrimSpace(input.KeyHash))
	if !strings.HasPrefix(input.KeyHash, hashPrefix) || len(input.KeyHash) != len(hashPrefix)+64 {
		return Policy{}, errors.New("key_hash must be a sha256 hash")
	}
	if input.RPM < 0 || input.DailyCalls < 0 || input.WeeklyCalls < 0 ||
		input.DailyTokens < 0 || input.WeeklyTokens < 0 {
		return Policy{}, errors.New("quotas cannot be negative")
	}
	seen := make(map[string]struct{}, len(input.Grants))
	grants := make([]Grant, 0, len(input.Grants))
	for _, grant := range input.Grants {
		grant.Provider = strings.ToLower(strings.TrimSpace(grant.Provider))
		grant.Model = strings.TrimSpace(grant.Model)
		if grant.Provider == "" || grant.Model == "" {
			return Policy{}, errors.New("each grant requires provider and model")
		}
		id := grant.Provider + "\x00" + strings.ToLower(grant.Model)
		if _, exists := seen[id]; exists {
			continue
		}
		seen[id] = struct{}{}
		grants = append(grants, grant)
	}
	sort.Slice(grants, func(i, j int) bool {
		if grants[i].Provider == grants[j].Provider {
			return grants[i].Model < grants[j].Model
		}
		return grants[i].Provider < grants[j].Provider
	})
	input.Grants = grants
	return input, nil
}

func (s *Store) refreshKeysLocked(force bool) error {
	info, err := os.Stat(s.keysFile)
	if err != nil {
		return fmt.Errorf("read native CPA keys: %w", err)
	}
	if !force && s.keysModTime == info.ModTime().UnixNano() && s.keysSize == info.Size() {
		return nil
	}
	raw, err := os.ReadFile(s.keysFile)
	if err != nil {
		return fmt.Errorf("read native CPA keys: %w", err)
	}
	var cfg nativeConfig
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return fmt.Errorf("parse native CPA keys: %w", err)
	}
	next := make(map[string]string, len(cfg.APIKeys))
	for _, key := range cfg.APIKeys {
		key = strings.TrimSpace(key)
		if key != "" {
			next[HashKey(key)] = PreviewKey(key)
		}
	}
	s.activeByHash = next
	s.keysModTime = info.ModTime().UnixNano()
	s.keysSize = info.Size()
	return nil
}

func (s *Store) refreshKeys() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshKeysLocked(false)
}

func (s *Store) loadState() error {
	raw, err := os.ReadFile(s.stateFile)
	if err != nil {
		return err
	}
	var data state
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("parse native policy state: %w", err)
	}
	for _, input := range data.Policies {
		policy, err := normalizePolicy(input)
		if err != nil {
			return err
		}
		s.policiesByHash[policy.KeyHash] = policy
	}
	if data.Usage != nil {
		s.usageByHash = data.Usage
	}
	return nil
}

func (s *Store) saveLocked() error {
	policies := make([]Policy, 0, len(s.policiesByHash))
	for _, policy := range s.policiesByHash {
		policies = append(policies, policy)
	}
	sort.Slice(policies, func(i, j int) bool { return policies[i].KeyHash < policies[j].KeyHash })
	raw, err := json.MarshalIndent(state{Version: 1, Policies: policies, Usage: s.usageByHash}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.stateFile), 0o750); err != nil {
		return err
	}
	temp := s.stateFile + ".tmp"
	if err := os.WriteFile(temp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(temp, s.stateFile)
}

func (s *Store) Identities() ([]Identity, error) {
	if err := s.refreshKeys(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Identity, 0, len(s.activeByHash))
	for hash, preview := range s.activeByHash {
		_, managed := s.policiesByHash[hash]
		out = append(out, Identity{KeyHash: hash, Preview: preview, Managed: managed})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].KeyHash < out[j].KeyHash })
	return out, nil
}

func (s *Store) Policies() []Policy {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Policy, 0, len(s.policiesByHash))
	for _, policy := range s.policiesByHash {
		out = append(out, policy)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].KeyHash < out[j].KeyHash })
	return out
}

func (s *Store) Upsert(input Policy) error {
	_, err := s.Apply([]Policy{input}, false, false)
	return err
}

// Apply validates the complete input before changing state. It is the common
// write path for both the CPAMP UI and declarative/AI automation.
//
// replace=false merges the supplied policies into existing state.
// replace=true makes the supplied list the complete desired policy set.
// dryRun=true performs all validation but does not mutate or persist anything.
func (s *Store) Apply(inputs []Policy, replace, dryRun bool) ([]Policy, error) {
	normalized := make([]Policy, 0, len(inputs))
	seen := make(map[string]struct{}, len(inputs))
	for _, input := range inputs {
		policy, err := normalizePolicy(input)
		if err != nil {
			return nil, err
		}
		if _, duplicate := seen[policy.KeyHash]; duplicate {
			return nil, fmt.Errorf("duplicate policy for %s", policy.KeyHash)
		}
		seen[policy.KeyHash] = struct{}{}
		normalized = append(normalized, policy)
	}
	if err := s.refreshKeys(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, policy := range normalized {
		if _, active := s.activeByHash[policy.KeyHash]; !active {
			return nil, fmt.Errorf("%s is not an active CPA api-key", policy.KeyHash)
		}
	}
	if dryRun {
		return normalized, nil
	}
	next := s.policiesByHash
	if replace {
		next = make(map[string]Policy, len(normalized))
	} else {
		next = make(map[string]Policy, len(s.policiesByHash)+len(normalized))
		for hash, policy := range s.policiesByHash {
			next[hash] = policy
		}
	}
	for _, policy := range normalized {
		next[policy.KeyHash] = policy
	}
	previous := s.policiesByHash
	s.policiesByHash = next
	if err := s.saveLocked(); err != nil {
		s.policiesByHash = previous
		return nil, err
	}
	return normalized, nil
}

func (s *Store) Delete(hash string) error {
	hash = strings.ToLower(strings.TrimSpace(hash))
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.policiesByHash, hash)
	delete(s.usageByHash, hash)
	delete(s.rpm, hash)
	return s.saveLocked()
}

func resetWindows(u *usage, now time.Time) {
	day := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	if u.Daily.Start.IsZero() || u.Daily.Start.Before(day) {
		u.Daily = window{Start: day}
	}
	week := day.AddDate(0, 0, -6)
	if u.Weekly.Start.IsZero() || u.Weekly.Start.Before(week) {
		u.Weekly = window{Start: now}
	}
}

func (s *Store) Authenticate(rawKey, model string, modelsEndpoint bool) Decision {
	if err := s.refreshKeys(); err != nil {
		return Decision{Reason: "native_keys_unavailable"}
	}
	hash := HashKey(rawKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, active := s.activeByHash[hash]; !active {
		return Decision{Reason: "unknown_native_key"}
	}
	decision := Decision{Known: true, Principal: hash, Model: model}
	policy, exists := s.policiesByHash[hash]
	if !exists {
		decision.Reason = "policy_missing"
		return decision
	}
	if !policy.Enabled {
		decision.Reason = "policy_disabled"
		return decision
	}
	if modelsEndpoint {
		decision.Allowed = true
		decision.Reason = "models_endpoint_allowed"
		return decision
	}
	for _, grant := range policy.Grants {
		if strings.EqualFold(grant.Model, model) {
			decision.Provider = grant.Provider
			break
		}
	}
	if decision.Provider == "" {
		decision.Reason = "model_not_allowed"
		return decision
	}
	now := s.now()
	u := s.usageByHash[hash]
	if u == nil {
		u = &usage{}
		s.usageByHash[hash] = u
	}
	resetWindows(u, now)
	if policy.DailyCalls > 0 && u.Daily.Calls >= policy.DailyCalls {
		decision.Reason = "daily_calls_exceeded"
		return decision
	}
	if policy.WeeklyCalls > 0 && u.Weekly.Calls >= policy.WeeklyCalls {
		decision.Reason = "weekly_calls_exceeded"
		return decision
	}
	if policy.DailyTokens > 0 && u.Daily.Tokens >= policy.DailyTokens {
		decision.Reason = "daily_tokens_exceeded"
		return decision
	}
	if policy.WeeklyTokens > 0 && u.Weekly.Tokens >= policy.WeeklyTokens {
		decision.Reason = "weekly_tokens_exceeded"
		return decision
	}
	if policy.RPM > 0 {
		cutoff := now.Add(-time.Minute)
		recent := s.rpm[hash][:0]
		for _, at := range s.rpm[hash] {
			if at.After(cutoff) {
				recent = append(recent, at)
			}
		}
		if len(recent) >= policy.RPM {
			s.rpm[hash] = recent
			decision.Reason = "rpm_exceeded"
			return decision
		}
		s.rpm[hash] = append(recent, now)
	}
	u.Daily.Calls++
	u.Weekly.Calls++
	decision.Allowed = true
	decision.Reason = "allowed"
	return decision
}

func (s *Store) RecordUsage(rawKey string, tokens int64) {
	if strings.TrimSpace(rawKey) == "" || tokens <= 0 {
		return
	}
	hash := HashKey(rawKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, active := s.activeByHash[hash]; !active {
		return
	}
	now := s.now()
	u := s.usageByHash[hash]
	if u == nil {
		u = &usage{}
		s.usageByHash[hash] = u
	}
	resetWindows(u, now)
	u.Daily.Tokens += tokens
	u.Weekly.Tokens += tokens
	_ = s.saveLocked()
}

func (s *Store) Status() map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	managedActive := 0
	for hash := range s.activeByHash {
		if _, ok := s.policiesByHash[hash]; ok {
			managedActive++
		}
	}
	return map[string]any{
		"mode":            "native-access",
		"native_keys":     len(s.activeByHash),
		"managed_active":  managedActive,
		"orphan_policies": len(s.policiesByHash) - managedActive,
		"keys_file":       s.keysFile,
	}
}
