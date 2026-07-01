package policy

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParseTokenUsageOpenAI(t *testing.T) {
	u := ParseTokenUsage([]byte(`{"usage":{"prompt_tokens":120,"completion_tokens":40}}`))
	if !u.Found || u.PromptTokens != 120 || u.CompletionTokens != 40 {
		t.Fatalf("got %+v", u)
	}
}

func TestParseTokenUsageAnthropic(t *testing.T) {
	u := ParseTokenUsage([]byte(`{"usage":{"input_tokens":300,"output_tokens":90}}`))
	if !u.Found || u.PromptTokens != 300 || u.CompletionTokens != 90 {
		t.Fatalf("got %+v", u)
	}
}

func TestParseTokenUsageGemini(t *testing.T) {
	u := ParseTokenUsage([]byte(`{"usage_metadata":{"promptTokenCount":77,"candidatesTokenCount":23}}`))
	if !u.Found || u.PromptTokens != 77 || u.CompletionTokens != 23 {
		t.Fatalf("got %+v", u)
	}
}

func TestParseTokenUsageStreamingEmpty(t *testing.T) {
	if u := ParseTokenUsage(nil); u.Found {
		t.Fatalf("nil body should be unfound: %+v", u)
	}
	if u := ParseTokenUsage([]byte(`not json`)); u.Found {
		t.Fatalf("garbage should be unfound: %+v", u)
	}
	// usage object present but zero tokens → treat as unfound (no billing).
	if u := ParseTokenUsage([]byte(`{"usage":{"prompt_tokens":0,"completion_tokens":0}}`)); u.Found {
		t.Fatalf("zero usage should be unfound: %+v", u)
	}
}

func TestParseTokenUsageSSEOpenAIStyle(t *testing.T) {
	// OpenAI streaming: usage is in the final chunk's data frame.
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":50,\"completion_tokens\":30}}\n\ndata: [DONE]\n\n")
	u := ParseTokenUsage(body)
	if !u.Found || u.PromptTokens != 50 || u.CompletionTokens != 30 {
		t.Fatalf("got %+v", u)
	}
}

func TestParseTokenUsageSSEAnthropicCumulative(t *testing.T) {
	// Anthropic streams input_tokens in message_start and output_tokens in a
	// later message_delta frame — cumulative, so max() across frames = final.
	body := []byte(strings.Join([]string{
		`event: message_start`,
		`data: {"type":"message_start","message":{"usage":{"input_tokens":120,"output_tokens":1}}}`,
		``,
		`event: message_delta`,
		`data: {"type":"message_delta","usage":{"output_tokens":45}}`,
		``,
		`data: [DONE]`,
		``,
	}, "\n"))
	u := ParseTokenUsage(body)
	if !u.Found || u.PromptTokens != 120 || u.CompletionTokens != 45 {
		t.Fatalf("got %+v, want 120/45 (cumulative)", u)
	}
}

func TestParseTokenUsageSSENoUsageFrame(t *testing.T) {
	// SSE with no usage frame → unfound (not billed).
	body := []byte("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n")
	if u := ParseTokenUsage(body); u.Found {
		t.Fatalf("sse without usage should be unfound: %+v", u)
	}
}

func TestComputeCost(t *testing.T) {
	// 1M prompt tokens at $3/M + 0.5M completion at $15/M = 3 + 7.5 = 10.5
	usage := TokenUsage{PromptTokens: 1_000_000, CompletionTokens: 500_000, Found: true}
	got := ComputeCost(3, 15, true, usage)
	if !nearly(got, 10.5) {
		t.Fatalf("cost = %v, want 10.5", got)
	}
	// Unknown alias (priced=false) → 0 cost.
	if c := ComputeCost(3, 15, false, usage); c != 0 {
		t.Fatalf("unpriced cost = %v, want 0", c)
	}
	// No usage found → 0 cost even if priced.
	if c := ComputeCost(3, 15, true, TokenUsage{}); c != 0 {
		t.Fatalf("no-usage cost = %v, want 0", c)
	}
}

func TestPriceForAlias(t *testing.T) {
	k := &KeyConfig{Models: []ModelRule{{Alias: "fast", InputPricePerMillion: 2, OutputPricePerMillion: 8, CacheReadPricePerMillion: 0.2}}}
	in, out, cache, ok := k.PriceForAlias("Fast") // case-insensitive
	if !ok || in != 2 || out != 8 || cache != 0.2 {
		t.Fatalf("got in=%v out=%v cache=%v ok=%v", in, out, cache, ok)
	}
	if _, _, _, ok := k.PriceForAlias("missing"); ok {
		t.Fatal("missing alias should not be priced")
	}
}

