package plugin

import (
	"encoding/json"
	"testing"
)

func TestDecodeCatalogPolicyNormalizesModelSelectors(t *testing.T) {
	policy, err := decodeCatalogPolicy([]byte(`
catalog_groups:
  - name: team
    keys: ["team-*"]
    include_models: [" gpt-5-* ", "GPT-5-*", "claude-sonnet-4", ""]
    exclude_models: ["gpt-5-codex-*"]
    patch:
      context_window: 262144
    remove: ["upgrade", " upgrade ", ""]
`))
	if err != nil {
		t.Fatal(err)
	}
	group := policy.Groups[0]
	if len(group.IncludeModels) != 2 || group.IncludeModels[0] != "gpt-5-*" || group.IncludeModels[1] != "claude-sonnet-4" {
		t.Fatalf("include_models = %v", group.IncludeModels)
	}
	if len(group.ExcludeModels) != 1 || group.ExcludeModels[0] != "gpt-5-codex-*" {
		t.Fatalf("exclude_models = %v", group.ExcludeModels)
	}
	if len(group.Remove) != 1 || group.Remove[0] != "upgrade" {
		t.Fatalf("remove = %v", group.Remove)
	}

	if _, err := decodeCatalogPolicy([]byte(`
catalog_groups:
  - name: bad
    keys: ["*"]
    include_models: ["gpt-*mini"]
`)); err == nil {
		t.Fatal("expected non-prefix wildcard to be rejected")
	}
}

func TestCatalogBulkIncludeExcludeAndFieldPatch(t *testing.T) {
	selection := catalogSelectionForKey([]CatalogGroup{
		{
			Name:          "team",
			Keys:          []string{"team-*"},
			IncludeModels: []string{"gpt-5-*", "claude-sonnet-4"},
			ExcludeModels: []string{"gpt-5-codex-*"},
			Patch: map[string]any{
				"context_window": 262144,
				"nested":         map[string]any{"team": true},
			},
			Remove: []string{"upgrade"},
		},
	}, "team-a")

	body := []byte(`{"models":[
		{"slug":"gpt-5-mini","context_window":1048576,"upgrade":"pro","nested":{"keep":true}},
		{"slug":"gpt-5-codex-preview","context_window":1048576},
		{"slug":"claude-sonnet-4","context_window":200000,"upgrade":"max"},
		{"slug":"gemini-2.5-pro","context_window":1000000}
	]}`)
	patched, err := rewriteCatalogBodyWithSelection(body, selection)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(patched, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 2 {
		t.Fatalf("models = %+v, want gpt-5-mini and claude-sonnet-4", out.Models)
	}
	for _, model := range out.Models {
		if model["context_window"] != float64(262144) {
			t.Fatalf("context patch missing: %+v", model)
		}
		if _, exists := model["upgrade"]; exists {
			t.Fatalf("upgrade was not removed: %+v", model)
		}
		nested, ok := model["nested"].(map[string]any)
		if !ok || nested["team"] != true {
			t.Fatalf("nested patch missing: %+v", model)
		}
	}
	if out.Models[0]["slug"] != "gpt-5-mini" || out.Models[1]["slug"] != "claude-sonnet-4" {
		t.Fatalf("source order/identity changed: %+v", out.Models)
	}
}

func TestCatalogExclusionsWinAcrossMatchingGroups(t *testing.T) {
	selection := catalogSelectionForKey([]CatalogGroup{
		{Name: "base", Keys: []string{"team-a"}, IncludeModels: []string{"gpt-5-*"}},
		{Name: "block-codex", Keys: []string{"team-*"}, ExcludeModels: []string{"gpt-5-codex-*"}},
	}, "team-a")
	body := []byte(`{"object":"list","data":[
		{"id":"gpt-5-mini","object":"model"},
		{"id":"gpt-5-codex-preview","object":"model"}
	]}`)
	patched, err := rewriteCatalogBodyWithSelection(body, selection)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(patched, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Data) != 1 || out.Data[0]["id"] != "gpt-5-mini" {
		t.Fatalf("global exclusion failed: %+v", out.Data)
	}
}

func TestCatalogGroupPatchIsInheritedAndModelPatchWins(t *testing.T) {
	selection := catalogSelectionForKey([]CatalogGroup{{
		Name:  "team",
		Keys:  []string{"team-a"},
		Patch: map[string]any{"context_window": 100, "nested": map[string]any{"base": true, "value": 1}},
		Models: []CatalogModel{{
			ID:     "fast",
			Source: "gpt-5-mini",
			Patch:  map[string]any{"context_window": 200, "nested": map[string]any{"value": 2}},
		}},
	}}, "team-a")
	body := []byte(`{"models":[{"slug":"gpt-5-mini","context_window":1000,"nested":{"source":true}}]}`)
	patched, err := rewriteCatalogBodyWithSelection(body, selection)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(patched, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 1 || out.Models[0]["slug"] != "fast" || out.Models[0]["context_window"] != float64(200) {
		t.Fatalf("specific model patch did not override group patch: %+v", out.Models)
	}
	nested := out.Models[0]["nested"].(map[string]any)
	if nested["source"] != true || nested["base"] != true || nested["value"] != float64(2) {
		t.Fatalf("deep patch merge failed: %+v", nested)
	}
}

func TestCatalogBulkPatchCannotRenameAllModels(t *testing.T) {
	selection := catalogSelectionForKey([]CatalogGroup{{
		Name:            "all",
		Keys:            []string{"*"},
		IncludeUnlisted: true,
		Patch:           map[string]any{"slug": "same", "id": "same", "max_tokens": 4096},
	}}, "team-a")
	body := []byte(`{"models":[{"slug":"one"},{"slug":"two","id":"two-id"}]}`)
	patched, err := rewriteCatalogBodyWithSelection(body, selection)
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(patched, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Models) != 2 || out.Models[0]["slug"] != "one" || out.Models[1]["slug"] != "two" || out.Models[1]["id"] != "two-id" {
		t.Fatalf("bulk patch changed catalog identities: %+v", out.Models)
	}
}
