package policy

import (
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) (*Store, string) {
	t.Helper()
	plain := "cpa_test_key"
	hash, err := HashKey(plain)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	err = store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{
			{
				ID:         "team-a",
				Name:       "Team A",
				Enabled:    true,
				KeyHash:    hash,
				KeyPreview: PreviewKey(plain),
				RPM:        1,
				Models: []ModelRule{
					{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, plain
}

func TestStoreAuthenticateUnknownKeyFallsThrough(t *testing.T) {
	store, _ := newTestStore(t)
	decision := store.Authenticate("POST", "/v1/chat/completions", http.Header{"Authorization": {"Bearer other"}}, nil, []byte(`{"model":"fast"}`))
	if decision.Known || decision.Allowed {
		t.Fatalf("decision = %+v, want unknown fallthrough", decision)
	}
}

func TestStoreAuthenticateAllowedAndRoute(t *testing.T) {
	store, plain := newTestStore(t)
	headers := http.Header{"Authorization": {"Bearer " + plain}}
	decision := store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"fast"}`))
	if !decision.Known || !decision.Allowed || decision.Rule.TargetModel != "gpt-5-codex" {
		t.Fatalf("decision = %+v, want allowed", decision)
	}
	rule, keyID, ok := store.Route(headers, nil, "fast")
	if !ok || keyID != "team-a" || rule.Provider != "codex" {
		t.Fatalf("Route() = %+v, %q, %v", rule, keyID, ok)
	}
}

func TestStoreAuthenticateRejectsUnauthorizedModel(t *testing.T) {
	store, plain := newTestStore(t)
	decision := store.Authenticate("POST", "/v1/chat/completions", http.Header{"Authorization": {"Bearer " + plain}}, nil, []byte(`{"model":"slow"}`))
	if !decision.Known || decision.Allowed || decision.Reason != "model_not_allowed" {
		t.Fatalf("decision = %+v, want model_not_allowed", decision)
	}
}

func TestStoreAuthenticateRejectsModelsEndpoint(t *testing.T) {
	store, plain := newTestStore(t)
	decision := store.Authenticate("GET", "/v1/models", http.Header{"Authorization": {"Bearer " + plain}}, nil, nil)
	if !decision.Known || decision.Allowed || decision.Reason != "models_endpoint_disabled" {
		t.Fatalf("decision = %+v, want models endpoint denied", decision)
	}
}

func TestStoreAuthenticateRateLimits(t *testing.T) {
	store, plain := newTestStore(t)
	headers := http.Header{"Authorization": {"Bearer " + plain}}
	_ = store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"fast"}`))
	decision := store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"fast"}`))
	if !decision.RateLimited || decision.Allowed {
		t.Fatalf("decision = %+v, want rate limited", decision)
	}
}

// perCallImageStore builds a store with one per_call-billed image alias, used
// to exercise the access-time pre-charge for image/video endpoints.
func perCallImageStore(t *testing.T) (*Store, string) {
	t.Helper()
	plain := "cpa_image_key"
	hash, err := HashKey(plain)
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	err = store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{
			{
				ID:      "img-team",
				Name:    "Image Team",
				Enabled: true,
				KeyHash: hash,
				Models: []ModelRule{
					{Alias: "grok-imagine-image-quality", Provider: "xai", TargetModel: "grok-imagine-image-quality", BillingMode: "per_call", PerCallUSD: 2},
					{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex"},
				},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, plain
}

// imgTeamKey returns the configured KeyConfig for the per_call image test key.
func imgTeamKey(store *Store) KeyConfig {
	k := store.findByID("img-team")
	if k == nil {
		panic("img-team key not found")
	}
	return *k
}

func TestAuthenticatePerCallImagePreCharged(t *testing.T) {
	store, plain := perCallImageStore(t)
	headers := http.Header{"Authorization": {"Bearer " + plain}}
	decision := store.Authenticate("POST", "/v1/images/generations", headers, nil, []byte(`{"model":"grok-imagine-image-quality","prompt":"a boat"}`))
	if !decision.Allowed || !decision.PreCharged {
		t.Fatalf("decision = %+v, want Allowed+PreCharged", decision)
	}
	sum := store.UsageSummaryFor(imgTeamKey(store))
	if sum.DailyUSD != 2 || sum.DailyCallCount != 1 {
		t.Fatalf("summary = %+v, want DailyUSD=2, DailyCallCount=1", sum)
	}
}

func TestAuthenticatePerCallVideoPreCharged(t *testing.T) {
	store, plain := perCallImageStore(t)
	headers := http.Header{"Authorization": {"Bearer " + plain}}
	body := []byte(`{"model":"grok-imagine-image-quality","prompt":"a clip"}`)
	// Path-parameter video subresource (/v1/videos/<id>) must also pre-charge.
	decision := store.Authenticate("GET", "/v1/videos/req_123", headers, nil, body)
	if !decision.Allowed || !decision.PreCharged {
		t.Fatalf("decision = %+v, want Allowed+PreCharged on video subresource", decision)
	}
	sum := store.UsageSummaryFor(imgTeamKey(store))
	if sum.DailyUSD != 2 {
		t.Fatalf("summary.DailyUSD = %v, want 2", sum.DailyUSD)
	}
}

func TestAuthenticatePerCallChatNotPreCharged(t *testing.T) {
	store, plain := perCallImageStore(t)
	headers := http.Header{"Authorization": {"Bearer " + plain}}
	// Same per_call alias, but on a chat endpoint — must NOT pre-charge. Chat
	// is billed via usage.handle (CPA emits a record there), and pre-charging
	// would double-bill.
	decision := store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"grok-imagine-image-quality"}`))
	if !decision.Allowed || decision.PreCharged {
		t.Fatalf("decision = %+v, want Allowed and NOT PreCharged on chat path", decision)
	}
	sum := store.UsageSummaryFor(imgTeamKey(store))
	if sum.DailyUSD != 0 {
		t.Fatalf("summary.DailyUSD = %v, want 0 (chat not pre-charged)", sum.DailyUSD)
	}
}

