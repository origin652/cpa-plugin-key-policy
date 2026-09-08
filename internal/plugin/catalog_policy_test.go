package plugin

import (
	"encoding/json"
	"testing"
)

func TestDecodeCatalogPolicyValidatesAndDefaultsSource(t *testing.T) {
	policy, err := decodeCatalogPolicy([]byte(`
catalog_groups:
  - name: pi
    keys: ["team-*", "dev"]
    include_unlisted: true
    models:
      - id: fast
        patch:
          context_window: 262144
      - id: gemini
        source: gemini-3.8-flash-high
`))
	if err != nil {
		t.Fatal(err)
	}
	if len(policy.Groups) != 1 || len(policy.Groups[0].Models) != 2 {
		t.Fatalf("unexpected policy: %+v", policy)
	}
	if got := policy.Groups[0].Models[0].Source; got != "fast" {
		t.Fatalf("default source = %q, want fast", got)
	}
	if !policy.Groups[0].IncludeUnlisted {
		t.Fatal("include_unlisted was not decoded")
	}
	if !catalogGroupMatchesKey(policy.Groups[0], "team-a") {
		t.Fatal("team-* did not match team-a")
	}
}

func TestDecodeCatalogPolicyRejectsDuplicateModelIDs(t *testing.T) {
	_, err := decodeCatalogPolicy([]byte(`
catalog_groups:
  - name: pi
    keys: ["*"]
    models:
      - id: fast
      - id: FAST
`))
	if err == nil {
		t.Fatal("expected duplicate model id error")
	}
}

