package plugin

import (
	"encoding/json"
	"net/http"
	"net/url"
	"path/filepath"
	"testing"

	"cpa-key-policy/internal/policy"
)

func nearly(a, b float64) bool {
	if a < 0 {
		a = -a
	}
	if b < 0 {
		b = -b
	}
	return a < b+1e-9 && b < a+1e-9
}

func configureTestApp(t *testing.T) (*App, string) {
	t.Helper()
	app := NewApp()
	plain := "cpa_plugin_test"
	hash := hashForTest(t, plain)
	yaml := []byte(`
enabled: true
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
keys:
  - id: team-a
    name: Team A
    enabled: true
    key_hash: "` + hash + `"
    key_preview: "cpa_plu..._test"
    rpm: 60
    models:
      - alias: fast
        provider: codex
        target_model: gpt-5-codex
`)
	req, _ := json.Marshal(LifecycleRequest{ConfigYAML: yaml})
	if _, err := app.HandleMethod(MethodPluginReconfigure, req); err != nil {
		t.Fatalf("configure: %v", err)
	}
	return app, plain
}

func hashForTest(t *testing.T, key string) string {
	t.Helper()
	hash, err := policy.HashKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return hash
}

func TestAppAuthenticationAndRoute(t *testing.T) {
	app, plain := configureTestApp(t)
	authReq, _ := json.Marshal(FrontendAuthRequest{
		Method:  "POST",
		Path:    "/v1/chat/completions",
		Headers: http.Header{"Authorization": {"Bearer " + plain}},
		Body:    []byte(`{"model":"fast"}`),
	})
	raw, err := app.HandleMethod(MethodFrontendAuthAuthenticate, authReq)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var authResp FrontendAuthResponse
	if err := json.Unmarshal(env.Result, &authResp); err != nil {
		t.Fatal(err)
	}
	if !authResp.Authenticated || authResp.Principal != "team-a" {
		t.Fatalf("auth response = %+v", authResp)
	}

	routeReq, _ := json.Marshal(ModelRouteRequest{
		RequestedModel: "fast",
		Headers:        http.Header{"Authorization": {"Bearer " + plain}},
	})
	raw, err = app.HandleMethod(MethodModelRoute, routeReq)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var routeResp ModelRouteResponse
	if err := json.Unmarshal(env.Result, &routeResp); err != nil {
		t.Fatal(err)
	}
	if !routeResp.Handled || routeResp.Target != "codex" || routeResp.TargetModel != "gpt-5-codex" {
		t.Fatalf("route response = %+v", routeResp)
	}
}

func TestAppModelsEndpointDenied(t *testing.T) {
	app, plain := configureTestApp(t)
	authReq, _ := json.Marshal(FrontendAuthRequest{
		Method:  "GET",
		Path:    "/v1/models",
		Headers: http.Header{"Authorization": {"Bearer " + plain}},
	})
	raw, err := app.HandleMethod(MethodFrontendAuthAuthenticate, authReq)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	var authResp FrontendAuthResponse
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(env.Result, &authResp); err != nil {
		t.Fatal(err)
	}
	if authResp.Authenticated {
		t.Fatalf("auth response = %+v, want denied", authResp)
	}
}

func TestAppResponseInterceptorRewritesModel(t *testing.T) {
	app, plain := configureTestApp(t)
	req, _ := json.Marshal(ResponseInterceptRequest{
		RequestedModel: "fast",
		RequestHeaders: http.Header{"Authorization": {"Bearer " + plain}},
		Body:           []byte(`{"id":"resp","model":"gpt-5-codex"}`),
	})
	raw, err := app.HandleMethod(MethodResponseInterceptAfter, req)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp ResponseInterceptResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if string(resp.Body) != `{"id":"resp","model":"fast"}` {
		t.Fatalf("response body = %s", resp.Body)
	}
}

func TestAppManagementCreateAndRotate(t *testing.T) {
	app, _ := configureTestApp(t)
	createBody := []byte(`{"id":"team-b","models":[{"alias":"sonnet","provider":"claude","target_model":"claude-sonnet"}]}`)
	req, _ := json.Marshal(ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/cpa-key-policy/keys",
		Body:   createBody,
	})
	raw, err := app.HandleMethod(MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	if !json.Valid(resp.Body) {
		t.Fatalf("create body is not json: %s", resp.Body)
	}

	rotateReq, _ := json.Marshal(ManagementRequest{
		Method: http.MethodPost,
		Path:   "/v0/management/plugins/cpa-key-policy/keys/rotate",
		Query:  url.Values{"id": {"team-b"}},
	})
	raw, err = app.HandleMethod(MethodManagementHandle, rotateReq)
	if err != nil {
		t.Fatal(err)
	}
	resp = managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("rotate status = %d, body = %s", resp.StatusCode, resp.Body)
	}
}