func nearly(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// TestComputeCacheCostSubsetProvider: OpenAI-style — cache hits are a SUBSET of
// InputTokens. 1M input total, 200K of which are cached. Input $3/M, output
// $15/M, cache-read $0.30/M. Expected: (1M-200K)*3 + 200K*0.30 + 0.5M*15
// = 800K*3/1M + 200K*0.3/1M + 500K*15/1M = 2.4 + 0.06 + 7.5 = 9.96.
// Verify the cached subset is NOT double-billed at the input price.
func TestComputeCacheCostSubsetProvider(t *testing.T) {
	detail := UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 500_000,
		CachedTokens: 200_000, // subset of input
	}
	got := ComputeCacheCost("openai", 3, 15, 0.30, true, detail)
	if !nearly(got, 9.96) {
		t.Fatalf("subset cache cost = %v, want 9.96", got)
	}
}

// TestComputeCacheCostAdditiveProvider: Anthropic — cache reads are OUTSIDE
// InputTokens. 800K regular input + 200K cache-read, 500K output. Cache-creation
// tokens (writes) billed at input price. Input $3/M, output $15/M, cache-read
// $0.30/M, plus 100K cache-creation. Expected: 800K*3 + 200K*0.30 + 100K*3 +
// 500K*15 = (900K*3 + 200K*0.3 + 500K*15)/1M = 2.7 + 0.06 + 7.5 = 10.26.
func TestComputeCacheCostAdditiveProvider(t *testing.T) {
	detail := UsageDetail{
		InputTokens:         800_000,
		OutputTokens:        500_000,
		CacheReadTokens:     200_000,
		CacheCreationTokens: 100_000,
	}
	got := ComputeCacheCost("claude", 3, 15, 0.30, true, detail)
	if !nearly(got, 10.26) {
		t.Fatalf("additive cache cost = %v, want 10.26", got)
	}
}

// TestComputeCacheCostNoCachePriceFallsBackToInput: with cacheReadPerMillion=0,
// cache hits fall back to the input price. Subset provider: input already
// includes cache, so total == plain input+output cost (no double count).
// 1M input (incl 200K cached) + 500K output @ $3/$15 → 3 + 7.5 = 10.5.
func TestComputeCacheCostNoCachePriceFallsBackToInput(t *testing.T) {
	detail := UsageDetail{InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000}
	got := ComputeCacheCost("openai", 3, 15, 0, true, detail)
	if !nearly(got, 10.5) {
		t.Fatalf("fallback cost = %v, want 10.5", got)
	}
	// Additive provider: cache reads are outside input, so the fallback total
	// must add them at the input price (matching pre-cache-pricing behavior).
	// 800K input + 200K cacheRead + 500K output @ $3/$15 → 1M*3 + 0.5M*15 = 10.5.
	detail2 := UsageDetail{InputTokens: 800_000, OutputTokens: 500_000, CacheReadTokens: 200_000}
	got2 := ComputeCacheCost("claude", 3, 15, 0, true, detail2)
	if !nearly(got2, 10.5) {
		t.Fatalf("additive fallback cost = %v, want 10.5", got2)
	}
}

// TestComputeCacheCostUnpricedZero: unknown alias (priced=false) → 0 even with
// tokens and cache configured.
func TestComputeCacheCostUnpricedZero(t *testing.T) {
	detail := UsageDetail{InputTokens: 1_000_000, OutputTokens: 1_000_000, CachedTokens: 500_000}
	if c := ComputeCacheCost("openai", 3, 15, 0.3, false, detail); c != 0 {
		t.Fatalf("unpriced cost = %v, want 0", c)
	}
}

// TestComputeCacheCostBreakdown verifies the cache-spend / cache-hit breakdown
// returned alongside the total, used by the ledger for hit-rate + cache spend.
// Subset provider, explicit cache price: 1M input (incl 200K cached) + 500K
// output @ $3/$15/$0.30. Total 9.96 (as above). cacheCost = 200K×0.30/1M = 0.06.
// cacheReadTokens = 200K. nonCache input billed at input price = 800K.
func TestComputeCacheCostBreakdown(t *testing.T) {
	detail := UsageDetail{InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000}
	total, cacheCost, cacheRead := ComputeCacheCostBreakdown("openai", 3, 15, 0.30, true, detail)
	if !nearly(total, 9.96) {
		t.Fatalf("total = %v, want 9.96", total)
	}
	if !nearly(cacheCost, 0.06) {
		t.Fatalf("cacheCost = %v, want 0.06", cacheCost)
	}
	if cacheRead != 200_000 {
		t.Fatalf("cacheRead = %d, want 200000", cacheRead)
	}
}