func TestRewriteCatalogBodyCodexAliasesAndPatches(t *testing.T) {
	body := []byte(`{
  "models": [
    {
      "slug": "gemini-3.8-flash-high",
      "display_name": "Gemini 3.8 Flash",
      "context_window": 1048576,
      "max_context_window": 1048576,
      "max_tokens": 65536,
      "nested": {"keep": true, "value": 1}
    },
    {"slug": "blocked", "context_window": 128000}
  ]
}`)
	patched, err := rewriteCatalogBody(body, []CatalogModel{
		{
			ID:     "fast",
			Source: "gemini-3.8-flash-high",
			Patch: map[string]any{
				"context_window":     262144,
				"max_context_window": 262144,
				"max_tokens":         32768,
				"nested": map[string]any{
					"value": 2,
				},
			},
			Remove: []string{"display_name"},
		},
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(patched, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 {
		t.Fatalf("models = %+v, want one", out.Models)
	}
	model := out.Models[0]
	if model["slug"] != "fast" {
		t.Fatalf("slug = %#v, want fast", model["slug"])
	}
	if model["context_window"] != float64(262144) || model["max_context_window"] != float64(262144) {
		t.Fatalf("context patch failed: %+v", model)
	}
	if model["max_tokens"] != float64(32768) {
		t.Fatalf("max_tokens = %#v", model["max_tokens"])
	}
	if _, exists := model["display_name"]; exists {
		t.Fatalf("display_name was not removed: %+v", model)
	}
	nested, ok := model["nested"].(map[string]any)
	if !ok || nested["keep"] != true || nested["value"] != float64(2) {
		t.Fatalf("deep merge failed: %#v", model["nested"])
	}
}

func TestRewriteCatalogBodyOpenAI(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"id":"gpt-5.4","object":"model"},{"id":"blocked","object":"model"}]}`)
	patched, err := rewriteCatalogBody(body, []CatalogModel{{ID: "coding", Source: "gpt-5.4"}}, false)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(patched, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0]["id"] != "coding" {
		t.Fatalf("unexpected data: %+v", out.Data)
	}
}

func TestRewriteCatalogBodyPatchesSelectedAndKeepsUnlisted(t *testing.T) {
	body := []byte(`{"models":[{"slug":"gpt-6-astra","context_window":272000},{"slug":"gpt-5.6-sol","context_window":272000},{"slug":"gpt-5.6-terra","context_window":272000}]}`)
	patched, err := rewriteCatalogBody(body, []CatalogModel{
		{ID: "gpt-6-astra", Source: "gpt-6-astra", Patch: map[string]any{"context_window": 600000}},
		{ID: "gpt-5.6-sol", Source: "gpt-5.6-sol", Patch: map[string]any{"context_window": 372000}},
	}, true)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(patched, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 3 {
		t.Fatalf("models = %+v, want patched pair plus unlisted model", out.Models)
	}
	if out.Models[0]["slug"] != "gpt-6-astra" || out.Models[0]["context_window"] != float64(600000) {
		t.Fatalf("astra patch missing: %+v", out.Models[0])
	}
	if out.Models[1]["slug"] != "gpt-5.6-sol" || out.Models[1]["context_window"] != float64(372000) {
		t.Fatalf("sol patch missing: %+v", out.Models[1])
	}
	if out.Models[2]["slug"] != "gpt-5.6-terra" || out.Models[2]["context_window"] != float64(272000) {
		t.Fatalf("unlisted model changed: %+v", out.Models[2])
	}
}

func TestRewriteModelCatalogUsesAuthenticatedKeyMetadata(t *testing.T) {
	app := NewApp()
	app.setCatalogPolicy(catalogPolicy{Groups: []CatalogGroup{
		{
			Name: "team",
			Keys: []string{"team-*"},
			Models: []CatalogModel{
				{ID: "fast", Source: "gemini-3.8-flash-high", Patch: map[string]any{"context_window": 262144}},
			},
		},
	}})

	req := ResponseInterceptRequest{
		SourceFormat: modelCatalogSourceCodex,
		Body:         []byte(`{"models":[{"slug":"gemini-3.8-flash-high","context_window":1048576}]}`),
		Metadata: map[string]any{
			"access_provider": "plugin:cpa-key-policy:cpa-key-policy",
			"access_metadata": map[string]any{"key_id": "team-a"},
		},
	}
	body, changed := app.rewriteModelCatalog(req)
	if !changed {
		t.Fatal("catalog was not rewritten")
	}
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0]["slug"] != "fast" || out.Models[0]["context_window"] != float64(262144) {
		t.Fatalf("unexpected rewritten catalog: %+v", out.Models)
	}
}

func TestCatalogProviderMatchesPlugin(t *testing.T) {
	tests := []struct {
		provider string
		want     bool
	}{
		{provider: PluginID, want: true},
		{provider: "plugin:cpa-key-policy:cpa-key-policy", want: true},
		{provider: "plugin:CPA-KEY-POLICY:frontend", want: true},
		{provider: "plugin:other:cpa-key-policy", want: false},
		{provider: "some-other-auth-plugin", want: false},
	}
	for _, tt := range tests {
		t.Run(tt.provider, func(t *testing.T) {
			if got := catalogProviderMatchesPlugin(tt.provider); got != tt.want {
				t.Fatalf("catalogProviderMatchesPlugin(%q) = %v, want %v", tt.provider, got, tt.want)
			}
		})
	}
}

func TestRewriteModelCatalogUnmatchedPluginKeyGetsEmptyCatalog(t *testing.T) {
	app := NewApp()
	app.setCatalogPolicy(catalogPolicy{Groups: []CatalogGroup{{Name: "team", Keys: []string{"team-*"}, Models: []CatalogModel{{ID: "fast"}}}}})

	body, changed := app.rewriteModelCatalog(ResponseInterceptRequest{
		SourceFormat: modelCatalogSourceCodex,
		Body:         []byte(`{"models":[{"slug":"fast"}]}`),
		Metadata: map[string]any{
			"access_provider": PluginID,
			"access_metadata": map[string]any{"key_id": "other"},
		},
	})
	if !changed {
		t.Fatal("expected unmatched plugin key to receive an empty catalog")
	}
	if string(body) != `{"models":[]}` {
		t.Fatalf("body = %s, want empty catalog", body)
	}
}

func TestRewriteModelCatalogLeavesNativeKeysUntouched(t *testing.T) {
	app := NewApp()
	app.setCatalogPolicy(catalogPolicy{Groups: []CatalogGroup{{Name: "all", Keys: []string{"*"}, Models: []CatalogModel{{ID: "fast"}}}}})

	body, changed := app.rewriteModelCatalog(ResponseInterceptRequest{
		SourceFormat: modelCatalogSourceCodex,
		Body:         []byte(`{"models":[{"slug":"fast"}]}`),
	})
	if changed || body != nil {
		t.Fatalf("native catalog unexpectedly changed: changed=%v body=%s", changed, body)
	}
}

func TestRewriteModelCatalogLeavesOtherAuthProviderUntouched(t *testing.T) {
	app := NewApp()
	app.setCatalogPolicy(catalogPolicy{Groups: []CatalogGroup{{Name: "all", Keys: []string{"*"}, Models: []CatalogModel{{ID: "fast"}}}}})

	body, changed := app.rewriteModelCatalog(ResponseInterceptRequest{
		SourceFormat: modelCatalogSourceCodex,
		Body:         []byte(`{"models":[{"slug":"fast"}]}`),
		Metadata: map[string]any{
			"access_provider": "some-other-auth-plugin",
			"access_metadata": map[string]any{"key_id": "team-a"},
		},
	})
	if changed || body != nil {
		t.Fatalf("other auth provider catalog unexpectedly changed: changed=%v body=%s", changed, body)
	}
}

func TestRewriteModelCatalogFailsClosedOnUnexpectedHostShape(t *testing.T) {
	app := NewApp()
	app.setCatalogPolicy(catalogPolicy{Groups: []CatalogGroup{{Name: "all", Keys: []string{"*"}, Models: []CatalogModel{{ID: "fast"}}}}})

	body, changed := app.rewriteModelCatalog(ResponseInterceptRequest{
		SourceFormat: modelCatalogSourceCodex,
		Body:         []byte(`{"unexpected":[{"slug":"fast"}]}`),
		Metadata: map[string]any{
			"access_provider": PluginID,
			"access_metadata": map[string]any{"key_id": "team-a"},
		},
	})
	if !changed {
		t.Fatal("expected restricted catalog to fail closed")
	}
	if string(body) != `{"models":[]}` {
		t.Fatalf("body = %s, want empty Codex catalog", body)
	}
}

func TestInterceptResponseFiltersModelCatalogBeforeAliasRewrite(t *testing.T) {
	app := NewApp()
	app.setCatalogPolicy(catalogPolicy{Groups: []CatalogGroup{{
		Name: "pi",
		Keys: []string{"team-a"},
		Models: []CatalogModel{{
			ID: "fast", Source: "gemini-3.8-flash-high", Patch: map[string]any{"context_window": 262144},
		}},
	}}})

	rawReq, err := json.Marshal(ResponseInterceptRequest{
		SourceFormat: modelCatalogSourceCodex,
		Body:         []byte(`{"models":[{"slug":"gemini-3.8-flash-high","context_window":1048576}]}`),
		Metadata: map[string]any{
			"access_provider": PluginID,
			"access_metadata": map[string]any{"key_id": "team-a"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := app.interceptResponse(rawReq)
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
	if len(resp.Body) == 0 {
		t.Fatal("expected rewritten catalog body")
	}
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0]["slug"] != "fast" {
		t.Fatalf("unexpected catalog: %+v", out.Models)
	}
}