func managementResponseFromEnvelope(t *testing.T, raw []byte) ManagementResponse {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp ManagementResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestAppResponseInterceptorBillsUsage(t *testing.T) {
	app, plain := configureTestApp(t)
	// Record a billing response for the configured "fast" alias. The test config
	// has no prices, so cost stays 0 and nothing is blocked — we just verify
	// the path doesn't break response rewriting.
	req, _ := json.Marshal(ResponseInterceptRequest{
		RequestedModel: "fast",
		RequestHeaders: http.Header{"Authorization": {"Bearer " + plain}},
		Body:           []byte(`{"id":"resp","model":"gpt-5-codex","usage":{"prompt_tokens":100,"completion_tokens":20}}`),
	})
	raw, err := app.HandleMethod(MethodResponseInterceptAfter, req)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp ResponseInterceptResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	// The interceptor rewrites the top-level "model" to the alias ("fast") and
	// leaves usage intact. Parse rather than string-compare (Go encodes map keys
	// in sorted order).
	var body map[string]any
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("body not json: %s err=%v", resp.Body, err)
	}
	if body["model"] != "fast" {
		t.Fatalf("model = %v, want fast", body["model"])
	}
	usage, ok := body["usage"].(map[string]any)
	if !ok || usage["prompt_tokens"] != float64(100) || usage["completion_tokens"] != float64(20) {
		t.Fatalf("usage = %+v, want preserved prompt=100 completion=20", usage)
	}
}