func TestAuthenticateTokenModeImageNotPreCharged(t *testing.T) {
	store, plain := perCallImageStore(t)
	headers := http.Header{"Authorization": {"Bearer " + plain}}
	// Image endpoint, but the alias is token-billed ("fast") — pre-charge only
	// applies to per_call aliases. Token-mode images would be billed by tokens
	// if CPA reported usage, and pre-charging a fixed USD would be wrong.
	decision := store.Authenticate("POST", "/v1/images/generations", headers, nil, []byte(`{"model":"fast","prompt":"x"}`))
	if !decision.Allowed || decision.PreCharged {
		t.Fatalf("decision = %+v, want Allowed and NOT PreCharged for token-mode alias", decision)
	}
	sum := store.UsageSummaryFor(imgTeamKey(store))
	if sum.DailyUSD != 0 {
		t.Fatalf("summary.DailyUSD = %v, want 0 (token mode not pre-charged)", sum.DailyUSD)
	}
}

func TestIsImageVideoEndpoint(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/v1/images/generations", true},
		{"/v1/images/edits", true},
		{"/openai/v1/images/generations", true},
		{"/v1/videos", true},
		{"/v1/videos/generations", true},
		{"/v1/videos/req_abc", true},
		{"/openai/v1/videos/extensions", true},
		{"/v1/chat/completions", false},
		{"/v1/models", false},
		{"/v1/responses", false},
		{"", false},
	}
	for _, c := range cases {
		if got := IsImageVideoEndpoint(c.path); got != c.want {
			t.Errorf("IsImageVideoEndpoint(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

// TestConfigureDoesNotResurrectKeysMissingFromState verifies that persisted
// state remains authoritative during reconfigure. Retaining an in-memory key
// that was removed from state would keep a revoked credential usable.
func TestConfigureDoesNotResurrectKeysMissingFromState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	onDiskHash, err := HashKey("cpa_on_disk")
	if err != nil {
		t.Fatal(err)
	}
	revokedHash, err := HashKey("cpa_revoked")
	if err != nil {
		t.Fatal(err)
	}
	// Seed initial state with one key on disk.
	s1 := NewStore()
	if err := s1.Configure(Config{Enabled: true, StateFile: path, Keys: []KeyConfig{
		{ID: "on-disk", Enabled: true, KeyHash: onDiskHash, Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	// Add a second key via the management API (persisted to disk).
	if err := s1.UpsertKey(KeyConfig{ID: "in-mem", Enabled: true, KeyHash: revokedHash, Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex"}}}, true); err != nil {
		t.Fatal(err)
	}
	// Simulate a stale disk snapshot: write a state containing only "on-disk".
	if err := SaveState(path, []KeyConfig{{ID: "on-disk", Enabled: true, KeyHash: onDiskHash, Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex"}}}}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	// Reconfigure with the same path. The persisted state lacks "in-mem", so it
	// must be removed from the active authentication index.
	if err := s1.Configure(Config{Enabled: true, StateFile: path}); err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, k := range s1.Keys() {
		ids[k.ID] = true
	}
	if !ids["on-disk"] || ids["in-mem"] {
		t.Fatalf("after reconfigure, keys = %v, want only on-disk", ids)
	}
	if s1.FindByAPIKey("cpa_revoked") != nil {
		t.Fatal("credential removed from persisted state remained authenticatable")
	}
}

// TestConfigureFlushesBeforeReload (Bug 2): a reconfigure must flush any
// un-persisted in-memory usage to the OLD state path before loading, so a
// pending usage change is not lost when the disk snapshot is stale. We verify
// by recording usage, NOT calling FlushUsage, then reconfiguring with the same
// path: the usage must survive because Configure flushes first.
func TestConfigureFlushesBeforeReload(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	hash, err := HashKey("cpa_flush")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore()
	s.SetClock(func() time.Time { return now })
	if err := s.Configure(Config{Enabled: true, StateFile: path, Keys: []KeyConfig{
		{ID: "k", Enabled: true, KeyHash: hash, Models: []ModelRule{{Alias: "fast", Provider: "openai", TargetModel: "m", InputPricePerMillion: 3, OutputPricePerMillion: 15}}},
	}}); err != nil {
		t.Fatal(err)
	}
	s.StartUsageFlusher()
	// Record usage but do NOT flush manually. Without the Bug 2 fix, this
	// in-memory usage has never been written to disk, so a reconfigure that
	// LoadState's the (empty-usage) disk would lose it.
	_ = s.RecordUsage("k", "fast", "m", false, UsageDetail{InputTokens: 1_000_000, OutputTokens: 500_000})
	// Reconfigure with the same path. Bug 2 fix: Configure flushes first.
	if err := s.Configure(Config{Enabled: true, StateFile: path, Keys: []KeyConfig{
		{ID: "k", Enabled: true, KeyHash: hash, Models: []ModelRule{{Alias: "fast", Provider: "openai", TargetModel: "m", InputPricePerMillion: 3, OutputPricePerMillion: 15}}},
	}}); err != nil {
		t.Fatal(err)
	}
	sum := s.UsageSummaryFor(imgKey(s, "k"))
	if sum.DailyUSD <= 0 {
		t.Fatalf("after reconfigure, DailyUSD = %v, want >0 (usage should have been flushed before reload)", sum.DailyUSD)
	}
}

// TestFlushUsagePreservesDiskKeys (Bug 3): the periodic usage flush must not
// overwrite the on-disk key list. We seed a key on disk, then call FlushUsage
// on a store whose in-memory key set differs (missing the key). The disk key
// must survive because FlushUsage only writes usage, not keys.
func TestFlushUsagePreservesDiskKeys(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	hash, err := HashKey("cpa_p3")
	if err != nil {
		t.Fatal(err)
	}
	// Seed state with one key on disk.
	seed := NewStore()
	seed.SetClock(func() time.Time { return now })
	if err := seed.Configure(Config{Enabled: true, StateFile: path, Keys: []KeyConfig{
		{ID: "survivor", Enabled: true, KeyHash: hash, Models: []ModelRule{{Alias: "fast", Provider: "openai", TargetModel: "m", InputPricePerMillion: 3, OutputPricePerMillion: 15}}},
	}}); err != nil {
		t.Fatal(err)
	}
	// Build a second store pointed at the same path but with NO keys in memory,
	// then record usage and flush. Bug 3 fix: FlushUsage preserves disk keys.
	s := NewStore()
	s.SetClock(func() time.Time { return now })
	if err := s.Configure(Config{Enabled: true, StateFile: path}); err != nil {
		t.Fatal(err)
	}
	// s now has the disk key (loaded). Remove it from memory to simulate a
	// truncated in-memory snapshot, then flush usage.
	s.mu.Lock()
	delete(s.keys, "survivor")
	s.mu.Unlock()
	if err := s.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	// Reload from disk: the key must still be there.
	chk := NewStore()
	chk.SetClock(func() time.Time { return now })
	if err := chk.Configure(Config{Enabled: true, StateFile: path}); err != nil {
		t.Fatal(err)
	}
	if chk.findByID("survivor") == nil {
		t.Fatalf("disk key 'survivor' was wiped by FlushUsage; Bug 3 regression")
	}
}

func TestSaveUsageOnlyDoesNotOverwriteCorruptState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	original := []byte(`{"keys":`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveUsageOnly(path, map[string]*UsageState{}); err != nil {
		t.Fatal(err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != string(original) {
		t.Fatalf("corrupt state was overwritten: %q", current)
	}
	if _, err := os.Stat(path + ".usage"); err != nil {
		t.Fatalf("usage sidecar was not written: %v", err)
	}
}

func TestConcurrentSaveStateAndUsagePreservesManagementKeys(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	oldKey := KeyConfig{ID: "old", Enabled: true, KeyHash: "old-hash"}
	newKey := KeyConfig{ID: "new", Enabled: true, KeyHash: "new-hash"}
	if err := SaveState(path, []KeyConfig{oldKey}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	usage := make(map[string]*UsageState, 4096)
	for i := 0; i < 4096; i++ {
		usage["usage-"+itoa(i)] = &UsageState{}
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		<-start
		if err := SaveUsageOnly(path, usage); err != nil {
			t.Errorf("SaveUsageOnly: %v", err)
		}
	}()
	go func() {
		defer wg.Done()
		<-start
		if err := SaveState(path, []KeyConfig{newKey}, nil, nil, nil); err != nil {
			t.Errorf("SaveState: %v", err)
		}
	}()
	close(start)
	wg.Wait()
	state, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Keys) != 1 || state.Keys[0].ID != newKey.ID {
		t.Fatalf("concurrent usage flush replaced management keys: %+v", state.Keys)
	}
}

func TestIndependentStoresMergeDifferentKeyUpdates(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	hashA, _ := HashKey("cpa_merge_a")
	hashB, _ := HashKey("cpa_merge_b")
	cfg := Config{Enabled: true, StateFile: path, Keys: []KeyConfig{
		{ID: "a", Enabled: true, KeyHash: hashA, Models: []ModelRule{{Alias: "base", Provider: "openai", TargetModel: "m"}}},
		{ID: "b", Enabled: true, KeyHash: hashB, Models: []ModelRule{{Alias: "base", Provider: "openai", TargetModel: "m"}}},
	}}
	first := NewStore()
	if err := first.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	second := NewStore()
	if err := second.Configure(Config{Enabled: true, StateFile: path}); err != nil {
		t.Fatal(err)
	}
	a := first.findByID("a")
	a.Name = "updated-a"
	if err := first.UpsertKey(*a, true); err != nil {
		t.Fatal(err)
	}
	b := second.findByID("b")
	b.Name = "updated-b"
	if err := second.UpsertKey(*b, true); err != nil {
		t.Fatal(err)
	}
	state, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]string{}
	for _, key := range state.Keys {
		names[key.ID] = key.Name
	}
	if names["a"] != "updated-a" || names["b"] != "updated-b" {
		t.Fatalf("transactional key merge lost an update: %+v", names)
	}
}

func TestRefreshFromDiskMakesIndependentStoreSeeManagementChanges(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	hash, err := HashKey("cpa_refresh")
	if err != nil {
		t.Fatal(err)
	}
	cfg := Config{Enabled: true, StateFile: path, Keys: []KeyConfig{{
		ID: "shared", Enabled: true, KeyHash: hash, Models: []ModelRule{{Alias: "base", Provider: "openai", TargetModel: "m"}},
	}}}
	writer := NewStore()
	if err := writer.Configure(cfg); err != nil {
		t.Fatal(err)
	}
	reader := NewStore()
	if err := reader.Configure(Config{Enabled: true, StateFile: path}); err != nil {
		t.Fatal(err)
	}
	updated := writer.Keys()[0]
	updated.Name = "updated-on-disk"
	if err := writer.UpsertKey(updated, true); err != nil {
		t.Fatal(err)
	}
	if err := reader.RefreshFromDisk(); err != nil {
		t.Fatal(err)
	}
	got := reader.findByID("shared")
	if got == nil || got.Name != "updated-on-disk" {
		t.Fatalf("independent store did not refresh management state: %+v", got)
	}
}

// TestKeysSnapshotSortedByID (Bug 5): Keys() returns a deterministic order
// sorted by ID, not random map-iteration order.
func TestKeysSnapshotSortedByID(t *testing.T) {
	hash, err := HashKey("cpa_sort")
	if err != nil {
		t.Fatal(err)
	}
	s := NewStore()
	if err := s.Configure(Config{Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"), Keys: []KeyConfig{
		{ID: "zeta", Enabled: true, KeyHash: hash, Models: []ModelRule{{Alias: "a", Provider: "x", TargetModel: "m"}}},
		{ID: "alpha", Enabled: true, KeyHash: hash, Models: []ModelRule{{Alias: "a", Provider: "x", TargetModel: "m"}}},
		{ID: "mid", Enabled: true, KeyHash: hash, Models: []ModelRule{{Alias: "a", Provider: "x", TargetModel: "m"}}},
	}}); err != nil {
		t.Fatal(err)
	}
	got := s.Keys()
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}
	want := []string{"alpha", "mid", "zeta"}
	for i, w := range want {
		if got[i].ID != w {
			t.Fatalf("Keys()[%d].ID = %q, want %q (full order: %v)", i, got[i].ID, w, keyIDs(got))
		}
	}
	// Run several times: order must be stable (regression check for map
	// iteration randomness creeping back).
	for i := 0; i < 20; i++ {
		ks := s.Keys()
		for j, w := range want {
			if ks[j].ID != w {
				t.Fatalf("iter %d Keys()[%d].ID = %q, want %q", i, j, ks[j].ID, w)
			}
		}
	}
}

func keyIDs(ks []KeyConfig) []string {
	out := make([]string, len(ks))
	for i, k := range ks {
		out[i] = k.ID
	}
	return out
}

func TestStopUsageFlusherWaitsForWorkerExit(t *testing.T) {
	store, _ := newTestStore(t)
	store.StartUsageFlusher()
	store.mu.RLock()
	flusher := store.flusher
	store.mu.RUnlock()
	if flusher == nil {
		t.Fatal("usage flusher was not started")
	}
	store.StopUsageFlusher()
	select {
	case <-flusher.doneCh:
	default:
		t.Fatal("StopUsageFlusher returned before worker exit")
	}
}

func TestStopUsageFlusherFlushesWithoutWorker(t *testing.T) {
	now := time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "state.json")
	hash, err := HashKey("cpa_shutdown_flush")
	if err != nil {
		t.Fatal(err)
	}
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{Enabled: true, StateFile: path, Keys: []KeyConfig{
		{ID: "shutdown-key", Enabled: true, KeyHash: hash, Models: []ModelRule{{Alias: "fast", Provider: "openai", TargetModel: "m", BillingMode: "per_call", PerCallUSD: 1}}},
	}}); err != nil {
		t.Fatal(err)
	}
	store.RecordUsage("shutdown-key", "fast", "m", false, UsageDetail{})

	// A failed reconfigure can leave no worker running while the plugin keeps
	// serving. Shutdown must still persist usage recorded after that point.
	store.StopUsageFlusher()

	state, err := LoadState(path)
	if err != nil {
		t.Fatal(err)
	}
	usage := state.Usage["shutdown-key"]
	if usage == nil || usage.Daily.TotalUSD != 1 || usage.Daily.CallCount != 1 {
		t.Fatalf("persisted usage = %#v, want one $1 call", usage)
	}
}

// imgKey returns the KeyConfig for id (helper for tests that need a value, not
// a pointer, for UsageSummaryFor).
func imgKey(s *Store, id string) KeyConfig {
	k := s.findByID(id)
	if k == nil {
		panic("key not found: " + id)
	}
	return *k
}