// TestComputeCacheCostBreakdownNoCachePrice: when no cache price is configured,
// cache hits fold into the input-price line. cacheCost must be 0 (not separably
// priced) even though cacheRead is still reported for hit-rate accounting.
func TestComputeCacheCostBreakdownNoCachePrice(t *testing.T) {
	detail := UsageDetail{InputTokens: 1_000_000, OutputTokens: 500_000, CachedTokens: 200_000}
	total, cacheCost, cacheRead := ComputeCacheCostBreakdown("openai", 3, 15, 0, true, detail)
	if !nearly(total, 10.5) {
		t.Fatalf("total = %v, want 10.5", total)
	}
	if cacheCost != 0 {
		t.Fatalf("cacheCost = %v, want 0 (no separable cache spend without a cache price)", cacheCost)
	}
	if cacheRead != 200_000 {
		t.Fatalf("cacheRead = %d, want 200000 (still reported for hit-rate)", cacheRead)
	}
}

// TestComputeCacheCostBreakdownAdditive: Claude — cache reads outside input.
// 800K input + 200K cacheRead + 100K cacheCreation + 500K output @ $3/$15/$0.30.
// Total 10.26 (as above). cacheCost = 200K×0.30/1M = 0.06. cacheRead = 200K.
// cacheCreation tokens are NOT counted as cacheRead (they are writes).
func TestComputeCacheCostBreakdownAdditive(t *testing.T) {
	detail := UsageDetail{
		InputTokens:         800_000,
		OutputTokens:        500_000,
		CacheReadTokens:     200_000,
		CacheCreationTokens: 100_000,
	}
	total, cacheCost, cacheRead := ComputeCacheCostBreakdown("claude", 3, 15, 0.30, true, detail)
	if !nearly(total, 10.26) {
		t.Fatalf("total = %v, want 10.26", total)
	}
	if !nearly(cacheCost, 0.06) {
		t.Fatalf("cacheCost = %v, want 0.06", cacheCost)
	}
	if cacheRead != 200_000 {
		t.Fatalf("cacheRead = %d, want 200000 (creation excluded)", cacheRead)
	}
}

// the policy layer: RecordUsage bills from already-parsed token counts (as
// delivered by usage.handle), with no response body to parse. Previously only
// RecordResponseCost existed, which required a parseable body — unreachable
// for streams. 1M input × $1/M = $1.00 == daily limit → next auth blocked.
func TestRecordUsageBillsFromParsedTokens(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{{
			ID: "streamy", Enabled: true, DailyLimitUSD: 1.00,
			KeyHash: hashForUsageTest(t, "cpa_stream"),
			Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex",
				InputPricePerMillion: 1, OutputPricePerMillion: 1}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	hdr := map[string][]string{"Authorization": {"Bearer cpa_stream"}}

	// No body involved — usage came pre-parsed from the host's usage.handle.
	cost := store.RecordUsage("cpa_stream", "fast", "gpt-5-codex", UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 0, TotalTokens: 1_000_000,
	})
	if !nearly(cost, 1.0) {
		t.Fatalf("cost = %v, want 1.0", cost)
	}
	d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("streaming usage should be billed & block: %+v", d)
	}
}

// TestRecordUsageUnknownKeyZeroCost: usage for a key not in our config bills
// nothing (the host fires usage.handle for all keys, including non-managed ones).
func TestRecordUsageUnknownKeyZeroCost(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{{
			ID: "k", Enabled: true, DailyLimitUSD: 0.01,
			KeyHash: hashForUsageTest(t, "cpa_known"),
			Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex",
				InputPricePerMillion: 1, OutputPricePerMillion: 1}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	cost := store.RecordUsage("cpa_unknown", "fast", "gpt-5-codex", UsageDetail{
		InputTokens: 1_000_000, OutputTokens: 1_000_000,
	})
	if cost != 0 {
		t.Fatalf("unknown key should cost 0, got %v", cost)
	}
}

// TestRecordUsageMatchesByID verifies the real host wire value: CPA forwards
// our auth Principal (key.ID) as the UsageRecord.APIKey, not the plaintext
// secret. RecordUsage must resolve the key by ID.
func TestRecordUsageMatchesByID(t *testing.T) {
	now := time.Date(2026, 6, 29, 10, 0, 0, 0, time.UTC)
	store := NewStore()
	store.SetClock(func() time.Time { return now })
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{{
			ID: "team-x", Enabled: true, DailyLimitUSD: 0.50,
			KeyHash: hashForUsageTest(t, "cpa_secret_xyz"),
			Models: []ModelRule{{Alias: "fast", Provider: "codex", TargetModel: "gpt-5-codex",
				InputPricePerMillion: 1, OutputPricePerMillion: 1}},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	hdr := map[string][]string{"Authorization": {"Bearer cpa_secret_xyz"}}

	// Host sends key.ID ("team-x"), NOT the secret. Must still bill.
	cost := store.RecordUsage("team-x", "fast", "gpt-5-codex", UsageDetail{
		InputTokens: 500_000, OutputTokens: 0, TotalTokens: 500_000,
	})
	if !nearly(cost, 0.50) {
		t.Fatalf("cost = %v, want 0.50", cost)
	}
	d := store.Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("ID-matched usage should bill & block: %+v", d)
	}
}