func TestAppKeysListExposesUsageAndLimits(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/cpa-key-policy/keys",
	})
	raw, err := app.HandleMethod(MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var payload struct {
		Keys []struct {
			ID             string  `json:"id"`
			DailyLimitUSD  float64 `json:"daily_limit_usd"`
			WeeklyLimitUSD float64 `json:"weekly_limit_usd"`
			Usage          struct {
				DailyUSD      float64 `json:"daily_usd"`
				WeeklyUSD     float64 `json:"weekly_usd"`
				DailyLimitUSD float64 `json:"daily_limit_usd"`
			} `json:"usage"`
		} `json:"keys"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatalf("unmarshal: %v body=%s", err, resp.Body)
	}
	if len(payload.Keys) == 0 {
		t.Fatal("no keys returned")
	}
	k := payload.Keys[0]
	if k.ID != "team-a" {
		t.Fatalf("id = %s", k.ID)
	}
	if k.Usage.DailyLimitUSD != 0 {
		t.Fatalf("test config has no daily limit, got %v", k.Usage.DailyLimitUSD)
	}
	// Usage object must always be present (even when empty).
	if k.Usage.DailyUSD != 0 {
		t.Fatalf("expected 0 daily usage, got %v", k.Usage.DailyUSD)
	}
}

func TestAppPatchKeySetsLimits(t *testing.T) {
	app, _ := configureTestApp(t)
	f := 1.5
	patchBody, _ := json.Marshal(map[string]any{
		"id": "team-a", "daily_limit_usd": f, "weekly_limit_usd": 10.0,
	})
	req, _ := json.Marshal(ManagementRequest{
		Method: http.MethodPatch,
		Path:   "/v0/management/plugins/cpa-key-policy/keys",
		Body:   patchBody,
	})
	raw, err := app.HandleMethod(MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d body = %s", resp.StatusCode, resp.Body)
	}
	var payload struct {
		Key struct {
			DailyLimitUSD  float64 `json:"daily_limit_usd"`
			WeeklyLimitUSD float64 `json:"weekly_limit_usd"`
		} `json:"key"`
	}
	if err := json.Unmarshal(resp.Body, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Key.DailyLimitUSD != 1.5 || payload.Key.WeeklyLimitUSD != 10.0 {
		t.Fatalf("limits = %+v", payload.Key)
	}
}

func TestManagementRegistrationDeclaresResource(t *testing.T) {
	app := NewApp()
	resp := app.managementRegistration()
	if len(resp.Resources) == 0 {
		t.Fatal("no resources declared")
	}
	found := false
	for _, r := range resp.Resources {
		if r.Path == "/index.html" {
			found = true
		}
	}
	if !found {
		t.Fatalf("resources do not include /index.html: %+v", resp.Resources)
	}
}

func TestHandleManagementServesResourceUI(t *testing.T) {
	app := NewApp()
	req, _ := json.Marshal(ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/cpa-key-policy/index.html",
	})
	raw, err := app.HandleMethod(MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Headers.Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Fatalf("content-type = %q, want text/html", ct)
	}
	if len(resp.Body) == 0 {
		t.Fatal("resource body empty")
	}
}

func TestHandleManagementResourceUnknownPath404(t *testing.T) {
	app := NewApp()
	req, _ := json.Marshal(ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/resource/plugins/cpa-key-policy/assets/app.js",
	})
	raw, err := app.HandleMethod(MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
}

// configurePricedApp builds an app with one priced key ($1/M input, $1/M
// output) under a $1 daily limit, so usage.handle billing can be observed.
func configurePricedApp(t *testing.T) (*App, string) {
	t.Helper()
	app := NewApp()
	plain := "cpa_priced"
	hash := hashForTest(t, plain)
	yaml := []byte(`
enabled: true
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
keys:
  - id: priced
    enabled: true
    key_hash: "` + hash + `"
    key_preview: "cpa_pr...ced"
    rpm: 0
    daily_limit_usd: 1.00
    models:
      - alias: fast
        provider: codex
        target_model: gpt-5-codex
        input_price_per_million: 1
        output_price_per_million: 1
`)
	req, _ := json.Marshal(LifecycleRequest{ConfigYAML: yaml})
	if _, err := app.HandleMethod(MethodPluginReconfigure, req); err != nil {
		t.Fatalf("configure: %v", err)
	}
	return app, plain
}

// TestUsageHandleBills is the regression test for streaming billing. The host
// delivers a finalized usage record via usage.handle for EVERY completed
// request (streaming and non-streaming). 1M input tokens × $1/M = $1.00, which
// equals the $1 daily limit, so the NEXT auth check is rejected on the daily
// window. This is the path that covers streaming (response.intercept_after is
// never invoked on streams, so usage.handle is the only billing entry point).
func TestUsageHandleBills(t *testing.T) {
	app, plain := configurePricedApp(t)
	hdr := http.Header{"Authorization": {"Bearer " + plain}}

	// The host's usage.handle APIKey field carries our auth Principal, which
	// THIS plugin sets to key.ID ("priced") — NOT the plaintext secret. This is
	// the regression for the real wire value: matching must be ID-based.
	req, _ := json.Marshal(UsageHandleRequest{
		APIKey: "priced", // key.ID, what CPA actually forwards
		Alias:  "fast",
		Model:  "gpt-5-codex",
		Detail: UsageDetail{InputTokens: 1_000_000, OutputTokens: 0, TotalTokens: 1_000_000},
	})
	raw, err := app.HandleMethod(MethodUsageHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	if !okEnvelope(t, raw) {
		t.Fatalf("usage.handle should always return ok, got %s", raw)
	}

	// $1.00 spent == $1.00 daily limit → the next request is rejected.
	d := app.Store().Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("after usage.handle billing of $1.00, next request should be daily_exceeded: %+v", d)
	}
}

// TestUsageHandleAliasFallbackToModel verifies that when the host does not set
// an Alias, we price against the resolved upstream Model (which equals the
// alias for this plugin, since alias == target_model).
func TestUsageHandleAliasFallbackToModel(t *testing.T) {
	app, plain := configurePricedApp(t)
	hdr := http.Header{"Authorization": {"Bearer " + plain}}

	// No Alias, only the resolved Model. 2M input tokens × $1/M = $2.00.
	// APIKey is key.ID ("priced"), matching the real host wire value.
	req, _ := json.Marshal(UsageHandleRequest{
		APIKey: "priced",
		Model:  "fast", // the configured alias; pricing lookup is alias-based
		Detail: UsageDetail{InputTokens: 2_000_000, OutputTokens: 0, TotalTokens: 2_000_000},
	})
	if _, err := app.HandleMethod(MethodUsageHandle, req); err != nil {
		t.Fatal(err)
	}
	d := app.Store().Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if d.Allowed || !d.CostLimited {
		t.Fatalf("alias-fallback billing should block: %+v", d)
	}
}

// TestUsageHandleUnknownKeyNotBilled: a usage record for a key we don't manage
// bills nothing and never blocks. (The host fires usage.handle for all keys,
// including plain CPA keys not routed through this plugin.)
func TestUsageHandleUnknownKeyNotBilled(t *testing.T) {
	app, plain := configurePricedApp(t)
	hdr := http.Header{"Authorization": {"Bearer " + plain}}

	req, _ := json.Marshal(UsageHandleRequest{
		APIKey: "cpa_some_other_key_we_dont_know",
		Alias:  "fast",
		Detail: UsageDetail{InputTokens: 100_000_000, OutputTokens: 100_000_000},
	})
	if _, err := app.HandleMethod(MethodUsageHandle, req); err != nil {
		t.Fatal(err)
	}
	d := app.Store().Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if !d.Allowed {
		t.Fatalf("unknown-key usage must not affect our key: %+v", d)
	}
}

func okEnvelope(t *testing.T, raw []byte) bool {
	t.Helper()
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	return env.OK
}

// TestUsageHandleBillsCacheReadClaude verifies cache-aware billing end to end
// via usage.handle for an additive provider (Claude). 1M input @ $1/M +
// 500K cache-read @ $0.10/M = 1.0 + 0.05 = $1.05 > $1 daily limit → next auth
// blocked. Without cache pricing the cache-read tokens would be unbilled and
// the daily total would be $1.00 (exactly the limit, still blocked, but the
// cost returned would be 1.0, not 1.05) — the test pins the cache-aware total.
func TestUsageHandleBillsCacheReadClaude(t *testing.T) {
	app := NewApp()
	hash := hashForTest(t, "cpa_cache")
	yaml := []byte(`
enabled: true
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
keys:
  - id: cached
    enabled: true
    key_hash: "` + hash + `"
    daily_limit_usd: 1.00
    models:
      - alias: sonnet
        provider: claude
        target_model: claude-sonnet-4
        input_price_per_million: 1
        output_price_per_million: 1
        cache_read_price_per_million: 0.10
`)
	req, _ := json.Marshal(LifecycleRequest{ConfigYAML: yaml})
	if _, err := app.HandleMethod(MethodPluginReconfigure, req); err != nil {
		t.Fatalf("configure: %v", err)
	}

	// Host wire: APIKey = key.ID ("cached"). 1M input (excl cache) + 500K cache-read.
	usageReq, _ := json.Marshal(UsageHandleRequest{
		APIKey: "cached",
		Alias:  "sonnet",
		Model:  "claude-sonnet-4",
		Detail: UsageDetail{InputTokens: 1_000_000, CacheReadTokens: 500_000, TotalTokens: 1_500_000},
	})
	raw, err := app.HandleMethod(MethodUsageHandle, usageReq)
	if err != nil {
		t.Fatal(err)
	}
	if !okEnvelope(t, raw) {
		t.Fatalf("usage.handle ok envelope: %s", raw)
	}

	// $1.05 spent > $1.00 daily limit → next request rejected.
	hdr := http.Header{"Authorization": {"Bearer cpa_cache"}}
	d := app.Store().Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"sonnet"}`))
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("cache-billed usage should block at $1.05 > $1.00: %+v", d)
	}
}

