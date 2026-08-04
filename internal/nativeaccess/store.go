package nativeaccess

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"
)

const hashPrefix = "sha256:"

type Grant struct {
	Provider         string   `json:"provider"`
	Model            string   `json:"model"`
	Group            string   `json:"group,omitempty"`
	UpstreamPrefix   string   `json:"upstream_prefix,omitempty"`
	AcceptedPrefixes []string `json:"accepted_prefixes,omitempty"`
	AcceptedModels   []string `json:"accepted_models,omitempty"`
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
	Known       bool
	Allowed     bool
	Principal   string
	Provider    string
	Model       string
	TargetModel string
	Group       string
	Reason      string
}

type grantMatch struct {
	grant          Grant
	canonicalModel string
	score          int
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
	stateModTime   int64
	stateSize      int64
	activeByHash   map[string]string
	policiesByHash map[string]Policy
	usageByHash    map[string]*usage
	rpm            map[string][]time.Time
	now            func() time.Time
	dirty          bool
	lastFlush      time.Time
	flushScheduled bool
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
	if err := s.loadStateLocked(); err != nil && !errors.Is(err, os.ErrNotExist) {
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
		grant.Group = strings.TrimSpace(grant.Group)
		grant.UpstreamPrefix = strings.Trim(strings.TrimSpace(grant.UpstreamPrefix), "/")
		prefixes := make([]string, 0, len(grant.AcceptedPrefixes))
		prefixSeen := make(map[string]struct{}, len(grant.AcceptedPrefixes))
		for _, prefix := range grant.AcceptedPrefixes {
			prefix = strings.TrimSpace(prefix)
			if prefix == "" || strings.Contains(prefix, "/") {
				return Policy{}, errors.New("accepted_prefixes must be non-empty client prefixes without '/'")
			}
			lower := strings.ToLower(prefix)
			if _, exists := prefixSeen[lower]; exists {
				continue
			}
			prefixSeen[lower] = struct{}{}
			prefixes = append(prefixes, prefix)
		}
		sort.Strings(prefixes)
		grant.AcceptedPrefixes = prefixes
		models := make([]string, 0, len(grant.AcceptedModels))
		modelSeen := make(map[string]struct{}, len(grant.AcceptedModels))
		for _, acceptedModel := range grant.AcceptedModels {
			acceptedModel = strings.TrimSpace(acceptedModel)
			if acceptedModel == "" || strings.ContainsAny(acceptedModel, "*?") {
				return Policy{}, errors.New("accepted_models must contain exact non-empty model names")
			}
			lower := strings.ToLower(acceptedModel)
			if _, exists := modelSeen[lower]; exists {
				continue
			}
			modelSeen[lower] = struct{}{}
			models = append(models, acceptedModel)
		}
		sort.Strings(models)
		grant.AcceptedModels = models
		if len(grant.AcceptedModels) > 0 && strings.ContainsAny(grant.Model, "*?") {
			return Policy{}, errors.New("accepted_models require an exact canonical model")
		}
		if grant.Provider == "" || grant.Model == "" {
			return Policy{}, errors.New("each grant requires provider and model")
		}
		if grant.Provider == "*" && (grant.Group != "" || grant.UpstreamPrefix != "") {
			return Policy{}, errors.New("group and upstream_prefix require a concrete provider")
		}
		id := grant.Provider + "\x00" + strings.ToLower(grant.Model) + "\x00" +
			strings.ToLower(grant.Group) + "\x00" + strings.ToLower(grant.UpstreamPrefix) +
			"\x00" + strings.ToLower(strings.Join(grant.AcceptedPrefixes, "\x01")) +
			"\x00" + strings.ToLower(strings.Join(grant.AcceptedModels, "\x01"))
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

func wildcardMatch(pattern, value string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	value = strings.ToLower(strings.TrimSpace(value))
	if pattern == "*" {
		return true
	}
	expression := "^" + strings.ReplaceAll(
		strings.ReplaceAll(regexp.QuoteMeta(pattern), `\*`, ".*"),
		`\?`,
		".",
	) + "$"
	matched, err := regexp.MatchString(expression, value)
	return err == nil && matched
}

func matchGrant(grant Grant, requestedModel string) (canonicalModel string, score int, ok bool) {
	requestedModel = strings.TrimSpace(requestedModel)
	canonicalModel = requestedModel
	compatibilityMatch := false
	if !wildcardMatch(grant.Model, canonicalModel) {
		canonicalModel = ""
		for _, acceptedModel := range grant.AcceptedModels {
			if strings.EqualFold(requestedModel, acceptedModel) {
				canonicalModel = grant.Model
				compatibilityMatch = true
				break
			}
		}
		for _, prefix := range grant.AcceptedPrefixes {
			if canonicalModel != "" {
				break
			}
			if len(requestedModel) <= len(prefix) || !strings.EqualFold(requestedModel[:len(prefix)], prefix) {
				continue
			}
			candidate := requestedModel[len(prefix):]
			if wildcardMatch(grant.Model, candidate) {
				canonicalModel = candidate
				compatibilityMatch = true
				break
			}
		}
		if canonicalModel == "" {
			return "", -1, false
		}
	}
	score = modelPatternSpecificity(grant.Model)
	if grant.Provider != "*" {
		score += 2
	}
	// Prefer the canonical unprefixed spelling when two otherwise equivalent
	// grants overlap. Compatibility names remain accepted during migration.
	if !compatibilityMatch {
		score++
	}
	return canonicalModel, score, true
}

func modelPatternSpecificity(pattern string) int {
	pattern = strings.TrimSpace(pattern)
	literalLength := len(strings.NewReplacer("*", "", "?", "").Replace(pattern))
	if !strings.ContainsAny(pattern, "*?") {
		return 10_000 + literalLength*10
	}
	return literalLength * 10
}

func grantScore(grant Grant, model string) int {
	_, score, ok := matchGrant(grant, model)
	if !ok {
		return -1
	}
	return score
}

func targetModel(grant Grant, canonicalModel string) string {
	if grant.UpstreamPrefix == "" {
		return canonicalModel
	}
	return grant.UpstreamPrefix + "/" + canonicalModel
}

func splitNativeModelPrefix(requestedModel string) (prefix, model string, explicit bool) {
	requestedModel = strings.TrimSpace(requestedModel)
	prefix, model, explicit = strings.Cut(requestedModel, "/")
	prefix = strings.TrimSpace(prefix)
	model = strings.TrimSpace(model)
	if !explicit || prefix == "" || model == "" {
		return "", requestedModel, false
	}
	return strings.Trim(prefix, "/"), model, true
}

func matchingGrants(policy Policy, requestedModel string, explicitPrefix string) []grantMatch {
	matches := make([]grantMatch, 0, len(policy.Grants))
	bestScore := -1
	for _, grant := range policy.Grants {
		if explicitPrefix != "" && !strings.EqualFold(grant.UpstreamPrefix, explicitPrefix) {
			continue
		}
		canonicalModel, score, matched := matchGrant(grant, requestedModel)
		if !matched {
			continue
		}
		if score > bestScore {
			matches = matches[:0]
			bestScore = score
		}
		if score == bestScore {
			matches = append(matches, grantMatch{
				grant:          grant,
				canonicalModel: canonicalModel,
				score:          score,
			})
		}
	}
	return matches
}

// routeDecision separates authorization from CPA's own credential scheduler.
//
//   - An explicit native "prefix/model" request must match a grant carrying
//     exactly that upstream_prefix.
//   - A provider-wide grant (no group/prefix) delegates credential selection
//     to CPA for that provider.
//   - A global "* / model" grant delegates provider and credential selection
//     to CPA unchanged.
//   - One account-scoped grant is safely normalized to its native prefix.
//   - Multiple account-scoped grants are ambiguous without an explicit prefix
//     and fail closed instead of selecting the first sorted grant.
func routeDecision(policy Policy, requestedModel string) Decision {
	explicitPrefix, baseModel, explicit := splitNativeModelPrefix(requestedModel)
	matches := matchingGrants(policy, baseModel, explicitPrefix)
	if len(matches) == 0 {
		return Decision{Model: baseModel, Reason: "model_not_allowed"}
	}

	if explicit {
		selected := matches[0]
		return Decision{
			Allowed:     true,
			Provider:    selected.grant.Provider,
			Model:       selected.canonicalModel,
			TargetModel: targetModel(selected.grant, selected.canonicalModel),
			Group:       selected.grant.Group,
			Reason:      "allowed_explicit_upstream",
		}
	}

	for _, match := range matches {
		if match.grant.Provider == "*" && match.grant.Group == "" && match.grant.UpstreamPrefix == "" {
			return Decision{
				Allowed:     true,
				Provider:    "*",
				Model:       match.canonicalModel,
				TargetModel: match.canonicalModel,
				Reason:      "allowed_all_upstreams",
			}
		}
	}

	providerWide := make(map[string]grantMatch)
	accountScoped := make([]grantMatch, 0, len(matches))
	for _, match := range matches {
		if match.grant.Group == "" && match.grant.UpstreamPrefix == "" {
			providerWide[match.grant.Provider] = match
			continue
		}
		accountScoped = append(accountScoped, match)
	}
	if len(providerWide) == 1 {
		for _, selected := range providerWide {
			return Decision{
				Allowed:     true,
				Provider:    selected.grant.Provider,
				Model:       selected.canonicalModel,
				TargetModel: selected.canonicalModel,
				Reason:      "allowed_provider_scheduler",
			}
		}
	}
	if len(providerWide) > 1 {
		return Decision{Model: baseModel, Reason: "ambiguous_upstream_prefix_required"}
	}

	if len(accountScoped) == 1 {
		selected := accountScoped[0]
		if selected.grant.UpstreamPrefix == "" {
			return Decision{Model: baseModel, Reason: "upstream_prefix_required"}
		}
		return Decision{
			Allowed:     true,
			Provider:    selected.grant.Provider,
			Model:       selected.canonicalModel,
			TargetModel: targetModel(selected.grant, selected.canonicalModel),
			Group:       selected.grant.Group,
			Reason:      "allowed_unique_upstream",
		}
	}
	return Decision{Model: baseModel, Reason: "ambiguous_upstream_prefix_required"}
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

func (s *Store) loadStateLocked() error {
	raw, err := os.ReadFile(s.stateFile)
	if err != nil {
		return err
	}
	var data state
	if err := json.Unmarshal(raw, &data); err != nil {
		return fmt.Errorf("parse native policy state: %w", err)
	}
	nextPolicies := make(map[string]Policy, len(data.Policies))
	for _, input := range data.Policies {
		policy, err := normalizePolicy(input)
		if err != nil {
			return err
		}
		nextPolicies[policy.KeyHash] = policy
	}
	s.policiesByHash = nextPolicies
	if data.Usage != nil {
		mergeUsageMaps(s.usageByHash, data.Usage)
	}
	if info, statErr := os.Stat(s.stateFile); statErr == nil {
		s.stateModTime = info.ModTime().UnixNano()
		s.stateSize = info.Size()
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
	if err := os.Rename(temp, s.stateFile); err != nil {
		return err
	}
	if info, statErr := os.Stat(s.stateFile); statErr == nil {
		s.stateModTime = info.ModTime().UnixNano()
		s.stateSize = info.Size()
	}
	s.dirty = false
	s.lastFlush = s.now()
	return nil
}

func (s *Store) refreshStateLocked(force bool) error {
	info, err := os.Stat(s.stateFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if !force && s.stateModTime == info.ModTime().UnixNano() && s.stateSize == info.Size() {
		return nil
	}
	return s.loadStateLocked()
}

func mergeUsageMaps(current, incoming map[string]*usage) {
	for hash, source := range incoming {
		if source == nil {
			continue
		}
		target := current[hash]
		if target == nil {
			copyUsage := *source
			current[hash] = &copyUsage
			continue
		}
		mergeWindowMax(&target.Daily, source.Daily)
		mergeWindowMax(&target.Weekly, source.Weekly)
	}
}

func mergeWindowMax(target *window, source window) {
	if source.Start.After(target.Start) {
		*target = source
		return
	}
	if source.Start.Equal(target.Start) {
		if source.Calls > target.Calls {
			target.Calls = source.Calls
		}
		if source.Tokens > target.Tokens {
			target.Tokens = source.Tokens
		}
	}
}

func acquireFileLock(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, err
	}
	lockPath := path + ".lock"
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
			return nil, fmt.Errorf("timed out waiting for state lock")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (s *Store) refreshState() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.refreshStateLocked(false)
}

func (s *Store) Identities() ([]Identity, error) {
	if err := s.refreshKeys(); err != nil {
		return nil, err
	}
	if err := s.refreshState(); err != nil {
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
	_ = s.refreshState()
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
	release, err := acquireFileLock(s.stateFile)
	if err != nil {
		return nil, err
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshStateLocked(true); err != nil {
		return nil, err
	}
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
	release, err := acquireFileLock(s.stateFile)
	if err != nil {
		return err
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.refreshStateLocked(true); err != nil {
		return err
	}
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
	if err := s.refreshState(); err != nil {
		return Decision{Reason: "policy_state_unavailable"}
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
	route := routeDecision(policy, model)
	if !route.Allowed {
		decision.Model = route.Model
		decision.Reason = route.Reason
		return decision
	}
	decision.Provider = route.Provider
	decision.Model = route.Model
	decision.TargetModel = route.TargetModel
	decision.Group = route.Group
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
	s.dirty = true
	decision.Allowed = true
	decision.Reason = route.Reason
	return decision
}

// Route returns the provider, canonical model, upstream-prefixed model, and
// optional credential group for an already-authorized request. Compatibility
// client prefixes are stripped before matching; UpstreamPrefix is then applied
// using CPA's native "prefix/model" syntax.
func (s *Store) Route(rawKey, model string) (Decision, bool) {
	if err := s.refreshKeys(); err != nil {
		return Decision{}, false
	}
	if err := s.refreshState(); err != nil {
		return Decision{}, false
	}
	hash := HashKey(rawKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, active := s.activeByHash[hash]; !active {
		return Decision{}, false
	}
	policy, exists := s.policiesByHash[hash]
	if !exists || !policy.Enabled {
		return Decision{}, false
	}
	decision := routeDecision(policy, model)
	decision.Known = true
	decision.Principal = hash
	return decision, decision.Allowed
}

// RouteProvider is retained for API compatibility with earlier V2 callers.
func (s *Store) RouteProvider(rawKey, model string) (string, bool) {
	decision, ok := s.Route(rawKey, model)
	return decision.Provider, ok
}

// FilterModels returns only model entries matched by at least one grant for
// the active native key. Provider constraints are enforced at routing time;
// the OpenAI model catalog itself is keyed by model ID.
func (s *Store) FilterModels(rawKey string, models []map[string]any, modelProviders map[string][]string) ([]map[string]any, bool) {
	if err := s.refreshKeys(); err != nil {
		return nil, false
	}
	if err := s.refreshState(); err != nil {
		return nil, false
	}
	hash := HashKey(rawKey)
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, active := s.activeByHash[hash]; !active {
		return nil, false
	}
	policy, exists := s.policiesByHash[hash]
	if !exists || !policy.Enabled {
		return []map[string]any{}, true
	}
	filtered := make([]map[string]any, 0, len(models))
	seen := make(map[string]struct{}, len(models))
	appendModel := func(source map[string]any, id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		key := strings.ToLower(id)
		if _, exists := seen[key]; exists {
			return
		}
		seen[key] = struct{}{}
		clone := make(map[string]any, len(source))
		for field, value := range source {
			clone[field] = value
		}
		clone["id"] = id
		filtered = append(filtered, clone)
	}
	for _, model := range models {
		id, _ := model["id"].(string)
		for _, grant := range policy.Grants {
			if !providerMatches(grant.Provider, modelProviders[id]) {
				continue
			}
			canonicalID := id
			if grant.UpstreamPrefix != "" {
				nativePrefix := grant.UpstreamPrefix + "/"
				if len(id) <= len(nativePrefix) || !strings.EqualFold(id[:len(nativePrefix)], nativePrefix) {
					continue
				}
				canonicalID = id[len(nativePrefix):]
			}
			if !wildcardMatch(grant.Model, canonicalID) {
				continue
			}
			appendModel(model, canonicalID)
			if grant.UpstreamPrefix != "" {
				appendModel(model, grant.UpstreamPrefix+"/"+canonicalID)
			}
			for _, acceptedModel := range grant.AcceptedModels {
				appendModel(model, acceptedModel)
			}
			for _, prefix := range grant.AcceptedPrefixes {
				appendModel(model, prefix+canonicalID)
			}
		}
	}
	return filtered, true
}

func providerMatches(pattern string, providers []string) bool {
	if strings.TrimSpace(pattern) == "*" {
		return true
	}
	for _, provider := range providers {
		if wildcardMatch(pattern, provider) {
			return true
		}
	}
	return false
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
	s.dirty = true
	if !s.flushScheduled && (s.lastFlush.IsZero() || now.Sub(s.lastFlush) >= 15*time.Second) {
		s.flushScheduled = true
		go s.Flush()
	}
}

// Flush persists accumulated usage without rewriting on every request.
func (s *Store) Flush() {
	release, err := acquireFileLock(s.stateFile)
	if err != nil {
		s.mu.Lock()
		s.flushScheduled = false
		s.mu.Unlock()
		return
	}
	defer release()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.flushScheduled = false
	if !s.dirty {
		return
	}
	_ = s.refreshStateLocked(true)
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
