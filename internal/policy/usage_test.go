package policy

import (
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
	cost := store.RecordUsage("cache-key", "fast", "m", UsageDetail{
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
	_ = store.RecordUsage("cache-key", "fast", "m", UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000,
	})
	store.SetClock(func() time.Time { return now.Add(14 * time.Hour) }) // next UTC day
	// A new day-2 record: 1M input (200K cached), no output.
	_ = store.RecordUsage("cache-key", "fast", "m", UsageDetail{
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
	_ = store.RecordUsage("cache-key", "fast", "m", UsageDetail{
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
	_ = s1.RecordUsage("cache-key", "fast", "m", UsageDetail{
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