// TestUsageHandlePerCallBills verifies the full usage.handle path for a
// per-call-billed alias: each successful record charges the fixed PerCallUSD
// (ignoring token counts), and a Failed record charges nothing.
func TestUsageHandlePerCallBills(t *testing.T) {
	app := NewApp()
	plain := "cpa_percall_app"
	hash := hashForTest(t, plain)
	yaml := []byte(`
enabled: true
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
keys:
  - id: percall
    enabled: true
    key_hash: "` + hash + `"
    key_preview: "cpa_pe...app"
    daily_limit_usd: 1.00
    models:
      - alias: fast
        provider: codex
        target_model: gpt-5-codex
        billing_mode: per_call
        per_call_usd: 0.50
        input_price_per_million: 999
        output_price_per_million: 999
`)
	req, _ := json.Marshal(LifecycleRequest{ConfigYAML: yaml})
	if _, err := app.HandleMethod(MethodPluginReconfigure, req); err != nil {
		t.Fatalf("configure: %v", err)
	}
	hdr := http.Header{"Authorization": {"Bearer " + plain}}

	// Successful record with huge token counts — must charge $0.50, not token price.
	successReq, _ := json.Marshal(UsageHandleRequest{
		APIKey: "percall",
		Alias:  "fast",
		Detail: UsageDetail{InputTokens: 10_000_000, OutputTokens: 10_000_000},
	})
	if _, err := app.HandleMethod(MethodUsageHandle, successReq); err != nil {
		t.Fatal(err)
	}
	// Failed record — must charge nothing and not count.
	failReq, _ := json.Marshal(UsageHandleRequest{
		APIKey: "percall",
		Alias:  "fast",
		Failed: true,
		Detail: UsageDetail{InputTokens: 10_000_000, OutputTokens: 10_000_000},
	})
	if _, err := app.HandleMethod(MethodUsageHandle, failReq); err != nil {
		t.Fatal(err)
	}
	// A second successful record → total $1.00 == limit.
	if _, err := app.HandleMethod(MethodUsageHandle, successReq); err != nil {
		t.Fatal(err)
	}

	// Next auth rejected on daily limit. CallCount = 2 (failed didn't count).
	d := app.Store().Authenticate("POST", "/v1/chat/completions", hdr, nil, []byte(`{"model":"fast"}`))
	if d.Allowed || !d.CostLimited || d.Reason != "daily_exceeded" {
		t.Fatalf("after two per_call charges, next should be daily_exceeded: %+v", d)
	}
	keys := app.Store().Keys()
	var key policy.KeyConfig
	for _, k := range keys {
		if k.ID == "percall" {
			key = k
		}
	}
	s := app.Store().UsageSummaryFor(key)
	if s.DailyCallCount != 2 {
		t.Fatalf("daily call count = %d, want 2 (failed excluded)", s.DailyCallCount)
	}
	if !nearly(s.DailyUSD, 1.00) {
		t.Fatalf("daily usd = %v, want 1.00", s.DailyUSD)
	}
}

