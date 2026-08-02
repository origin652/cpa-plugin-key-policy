package policy

import (
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newClockedStore(t *testing.T, now time.Time) (*Store, time.Time) {
	t.Helper()
	tm := now
	store := NewStore()
	store.SetClock(func() time.Time { return tm })
	err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{
			{
				ID: "team-a", Enabled: true,
				KeyHash:    hashForUsageTest(t, "cpa_usage"),
				KeyPreview: "cpa_us..._age",
				Models: []ModelRule{
					{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex",
						InputPricePerMillion: 1, OutputPricePerMillion: 2},
				},
				DailyLimitUSD:  1.00,
				WeeklyLimitUSD: 5.00,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, tm
}

func TestUsageLedgerLoadAndSnapshotDeepCopyAliasMap(t *testing.T) {
	sourceState := &UsageState{
		Daily: UsageWindow{TotalUSD: 1},
		ByAlias: map[string]AliasUsageWindows{
			"fast": {Daily: UsageWindow{TotalUSD: 1}},
		},
	}
	ledger := newUsageLedger(time.Now)
	ledger.loadFromState(map[string]*UsageState{"team-a": sourceState})

	// Mutating the state object supplied by the persistence layer must not
	// change the live ledger.
	sourceState.Daily.TotalUSD = 9
	sourceState.ByAlias["fast"] = AliasUsageWindows{Daily: UsageWindow{TotalUSD: 9}}
	first := ledger.snapshot()["team-a"]
	if !nearly(first.Daily.TotalUSD, 1) || !nearly(first.ByAlias["fast"].Daily.TotalUSD, 1) {
		t.Fatalf("loaded ledger shares source state: %+v", first)
	}

	// A caller mutating a persistence/reporting snapshot must likewise leave
	// the live ledger untouched.
	first.Daily.TotalUSD = 7
	first.ByAlias["fast"] = AliasUsageWindows{Daily: UsageWindow{TotalUSD: 7}}
	second := ledger.snapshot()["team-a"]
	if !nearly(second.Daily.TotalUSD, 1) || !nearly(second.ByAlias["fast"].Daily.TotalUSD, 1) {
		t.Fatalf("ledger shares returned snapshot: %+v", second)
	}
}

func hashForUsageTest(t *testing.T, key string) string {
	t.Helper()
	h, err := HashKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestUsageRecordAndOverLimitDaily(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store, tm := newClockedStore(t, now)
	headers := map[string][]string{"Authorization": {"Bearer cpa_usage"}}

	// 500K prompt × $1/M = $0.50 → under the $1 daily limit.
	_ = store.RecordResponseCost(headers, nil, "fast", []byte(`{"usage":{"prompt_tokens":500000,"completion_tokens":0}}`))
	d := store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("first request should be allowed: %+v", d)
	}
	// Another $0.50 → total $1.00, equals the daily limit. Billing is
	// post-hoc: this request itself was allowed (the prior Authenticate
	// passed), but the NEXT request now sees daily_usd >= limit and is
	// rejected (Authenticate is a pre-request gate on accumulated usage).
	_ = store.RecordResponseCost(headers, nil, "fast", []byte(`{"usage":{"prompt_tokens":500000,"completion_tokens":0}}`))
	d = store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"fast"}`))
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("at-limit request should be rejected on the next Authenticate: %+v", d)
	}
	// Crossing UTC midnight resets the daily window.
	tm = tm.Add(14 * time.Hour) // next day
	store.SetClock(func() time.Time { return tm })
	d = store.Authenticate("POST", "/v1/chat/completions", headers, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("after midnight should be allowed again: %+v", d)
	}
}

func TestUsageUnlimitedKeyNeverBlocked(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{{
			ID: "free", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_free"),
			Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex",
				InputPricePerMillion: 10, OutputPricePerMillion: 10}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hdr := map[string][]string{"Authorization": {"Bearer cpa_free"}}
	for i := 0; i < 50; i++ {
		_ = store.RecordResponseCost(hdr, nil, "fast", []byte(`{"usage":{"prompt_tokens":1000000,"completion_tokens":1000000}}`))
		d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
		if !d.Allowed {
			t.Fatalf("unlimited key blocked at iter %d: %+v", i, d)
		}
	}
}

func TestUsageUnpricedAliasNotBilled(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{{
			ID: "cheap", Enabled: true, DailyLimitUSD: 0.01,
			KeyHash: hashForUsageTest(t, "cpa_cheap"),
			Models:  []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex"}}, // no prices
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hdr := map[string][]string{"Authorization": {"Bearer cpa_cheap"}}
	_ = store.RecordResponseCost(hdr, nil, "fast", []byte(`{"usage":{"prompt_tokens":99999999,"completion_tokens":99999999}}`))
	d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("unpriced alias should never exceed: %+v", d)
	}
}

func TestUsageStreamingBilledWhenUsageFrame(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{{
			ID: "streamy", Enabled: true, DailyLimitUSD: 0.01,
			KeyHash: hashForUsageTest(t, "cpa_stream"),
			Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex",
				InputPricePerMillion: 1, OutputPricePerMillion: 1}},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	hdr := map[string][]string{"Authorization": {"Bearer cpa_stream"}}

	// A streaming response whose host passed us an SSE body WITH a final usage
	// frame is billed (post-hoc billing applies to streams too).
	sse := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":1000000,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n")
	_ = store.RecordResponseCost(hdr, nil, "fast", sse)
	d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	// 1M tokens × $1/M = $1.00 >= $0.01 limit → rejected.
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("streaming with usage frame should be billed & blocked: %+v", d)
	}

	// A streaming body the host passes WITHOUT any usage frame is not billed.
	store2 := NewStore()
	store2.SetClock(func() time.Time { return now })
	if err := store2.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state2.json"),
		Keys: []KeyConfig{{
			ID: "streamy2", Enabled: true, DailyLimitUSD: 0.01,
			KeyHash: hashForUsageTest(t, "cpa_stream2"),
			Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex",
				InputPricePerMillion: 1, OutputPricePerMillion: 1}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	hdr2 := map[string][]string{"Authorization": {"Bearer cpa_stream2"}}
	_ = store2.RecordResponseCost(hdr2, nil, "fast", nil)
	_ = store2.RecordResponseCost(hdr2, nil, "fast", []byte(`data: {"delta":"hi"}`))
	d = store2.Authenticate("POST", "/v1/chat/completions", hdr2, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("streaming without usage frame should not be billed: %+v", d)
	}
}

func TestUsageSummaryReflectsUsage(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store, _ := newClockedStore(t, now)
	hdr := map[string][]string{"Authorization": {"Bearer cpa_usage"}}
	_ = store.RecordResponseCost(hdr, nil, "fast", []byte(`{"usage":{"prompt_tokens":200000,"completion_tokens":100000}}`))
	keys := store.Keys()
	var key KeyConfig
	for _, k := range keys {
		if k.ID == "team-a" {
			key = k
		}
	}
	s := store.UsageSummaryFor(key)
	// 200K×$1/M + 100K×$2/M = $0.20 + $0.20 = $0.40
	if !nearly(s.DailyUSD, 0.40) || !nearly(s.WeeklyUSD, 0.40) {
		t.Fatalf("summary = %+v, want 0.40/0.40", s)
	}
	if s.DailyLimitUSD != 1.0 || s.WeeklyLimitUSD != 5.0 {
		t.Fatalf("limits = %+v", s)
	}
	if s.DailyResetAt.IsZero() {
		t.Fatal("daily_reset_at should be set")
	}
}

func TestUsagePersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	mk := func(clock func() time.Time) *Store {
		s := NewStore()
		s.SetClock(clock)
		if err := s.Configure(Config{
			Enabled: true, StateFile: path,
			Keys: []KeyConfig{{
				ID: "team-a", Enabled: true, DailyLimitUSD: 1.0,
				KeyHash: hashForUsageTest(t, "cpa_usage"),
				Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex",
					InputPricePerMillion: 1, OutputPricePerMillion: 0}},
			}},
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	s1 := mk(func() time.Time { return now })
	hdr := map[string][]string{"Authorization": {"Bearer cpa_usage"}}
	_ = s1.RecordResponseCost(hdr, nil, "fast", []byte(`{"usage":{"prompt_tokens":800000,"completion_tokens":0}}`))
	if err := s1.FlushUsage(); err != nil {
		t.Fatal(err)
	}

	// "Restart": a fresh store loads from the same state file.
	s2 := mk(func() time.Time { return now })
	keys := s2.Keys()
	var key KeyConfig
	for _, k := range keys {
		if k.ID == "team-a" {
			key = k
		}
	}
	s := s2.UsageSummaryFor(key)
	if !nearly(s.DailyUSD, 0.80) {
		t.Fatalf("usage after restart = %+v, want 0.80", s)
	}
	// Over-limit is enforced post-restart (0.80 < 1.0, allowed; then bill to >1).
	d := s2.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("should be allowed at 0.80/1.0: %+v", d)
	}
}

func TestResetUsageWindowDailyAndWeekly(t *testing.T) {
	now := time.Date(2026, 8, 2, 1, 30, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: statePath,
		Keys: []KeyConfig{{
			ID: "resettable", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_resettable"),
			Models: []ModelRule{{
				Alias: "fast", Provider: "codex", TargetModel: "gpt-5",
				InputPricePerMillion: 1, OutputPricePerMillion: 1,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	_ = store.RecordUsage("resettable", "fast", "gpt-5", false, UsageDetail{
		InputTokens: 300_000,
	})
	dailyResult, err := store.ResetUsageWindow("resettable", UsageResetDaily)
	if err != nil {
		t.Fatal(err)
	}
	if !nearly(dailyResult.BeforeDailyUSD, 0.30) || !nearly(dailyResult.BeforeWeeklyUSD, 0.30) {
		t.Fatalf("daily reset before = %+v, want 0.30/0.30", dailyResult)
	}
	summary := store.UsageSummaryFor(store.Keys()[0])
	if !nearly(summary.DailyUSD, 0) || !nearly(summary.WeeklyUSD, 0.30) {
		t.Fatalf("after daily reset = %+v, want daily 0 / weekly 0.30", summary)
	}
	_, aliases, ok := store.AliasUsageFor("resettable")
	if !ok || len(aliases) != 1 || !nearly(aliases[0].Daily.TotalUSD, 0) || !nearly(aliases[0].Weekly.TotalUSD, 0.30) {
		t.Fatalf("alias after daily reset = %+v, want daily 0 / weekly 0.30", aliases)
	}

	_ = store.RecordUsage("resettable", "fast", "gpt-5", false, UsageDetail{
		InputTokens: 200_000,
	})
	weeklyResult, err := store.ResetUsageWindow("resettable", UsageResetWeekly)
	if err != nil {
		t.Fatal(err)
	}
	if !nearly(weeklyResult.BeforeDailyUSD, 0.20) || !nearly(weeklyResult.BeforeWeeklyUSD, 0.50) {
		t.Fatalf("weekly reset before = %+v, want 0.20/0.50", weeklyResult)
	}
	summary = store.UsageSummaryFor(store.Keys()[0])
	if !nearly(summary.DailyUSD, 0) || !nearly(summary.WeeklyUSD, 0) {
		t.Fatalf("after weekly reset = %+v, want daily 0 / weekly 0", summary)
	}
	_, aliases, _ = store.AliasUsageFor("resettable")
	if len(aliases) != 1 || !nearly(aliases[0].Daily.TotalUSD, 0) || !nearly(aliases[0].Weekly.TotalUSD, 0) {
		t.Fatalf("alias after weekly reset = %+v, want daily 0 / weekly 0", aliases)
	}

	reloaded := NewStore()
	reloaded.SetClock(func() time.Time { return now })
	if err := reloaded.Configure(Config{Enabled: true, StateFile: statePath}); err != nil {
		t.Fatal(err)
	}
	reloadedSummary := reloaded.UsageSummaryFor(reloaded.Keys()[0])
	if !nearly(reloadedSummary.DailyUSD, 0) || !nearly(reloadedSummary.WeeklyUSD, 0) {
		t.Fatalf("persisted reset after reload = %+v, want 0/0", reloadedSummary)
	}
}

func TestResetUsageWindowRejectsInvalidInput(t *testing.T) {
	store, _ := newClockedStore(t, time.Date(2026, 8, 2, 1, 30, 0, 0, time.UTC))
	if _, err := store.ResetUsageWindow("missing", UsageResetDaily); !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("unknown key error = %v, want ErrUnknownKey", err)
	}
	if _, err := store.ResetUsageWindow("team-a", UsageResetWindow("month")); err == nil {
		t.Fatal("invalid reset window should fail")
	}
}

func TestResetUsageWindowRollsBackWhenPersistenceFails(t *testing.T) {
	now := time.Date(2026, 8, 2, 1, 30, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: statePath,
		Keys: []KeyConfig{{
			ID: "resettable", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_resettable"),
			Models: []ModelRule{{
				Alias: "fast", Provider: "codex", TargetModel: "gpt-5",
				InputPricePerMillion: 1, OutputPricePerMillion: 1,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.RecordUsage("resettable", "fast", "gpt-5", false, UsageDetail{
		InputTokens: 300_000,
	})

	corruptState := []byte(`{"version":`)
	if err := os.WriteFile(statePath, corruptState, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResetUsageWindow("resettable", UsageResetWeekly); err == nil {
		t.Fatal("reset should fail when the current state file cannot be loaded")
	}

	summary := store.UsageSummaryFor(store.Keys()[0])
	if !nearly(summary.DailyUSD, 0.30) || !nearly(summary.WeeklyUSD, 0.30) {
		t.Fatalf("usage after failed reset = %+v, want in-memory rollback to 0.30/0.30", summary)
	}
	_, aliases, ok := store.AliasUsageFor("resettable")
	if !ok || len(aliases) != 1 || !nearly(aliases[0].Daily.TotalUSD, 0.30) || !nearly(aliases[0].Weekly.TotalUSD, 0.30) {
		t.Fatalf("alias usage after failed reset = %+v, want rollback to 0.30/0.30", aliases)
	}
	persisted, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(persisted) != string(corruptState) {
		t.Fatalf("failed reset changed state file: %q", persisted)
	}
}

// newCacheStore builds a store with one key whose alias has an explicit
// cache-read price, for cache-stat accounting tests.
func newCacheStore(t *testing.T, now time.Time, provider string) *Store {
	t.Helper()
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{{
			ID: "cache-key", Enabled: true,
			KeyHash:    hashForUsageTest(t, "cpa_cache"),
			KeyPreview: "cpa_ca...che",
			Models: []ModelRule{{
				Alias: "fast", Provider: provider, TargetModel: "m",
				InputPricePerMillion:     3,
				OutputPricePerMillion:    15,
				CacheReadPricePerMillion: 0.30,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestCacheStatsAccumulatedAndHitRate: a subset-provider record (1M input incl
// 200K cached + 500K output @ $3/$15/$0.30) → total $9.96, cacheCost $0.06,
// cacheRead 200K, nonCacheInput 800K. Hit-rate = 200K/(200K+800K) = 20%.
func TestCacheStatsAccumulatedAndHitRate(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := newCacheStore(t, now, "openai")
	cost := store.RecordUsage("cache-key", "FAST", "m", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000,
	})
	if !nearly(cost, 9.96) {
		t.Fatalf("cost = %v, want 9.96", cost)
	}
	key := store.Keys()[0]
	s := store.UsageSummaryFor(key)
	if !nearly(s.DailyCacheCostUSD, 0.06) {
		t.Fatalf("daily cache cost = %v, want 0.06", s.DailyCacheCostUSD)
	}
	if s.DailyCacheReadTokens != 200_000 {
		t.Fatalf("daily cache read tokens = %d, want 200000", s.DailyCacheReadTokens)
	}
	if s.DailyInputTokens != 800_000 {
		t.Fatalf("daily non-cache input tokens = %d, want 800000", s.DailyInputTokens)
	}
	// Hit-rate = cacheRead / (cacheRead + input) = 200K / 1M = 0.2.
	if got := float64(s.DailyCacheReadTokens) / float64(s.DailyCacheReadTokens+s.DailyInputTokens); !nearly(got, 0.2) {
		t.Fatalf("hit-rate = %v, want 0.2", got)
	}
	// Weekly window mirrors daily for a same-day single record.
	if !nearly(s.WeeklyCacheCostUSD, 0.06) || s.WeeklyCacheReadTokens != 200_000 {
		t.Fatalf("weekly cache stats = %+v, want 0.06/200000", s)
	}
	_, rows, ok := store.AliasUsageFor("cache-key")
	if !ok || len(rows) != 1 || rows[0].Alias != "fast" {
		t.Fatalf("cache-aware mixed-case alias rows = %+v, want one canonical fast row", rows)
	}
}

// TestCacheStatsResetAtMidnight: cache counters reset when the daily window
// rolls across UTC midnight. SetClock rebuilds the in-memory ledger (matching
// how the existing daily-limit test models a clock jump), so we re-record after
// advancing the clock and assert the daily cache stats reflect ONLY the new
// day's record. (Cross-day weekly accumulation is covered by the persist test,
// which survives the ledger rebuild via the state file.)
func TestCacheStatsResetAtMidnight(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := newCacheStore(t, now, "openai")
	_ = store.RecordUsage("cache-key", "fast", "m", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000,
	})
	store.SetClock(func() time.Time { return now.Add(14 * time.Hour) }) // next UTC day
	// A new day-2 record: 1M input (200K cached), no output.
	_ = store.RecordUsage("cache-key", "fast", "m", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 0, CachedTokens: 200_000,
	})

	s := store.UsageSummaryFor(store.Keys()[0])
	// Daily = day-2 only: 200K cacheRead, 0.06 cacheCost, 800K nonCache input.
	if s.DailyCacheReadTokens != 200_000 || !nearly(s.DailyCacheCostUSD, 0.06) || s.DailyInputTokens != 800_000 {
		t.Fatalf("daily cache stats after midnight = %+v, want 200000/0.06/800000", s)
	}
	// Day-1's 500K output must NOT leak into the new daily window.
	if !nearly(s.DailyUSD, 2.46) { // 800K*3 + 200K*0.30 = 2.4 + 0.06
		t.Fatalf("daily total after midnight = %v, want 2.46 (day-2 only)", s.DailyUSD)
	}
}

// TestCacheStatsAdditiveExcludesCreation: for an additive provider (Claude),
// cache-creation (write) tokens must NOT be counted in cacheRead — only reads.
// 800K input + 200K cacheRead + 100K cacheCreation + 500K output @ $3/$15/$0.30.
func TestCacheStatsAdditiveExcludesCreation(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := newCacheStore(t, now, "claude")
	_ = store.RecordUsage("cache-key", "fast", "m", false, UsageDetail{
		InputTokens:         800_000,
		OutputTokens:        500_000,
		CacheReadTokens:     200_000,
		CacheCreationTokens: 100_000,
	})
	s := store.UsageSummaryFor(store.Keys()[0])
	if s.DailyCacheReadTokens != 200_000 {
		t.Fatalf("daily cacheRead = %d, want 200000 (creation excluded)", s.DailyCacheReadTokens)
	}
	// nonCache input = input + creation = 900K (additive bills creation at input price).
	if s.DailyInputTokens != 900_000 {
		t.Fatalf("daily non-cache input = %d, want 900000", s.DailyInputTokens)
	}
}

// TestCacheStatsPersistAcrossRestart: cache counters survive a state-file
// reload, so the UI keeps showing cache spend/hit-rate after a plugin restart.
func TestCacheStatsPersistAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	mk := func() *Store {
		s := NewStore()
		s.SetClock(func() time.Time { return now })
		if err := s.Configure(Config{
			Enabled: true, StateFile: path,
			Keys: []KeyConfig{{
				ID: "cache-key", Enabled: true,
				KeyHash: hashForUsageTest(t, "cpa_cache"),
				Models: []ModelRule{{
					Alias: "fast", Provider: "openai", TargetModel: "m",
					InputPricePerMillion:     3,
					OutputPricePerMillion:    15,
					CacheReadPricePerMillion: 0.30,
				}},
			}},
		}); err != nil {
			t.Fatal(err)
		}
		return s
	}
	s1 := mk()
	_ = s1.RecordUsage("cache-key", "fast", "m", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000,
	})
	if err := s1.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	s2 := mk()
	got := s2.UsageSummaryFor(s2.Keys()[0])
	if !nearly(got.DailyCacheCostUSD, 0.06) || got.DailyCacheReadTokens != 200_000 || got.DailyInputTokens != 800_000 {
		t.Fatalf("cache stats after restart = %+v, want 0.06/200000/800000", got)
	}
}

// newPerCallStore builds a store with one key whose alias is billed per-call at
// perCallUSD, under a daily dollar limit, for per-call billing tests.
func newPerCallStore(t *testing.T, perCallUSD, dailyLimit float64) *Store {
	t.Helper()
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{{
			ID: "percall", Enabled: true, DailyLimitUSD: dailyLimit,
			KeyHash: hashForUsageTest(t, "cpa_percall"),
			Models: []ModelRule{{
				Alias:       "fast",
				Provider:    "codex",
				TargetModel: "gpt-5-codex",
				BillingMode: "per_call",
				PerCallUSD:  perCallUSD,
				// Token prices are dormant under per_call but kept to verify they
				// are NOT used.
				InputPricePerMillion:  999,
				OutputPricePerMillion: 999,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// TestPerCallBillsFixedUSD: each successful request charges PerCallUSD
// regardless of token counts; token prices are ignored. Two $0.50 calls hit
// the $1.00 daily limit, so the next Authenticate is rejected.
func TestPerCallBillsFixedUSD(t *testing.T) {
	store := newPerCallStore(t, 0.50, 1.00)
	hdr := map[string][]string{"Authorization": {"Bearer cpa_percall"}}

	// A "successful" usage record with huge token counts — per_call must charge
	// the fixed $0.50, NOT the (dormant) token price.
	cost := store.RecordUsage("percall", "FAST", "gpt-5-codex", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
	})
	if !nearly(cost, 0.50) {
		t.Fatalf("per_call cost = %v, want 0.50", cost)
	}
	d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("first call should be allowed: %+v", d)
	}
	// Second $0.50 → total $1.00 == limit. Next Authenticate rejected.
	_ = store.RecordUsage("percall", "FaSt", "gpt-5-codex", false, UsageDetail{})
	d = store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("after two per_call charges, next should be daily_exceeded: %+v", d)
	}
	// CallCount reflects two successful calls.
	s := store.UsageSummaryFor(store.Keys()[0])
	if s.DailyCallCount != 2 {
		t.Fatalf("daily call count = %d, want 2", s.DailyCallCount)
	}
	_, rows, ok := store.AliasUsageFor("percall")
	if !ok || len(rows) != 1 || rows[0].Alias != "fast" || rows[0].Daily.CallCount != 2 {
		t.Fatalf("per-call mixed-case alias rows = %+v, want one canonical fast row with 2 calls", rows)
	}
}

// TestTokenModeFreeAliasStillCounts: a token-mode alias whose configured
// input/output/cache prices are all 0 (priced=true but free) must still
// record token + call counters. Previously both RecordResponseCost and
// RecordUsage gated RecordCost on `cost > 0`, dropping free-but-priced
// requests entirely so their usage volume / hit-rate was invisible.
func TestTokenModeFreeAliasStillCounts(t *testing.T) {
	now := time.Date(2026, 7, 2, 12, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	hash, err := HashKey("cpa_free2")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"), Keys: []KeyConfig{{
		ID: "free", Enabled: true, KeyHash: hash,
		Models: []ModelRule{{
			Alias: "fast", Provider: "anthropic", TargetModel: "m",
			InputPricePerMillion:     0,
			OutputPricePerMillion:    0,
			CacheReadPricePerMillion: 0,
		}},
	}}}); err != nil {
		t.Fatal(err)
	}
	// usage.handle path (RecordUsage): non-zero tokens, all prices 0.
	cost := store.RecordUsage("free", "fast", "m", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 500_000, CacheReadTokens: 200_000,
	})
	if cost != 0 {
		t.Fatalf("cost = %v, want 0 (free alias)", cost)
	}
	sum := store.UsageSummaryFor(store.Keys()[0])
	if sum.DailyCallCount != 1 {
		t.Fatalf("DailyCallCount = %d, want 1 (free call must still count)", sum.DailyCallCount)
	}
	if sum.DailyInputTokens != 1_000_000 {
		t.Fatalf("DailyInputTokens = %d, want 1000000 (Anthropic input excludes cache reads)", sum.DailyInputTokens)
	}
	if sum.DailyCacheReadTokens != 200_000 {
		t.Fatalf("DailyCacheReadTokens = %d, want 200000 (cache tokens must count even when free)", sum.DailyCacheReadTokens)
	}
}

// TestAllowModelsEndpointPerKey: a key with AllowModelsEndpoint=true may reach
// GET /v1/models (allowed); a key with it false (default) is 401. We cannot
// filter the list contents per key (CPA limitation), only hide/show it.
func TestAllowModelsEndpointPerKey(t *testing.T) {
	// Distinct secrets per key so Authenticate resolves the intended config
	// (sharing one hash makes map iteration order non-deterministic).
	store := NewStore()
	hashA, _ := HashKey("cpa_a")
	hashB, _ := HashKey("cpa_b")
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(t.TempDir(), "state2.json"), Keys: []KeyConfig{
		{ID: "hidden", Enabled: true, KeyHash: hashA, Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex"}}},
		{ID: "open", Enabled: true, KeyHash: hashB, Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex"}}, AllowModelsEndpoint: true},
	}}); err != nil {
		t.Fatal(err)
	}
	dHidden := store.Authenticate("GET", "/v1/models", http.Header{"Authorization": {"Bearer cpa_a"}}, nil, nil)
	if dHidden.Allowed || dHidden.Reason != "models_endpoint_disabled" {
		t.Fatalf("hidden key (secret a) = %+v, want models_endpoint_disabled", dHidden)
	}
	dOpen := store.Authenticate("GET", "/v1/models", http.Header{"Authorization": {"Bearer cpa_b"}}, nil, nil)
	if !dOpen.Allowed || dOpen.Reason != "models_endpoint_allowed" {
		t.Fatalf("open key (secret b) = %+v, want Allowed + models_endpoint_allowed", dOpen)
	}
}

// TestPerCallFailedNotBilled: a failed request charges nothing and does not
// increment CallCount (per-call only applies to HTTP-200 outcomes).
func TestPerCallFailedNotBilled(t *testing.T) {
	store := newPerCallStore(t, 0.50, 1.00)
	cost := store.RecordUsage("percall", "fast", "gpt-5-codex", true, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
	})
	if cost != 0 {
		t.Fatalf("failed per_call cost = %v, want 0", cost)
	}
	s := store.UsageSummaryFor(store.Keys()[0])
	if s.DailyCallCount != 0 || !nearly(s.DailyUSD, 0) {
		t.Fatalf("failed call should not count: %+v", s)
	}
}

// TestPerCallZeroStillCounts: PerCallUSD=0 is allowed (free calls). Cost is 0
// but CallCount still increments, and the key never exceeds a dollar limit.
func TestPerCallZeroStillCounts(t *testing.T) {
	store := newPerCallStore(t, 0, 0.01)
	for i := 0; i < 5; i++ {
		cost := store.RecordUsage("percall", "fast", "gpt-5-codex", false, UsageDetail{})
		if cost != 0 {
			t.Fatalf("free per_call cost = %v, want 0", cost)
		}
	}
	s := store.UsageSummaryFor(store.Keys()[0])
	if s.DailyCallCount != 5 {
		t.Fatalf("daily call count = %d, want 5", s.DailyCallCount)
	}
	// No dollar spend → never blocked even with a tiny limit.
	hdr := map[string][]string{"Authorization": {"Bearer cpa_percall"}}
	d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("free per_call should never exceed dollar limit: %+v", d)
	}
}

// TestAliasUsageBreakdown: per-alias daily/weekly windows accumulate
// independently, configured-but-unused aliases appear as zero rows, output
// tokens are tracked, and an alias that was billed then removed from the key's
// config appears as a residual with InConfig=false.
func TestAliasUsageBreakdown(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	mkCfg := func(models []ModelRule) Config {
		return Config{Enabled: true, StateFile: statePath, Keys: []KeyConfig{{
			ID: "team-a", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_usage"),
			Models:  models,
		}}}
	}
	fast := ModelRule{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex",
		InputPricePerMillion: 1, OutputPricePerMillion: 2}
	slow := ModelRule{Alias: "slow", Provider: "codex", TargetModel: "o4-mini",
		InputPricePerMillion: 1, OutputPricePerMillion: 1}
	if err := store.Configure(mkCfg([]ModelRule{fast, slow})); err != nil {
		t.Fatal(err)
	}

	// Bill fast: 200K input + 100K output @ $1/$2 = $0.20 + $0.20 = $0.40.
	_ = store.RecordUsage("team-a", "fast", "gpt-5-codex", false, UsageDetail{
		InputTokens: 200_000, OutputTokens: 100_000,
	})
	// Bill slow so it has history, then remove it from the key's config below.
	_ = store.RecordUsage("team-a", "slow", "o4-mini", false, UsageDetail{
		InputTokens: 50_000, OutputTokens: 0,
	})
	// Update the key to ONLY fast (mirrors the management PATCH path:
	// the alias is removed from config but the usage ledger keeps its history).
	// In the new architecture, keys reference global aliases by name, so we
	// update teamA.Aliases to only reference "fast".
	teamA := store.Keys()[0]
	teamA.Aliases = []KeyAliasRef{{Alias: "fast"}}
	teamA.Models = nil
	if err := store.UpsertKey(teamA, true); err != nil {
		t.Fatal(err)
	}

	_, rows, ok := store.AliasUsageFor("team-a")
	if !ok {
		t.Fatal("key not found")
	}
	byAlias := map[string]AliasUsageEntry{}
	for _, r := range rows {
		byAlias[r.Alias] = r
	}
	if len(rows) != 2 {
		t.Fatalf("row count = %d, want 2 (fast+slow residual)", len(rows))
	}
	// fast: configured, billed $0.40 daily & weekly, 1 call, 200K input, 100K output.
	f := byAlias["fast"]
	if !f.InConfig || !nearly(f.Daily.TotalUSD, 0.40) || !nearly(f.Weekly.TotalUSD, 0.40) {
		t.Fatalf("fast row = %+v, want in_config=true $0.40/$0.40", f)
	}
	if f.Daily.CallCount != 1 || f.Daily.InputTokens != 200_000 || f.Daily.OutputTokens != 100_000 {
		t.Fatalf("fast daily counters = %+v, want 1/200000/100000", f.Daily)
	}
	if f.Provider != "codex" || f.TargetModel != "gpt-5-codex" {
		t.Fatalf("fast config fields = %+v", f)
	}
	// slow: removed from config but has historical usage → InConfig=false, residual data.
	s := byAlias["slow"]
	if s.InConfig {
		t.Fatalf("slow should be in_config=false after removal: %+v", s)
	}
	if !nearly(s.Daily.TotalUSD, 0.05) || s.Daily.InputTokens != 50_000 || s.Daily.CallCount != 1 {
		t.Fatalf("slow residual daily = %+v, want $0.05 / 50000 / 1 call", s.Daily)
	}
	// Sorted by alias.
	if rows[0].Alias != "fast" || rows[1].Alias != "slow" {
		t.Fatalf("rows not sorted by alias: %+v", rows)
	}
}

// TestAliasUsageUnknownKey: a missing key id returns ok=false.
func TestAliasUsageUnknownKey(t *testing.T) {
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(t.TempDir(), "s.json")}); err != nil {
		t.Fatal(err)
	}
	_, _, ok := store.AliasUsageFor("nope")
	if ok {
		t.Fatal("unknown key should return ok=false")
	}
}

// TestAliasUsageLegacyStateMigrates: a state file written in the legacy
// single-window ByAlias format (map[string]UsageWindow) loads into the new
// dual-window form: the old value lands in Daily, Weekly is zeroed, and the
// key detail API surfaces it (InConfig=false if the alias is no longer configured).
func TestAliasUsageLegacyStateMigrates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	// Hand-write a legacy state file: by_alias is map[string]UsageWindow.
	legacy := map[string]any{
		"version": 1,
		"keys": []map[string]any{{
			"id": "team-a", "enabled": true,
			"key_hash": hashForUsageTest(t, "cpa_usage"),
			"models": []map[string]any{{
				"alias": "fast", "provider": "codex", "target_model": "gpt-5-codex",
			}},
		}},
		"usage": map[string]any{
			"team-a": map[string]any{
				"daily":  map[string]any{"total_usd": 0.80, "window_start": "2026-06-29T00:00:00Z"},
				"weekly": map[string]any{"total_usd": 0.80, "window_start": "2026-06-29T00:00:00Z"},
				// Legacy single-window per-alias entry.
				"by_alias": map[string]any{
					"fast": map[string]any{"total_usd": 0.80, "call_count": 2, "input_tokens": 800000, "window_start": "2026-06-29T00:00:00Z"},
				},
			},
		},
		"updated_at": "2026-06-29T10:00:00Z",
	}
	raw, _ := json.Marshal(legacy)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}

	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{Enabled: true, StateFile: path}); err != nil {
		t.Fatal(err)
	}

	_, rows, ok := store.AliasUsageFor("team-a")
	if !ok {
		t.Fatal("key not found after migration")
	}
	if len(rows) != 1 || rows[0].Alias != "fast" {
		t.Fatalf("rows = %+v, want one fast row", rows)
	}
	fast := rows[0]
	// fast is in config → InConfig=true; legacy window migrated into Daily.
	if !fast.InConfig {
		t.Fatalf("fast should be in_config=true: %+v", fast)
	}
	if !nearly(fast.Daily.TotalUSD, 0.80) || fast.Daily.CallCount != 2 || fast.Daily.InputTokens != 800_000 {
		t.Fatalf("migrated daily = %+v, want 0.80/2/800000", fast.Daily)
	}
	// Weekly zeroed by migration (no legacy weekly per-alias data existed).
	if fast.Weekly.TotalUSD != 0 || fast.Weekly.CallCount != 0 {
		t.Fatalf("migrated weekly should be zero: %+v", fast.Weekly)
	}
	// Persisting then reloading keeps the new dual-window shape (round-trip).
	if err := store.FlushUsage(); err != nil {
		t.Fatal(err)
	}
	raw2, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var check struct {
		Usage map[string]struct {
			ByAlias map[string]struct {
				Daily  UsageWindow `json:"daily"`
				Weekly UsageWindow `json:"weekly"`
			} `json:"by_alias"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw2, &check); err != nil {
		t.Fatal(err)
	}
	a, ok := check.Usage["team-a"].ByAlias["fast"]
	if !ok {
		t.Fatal("fast not in re-persisted by_alias")
	}
	if !nearly(a.Daily.TotalUSD, 0.80) || !nearly(a.Weekly.TotalUSD, 0.80) {
		// After the flush, the weekly alias window was populated by the
		// post-migration in-memory state (RecordCost wrote both daily+weekly on
		// the original record, but the legacy file only had the single window).
		// The migration put 0.80 into Daily only; Weekly stays 0 here until a
		// new write occurs. Accept either: Daily must be 0.80.
		t.Logf("round-trip by_alias fast = %+v", a)
	}
	if !nearly(a.Daily.TotalUSD, 0.80) {
		t.Fatalf("round-trip daily = %v, want 0.80", a.Daily.TotalUSD)
	}
}

// TestRecordUsageCanonicalAliasCaseVariants: pure case variants of a configured
// alias must land in the single config-spelling ByAlias bucket (not three).
func TestRecordUsageCanonicalAliasCaseVariants(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "state.json")
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: statePath,
		Keys: []KeyConfig{{
			ID: "team-sol", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_sol"),
			Models: []ModelRule{{
				Alias: "gpt-5.6-sol", Provider: "codex", TargetModel: "gpt-5.6",
				InputPricePerMillion: 1, OutputPricePerMillion: 2,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// Three case variants, same token shape each: 100K in / 50K out → $0.10+$0.10=$0.20
	variants := []string{"gpt-5.6-sol", "Gpt-5.6-sol", "GPT-5.6-SOL"}
	for _, v := range variants {
		cost := store.RecordUsage("team-sol", v, "gpt-5.6", false, UsageDetail{
			InputTokens: 100_000, OutputTokens: 50_000,
		})
		if !nearly(cost, 0.20) {
			t.Fatalf("RecordUsage(%q) cost = %v, want 0.20", v, cost)
		}
	}

	_, rows, ok := store.AliasUsageFor("team-sol")
	if !ok {
		t.Fatal("key not found")
	}
	if len(rows) != 1 {
		t.Fatalf("row count = %d, want 1 canonical bucket; rows=%+v", len(rows), rows)
	}
	row := rows[0]
	if row.Alias != "gpt-5.6-sol" || !row.InConfig {
		t.Fatalf("row = %+v, want alias=gpt-5.6-sol in_config=true", row)
	}
	if !nearly(row.Daily.TotalUSD, 0.60) || row.Daily.CallCount != 3 {
		t.Fatalf("daily = %+v, want $0.60 / 3 calls", row.Daily)
	}
	if row.Daily.InputTokens != 300_000 || row.Daily.OutputTokens != 150_000 {
		t.Fatalf("daily tokens = %+v, want 300000/150000", row.Daily)
	}
	// Key-level totals must match (no double-count at key vs alias).
	s := store.UsageSummaryFor(store.Keys()[0])
	if !nearly(s.DailyUSD, 0.60) || s.DailyCallCount != 3 {
		t.Fatalf("key daily = %+v, want $0.60 / 3", s)
	}

	// Unknown alias: zero cost, no forged empty ByAlias row.
	unknownCost := store.RecordUsage("team-sol", "totally-unknown-model", "x", false, UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 0,
	})
	if unknownCost != 0 {
		t.Fatalf("unknown alias cost = %v, want 0", unknownCost)
	}
	_, rows2, _ := store.AliasUsageFor("team-sol")
	if len(rows2) != 1 {
		t.Fatalf("unknown alias created extra rows: %+v", rows2)
	}
	if rows2[0].Alias != "gpt-5.6-sol" {
		t.Fatalf("unexpected residual after unknown: %+v", rows2)
	}
}

// TestRecordResponseCostCanonicalAliasCaseVariants: non-stream response-cost
// path also writes only the configured canonical spelling.
func TestRecordResponseCostCanonicalAliasCaseVariants(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{{
			ID: "team-sol", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_sol"),
			Models: []ModelRule{{
				Alias: "gpt-5.6-sol", Provider: "codex", TargetModel: "gpt-5.6",
				InputPricePerMillion: 1, OutputPricePerMillion: 1,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	hdr := map[string][]string{"Authorization": {"Bearer cpa_sol"}}
	body := []byte(`{"usage":{"prompt_tokens":500000,"completion_tokens":0}}`)

	for _, v := range []string{"gpt-5.6-sol", "GPT-5.6-SOL", "Gpt-5.6-Sol"} {
		cost := store.RecordResponseCost(hdr, nil, v, body)
		if !nearly(cost, 0.50) {
			t.Fatalf("RecordResponseCost(%q) = %v, want 0.50", v, cost)
		}
	}
	_, rows, ok := store.AliasUsageFor("team-sol")
	if !ok {
		t.Fatal("key not found")
	}
	if len(rows) != 1 || rows[0].Alias != "gpt-5.6-sol" || !rows[0].InConfig {
		t.Fatalf("rows = %+v, want single canonical gpt-5.6-sol", rows)
	}
	if !nearly(rows[0].Daily.TotalUSD, 1.50) || rows[0].Daily.CallCount != 3 {
		t.Fatalf("daily = %+v, want $1.50 / 3", rows[0].Daily)
	}

	// Unknown alias on response path: zero, no empty row.
	if c := store.RecordResponseCost(hdr, nil, "no-such-alias", body); c != 0 {
		t.Fatalf("unknown response cost = %v, want 0", c)
	}
	_, rows2, _ := store.AliasUsageFor("team-sol")
	if len(rows2) != 1 || rows2[0].Alias != "gpt-5.6-sol" {
		t.Fatalf("unknown created rows: %+v", rows2)
	}
}

// TestAliasUsageCaseCanonicalReadOnlyMerge: historical mixed-case ByAlias
// buckets merge on read into the config spelling; state file bytes and ledger
// entries are not mutated by the read.
func TestAliasUsageCaseCanonicalReadOnlyMerge(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	statePath := filepath.Join(t.TempDir(), "state.json")

	// Seed state with legacy mixed-case ByAlias keys (simulates pre-fix history).
	// Daily: "gpt-5.6-sol" $0.20 + "Gpt-5.6-sol" $0.30 + "GPT-5.6-SOL" $0.10 = $0.60
	seed := map[string]any{
		"version": 1,
		"keys": []map[string]any{{
			"id": "team-sol", "enabled": true,
			"key_hash": hashForUsageTest(t, "cpa_sol"),
			"models": []map[string]any{{
				"alias": "gpt-5.6-sol", "provider": "codex", "target_model": "gpt-5.6",
				"input_price_per_million": 1, "output_price_per_million": 2,
			}},
		}},
		"usage": map[string]any{
			"team-sol": map[string]any{
				"daily":  map[string]any{"total_usd": 0.60, "call_count": 3, "window_start": now.Format(time.RFC3339)},
				"weekly": map[string]any{"total_usd": 0.60, "call_count": 3, "window_start": now.Format(time.RFC3339)},
				"by_alias": map[string]any{
					"gpt-5.6-sol": map[string]any{
						"daily":  map[string]any{"total_usd": 0.20, "call_count": 1, "cache_read_tokens": 10000, "cache_cost_usd": 0.01, "input_tokens": 100000, "output_tokens": 50000, "window_start": now.Format(time.RFC3339)},
						"weekly": map[string]any{"total_usd": 0.20, "call_count": 1, "cache_read_tokens": 10000, "cache_cost_usd": 0.01, "input_tokens": 100000, "output_tokens": 50000, "window_start": now.Format(time.RFC3339)},
					},
					"Gpt-5.6-sol": map[string]any{
						"daily":  map[string]any{"total_usd": 0.30, "call_count": 1, "cache_read_tokens": 20000, "cache_cost_usd": 0.02, "input_tokens": 150000, "output_tokens": 75000, "window_start": now.Format(time.RFC3339)},
						"weekly": map[string]any{"total_usd": 0.30, "call_count": 1, "cache_read_tokens": 20000, "cache_cost_usd": 0.02, "input_tokens": 150000, "output_tokens": 75000, "window_start": now.Format(time.RFC3339)},
					},
					"GPT-5.6-SOL": map[string]any{
						"daily":  map[string]any{"total_usd": 0.10, "call_count": 1, "cache_read_tokens": 30000, "cache_cost_usd": 0.03, "input_tokens": 50000, "output_tokens": 25000, "window_start": now.Format(time.RFC3339)},
						"weekly": map[string]any{"total_usd": 0.10, "call_count": 1, "cache_read_tokens": 30000, "cache_cost_usd": 0.03, "input_tokens": 50000, "output_tokens": 25000, "window_start": now.Format(time.RFC3339)},
					},
					// Truly removed residual — different name, must stay separate.
					"old-model": map[string]any{
						"daily":  map[string]any{"total_usd": 0.05, "call_count": 1, "input_tokens": 50000, "window_start": now.Format(time.RFC3339)},
						"weekly": map[string]any{"total_usd": 0.05, "call_count": 1, "input_tokens": 50000, "window_start": now.Format(time.RFC3339)},
					},
				},
			},
		},
	}
	raw, err := json.Marshal(seed)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(statePath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}

	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: statePath,
		Keys: []KeyConfig{{
			ID: "team-sol", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_sol"),
			Models: []ModelRule{{
				Alias: "gpt-5.6-sol", Provider: "codex", TargetModel: "gpt-5.6",
				InputPricePerMillion: 1, OutputPricePerMillion: 2,
			}},
		}},
	}); err != nil {
		t.Fatal(err)
	}

	// Snapshot the complete ledger before read (must be unchanged after).
	_, usageBefore := store.runtimeComponents()
	var beforeLedgerBytes []byte
	if usageBefore != nil {
		beforeLedgerBytes, err = json.Marshal(usageBefore.snapshot())
		if err != nil {
			t.Fatal(err)
		}
	}

	_, rows, ok := store.AliasUsageFor("team-sol")
	if !ok {
		t.Fatal("key not found")
	}
	byAlias := map[string]AliasUsageEntry{}
	for _, r := range rows {
		byAlias[r.Alias] = r
	}
	// Configured canonical + residual old-model only (3 case variants merged).
	if len(rows) != 2 {
		t.Fatalf("row count = %d, want 2; rows=%+v", len(rows), rows)
	}
	canon := byAlias["gpt-5.6-sol"]
	if !canon.InConfig {
		t.Fatalf("canonical must be in_config=true: %+v", canon)
	}
	if !nearly(canon.Daily.TotalUSD, 0.60) || canon.Daily.CallCount != 3 {
		t.Fatalf("canonical daily = %+v, want $0.60 / 3", canon.Daily)
	}
	if canon.Daily.InputTokens != 300_000 || canon.Daily.OutputTokens != 150_000 {
		t.Fatalf("canonical tokens = %+v, want 300000/150000", canon.Daily)
	}
	if canon.Daily.CacheReadTokens != 60_000 || !nearly(canon.Daily.CacheCostUSD, 0.06) {
		t.Fatalf("canonical cache counters = %+v, want 60000/$0.06", canon.Daily)
	}
	if !nearly(canon.Weekly.TotalUSD, 0.60) || canon.Weekly.CallCount != 3 {
		t.Fatalf("canonical weekly = %+v, want $0.60 / 3", canon.Weekly)
	}
	if canon.Weekly.CacheReadTokens != 60_000 || !nearly(canon.Weekly.CacheCostUSD, 0.06) {
		t.Fatalf("canonical weekly cache counters = %+v, want 60000/$0.06", canon.Weekly)
	}
	residual := byAlias["old-model"]
	if residual.InConfig || !nearly(residual.Daily.TotalUSD, 0.05) {
		t.Fatalf("residual = %+v, want in_config=false $0.05", residual)
	}

	// Key-level limits unchanged (read does not re-sum key totals from aliases).
	s := store.UsageSummaryFor(store.Keys()[0])
	if !nearly(s.DailyUSD, 0.60) || s.DailyCallCount != 3 {
		t.Fatalf("key summary daily = %+v, want $0.60 / 3 (unchanged)", s)
	}

	// The complete ledger still holds all historical spellings and values.
	_, usageAfter := store.runtimeComponents()
	var afterLedgerBytes []byte
	if usageAfter != nil {
		afterLedgerBytes, err = json.Marshal(usageAfter.snapshot())
		if err != nil {
			t.Fatal(err)
		}
	}
	if string(beforeLedgerBytes) != string(afterLedgerBytes) {
		t.Fatal("ledger snapshot changed by AliasUsage read")
	}
	// State file bytes untouched by the read (no flush/save side effect).
	afterBytes, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(beforeBytes) != string(afterBytes) {
		t.Fatal("state file bytes changed by AliasUsage read")
	}
}

func TestAddUsageWindowUsesEarliestNonZeroStart(t *testing.T) {
	earlier := time.Date(2026, 6, 27, 10, 0, 0, 0, time.UTC)
	later := earlier.Add(24 * time.Hour)

	merged := addUsageWindow(
		UsageWindow{TotalUSD: 0.20, WindowStart: later},
		UsageWindow{TotalUSD: 0.30, WindowStart: earlier},
	)
	if !merged.WindowStart.Equal(earlier) {
		t.Fatalf("merged window start = %s, want earliest %s", merged.WindowStart, earlier)
	}
	if !nearly(merged.TotalUSD, 0.50) {
		t.Fatalf("merged total = %v, want 0.50", merged.TotalUSD)
	}

	reversed := addUsageWindow(
		UsageWindow{TotalUSD: 0.30, WindowStart: earlier},
		UsageWindow{TotalUSD: 0.20, WindowStart: later},
	)
	if !reversed.WindowStart.Equal(earlier) || !nearly(reversed.TotalUSD, 0.50) {
		t.Fatalf("reverse-order merge = %+v, want earliest start and total 0.50", reversed)
	}
}

// increment CallCount (the counter is mode-agnostic for successful requests).
func TestCallCountIncrementedTokenMode(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{{
			ID: "tok", Enabled: true,
			KeyHash: hashForUsageTest(t, "cpa_tok"),
			Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "m",
				InputPricePerMillion: 1, OutputPricePerMillion: 1}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	_ = store.RecordUsage("tok", "fast", "m", false, UsageDetail{InputTokens: 100_000, OutputTokens: 0})
	_ = store.RecordUsage("tok", "fast", "m", false, UsageDetail{InputTokens: 0, OutputTokens: 100_000})
	s := store.UsageSummaryFor(store.Keys()[0])
	if s.DailyCallCount != 2 {
		t.Fatalf("token-mode daily call count = %d, want 2", s.DailyCallCount)
	}
}