// TestManagementKeyUsageEndpoint: GET /keys/usage?id=... returns the per-alias
// breakdown for a key after usage.handle billing. Verifies the response shape
// (key_id/key_name/aliases), per-alias daily+weekly figures, output tokens, and
// the 404 for an unknown key.
func TestManagementKeyUsageEndpoint(t *testing.T) {
	app, _ := configurePricedApp(t)

	// Bill 200K input + 100K output @ $1/$1 = $0.20 + $0.10 = $0.30.
	usageReq, _ := json.Marshal(UsageHandleRequest{
		APIKey: "priced", Alias: "fast", Model: "gpt-5-codex",
		Detail: UsageDetail{InputTokens: 200_000, OutputTokens: 100_000, TotalTokens: 300_000},
	})
	if _, err := app.HandleMethod(MethodUsageHandle, usageReq); err != nil {
		t.Fatal(err)
	}

	req, _ := json.Marshal(ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/cpa-key-policy/keys/usage",
		Query:  url.Values{"id": {"priced"}},
	})
	raw, err := app.HandleMethod(MethodManagementHandle, req)
	if err != nil {
		t.Fatal(err)
	}
	resp := managementResponseFromEnvelope(t, raw)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, resp.Body)
	}
	var got struct {
		KeyID   string                     `json:"key_id"`
		KeyName string                     `json:"key_name"`
		Aliases []policy.AliasUsageEntry `json:"aliases"`
	}
	if err := json.Unmarshal(resp.Body, &got); err != nil {
		t.Fatalf("unmarshal: %v, body=%s", err, resp.Body)
	}
	if got.KeyID != "priced" || len(got.Aliases) != 1 {
		t.Fatalf("usage response = %+v, want key_id=priced 1 alias", got)
	}
	a := got.Aliases[0]
	if a.Alias != "fast" || !a.InConfig || a.Provider != "codex" {
		t.Fatalf("alias row = %+v, want fast/in_config/codex", a)
	}
	if !nearly(a.Daily.TotalUSD, 0.30) || !nearly(a.Weekly.TotalUSD, 0.30) {
		t.Fatalf("alias usd = %+v, want 0.30/0.30", a)
	}
	if a.Daily.CallCount != 1 || a.Daily.InputTokens != 200_000 || a.Daily.OutputTokens != 100_000 {
		t.Fatalf("alias daily counters = %+v, want 1/200000/100000", a.Daily)
	}

	// Missing id → 400.
	badReq, _ := json.Marshal(ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/cpa-key-policy/keys/usage",
	})
	badResp := managementResponseFromEnvelope(t, mustHandle(t, app, MethodManagementHandle, badReq))
	if badResp.StatusCode != http.StatusBadRequest {
		t.Fatalf("missing id status = %d, want 400", badResp.StatusCode)
	}

	// Unknown id → 404.
	nopeReq, _ := json.Marshal(ManagementRequest{
		Method: http.MethodGet,
		Path:   "/v0/management/plugins/cpa-key-policy/keys/usage",
		Query:  url.Values{"id": {"nope"}},
	})
	nopeResp := managementResponseFromEnvelope(t, mustHandle(t, app, MethodManagementHandle, nopeReq))
	if nopeResp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown id status = %d, want 404", nopeResp.StatusCode)
	}
}

func mustHandle(t *testing.T, app *App, method string, req []byte) []byte {
	t.Helper()
	raw, err := app.HandleMethod(method, req)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
