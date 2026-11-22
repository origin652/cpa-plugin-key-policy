package policy

import (
	"encoding/json"
	"net/http"
	"reflect"
	"testing"
)

func allowOnly(ids ...string) func(string) bool {
	set := map[string]bool{}
	for _, id := range ids {
		set[normalizeModelListID(id)] = true
	}
	return func(id string) bool { return set[normalizeModelListID(id)] }
}

func listIDs(t *testing.T, body []byte, field string) []string {
	t.Helper()
	var top map[string]json.RawMessage
	if err := json.Unmarshal(body, &top); err != nil {
		t.Fatalf("invalid JSON %s: %v", body, err)
	}
	var entries []json.RawMessage
	if err := json.Unmarshal(top[field], &entries); err != nil {
		t.Fatalf("field %q: %v", field, err)
	}
	ids := []string{}
	for _, entry := range entries {
		id, _ := modelListEntryID(entry)
		ids = append(ids, id)
	}
	return ids
}

func TestFilterModelListOpenAI(t *testing.T) {
	body := []byte(`{"object":"list","data":[{"id":"claude-haiku-5-5","object":"model","owned_by":"anthropic"},{"id":"opencode-go/claude-haiku-5-5","object":"model"},{"id":"gpt-6.1-sol","object":"model"}]}`)
	out, changed := FilterModelListBody(body, allowOnly("claude-haiku-5-5", "gpt-6.1-sol"))
	if !changed {
		t.Fatal("expected change")
	}
	if got, want := listIDs(t, out, "data"), []string{"claude-haiku-5-5", "gpt-6.1-sol"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	var top map[string]any
	_ = json.Unmarshal(out, &top)
	if top["object"] != "list" {
		t.Fatalf("object field lost: %s", out)
	}
}

func TestFilterModelListCodexCatalogKeepsMetadata(t *testing.T) {
	body := []byte(`{"models":[{"slug":"claude-haiku-5-5","display_name":"Claude Haiku 5.5","default_reasoning_level":"medium","supported_reasoning_levels":[{"effort":"low"},{"effort":"max"}]},{"slug":"opencode-go/claude-haiku-5-5","display_name":"Claude Haiku 5.5"},{"slug":"gpt-5.5","display_name":"GPT 5.5"}]}`)
	out, changed := FilterModelListBody(body, allowOnly("claude-haiku-5-5"))
	if !changed {
		t.Fatal("expected change")
	}
	var parsed struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(out, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Models) != 1 || parsed.Models[0]["slug"] != "claude-haiku-5-5" || parsed.Models[0]["default_reasoning_level"] != "medium" {
		t.Fatalf("models = %+v", parsed.Models)
	}
	if levels, _ := parsed.Models[0]["supported_reasoning_levels"].([]any); len(levels) != 2 {
		t.Fatalf("reasoning levels lost: %+v", parsed.Models[0])
	}
}

func TestFilterModelListAnthropicRefreshesCursorAndUncloaks(t *testing.T) {
	cloakedGPT := claudeCloakedModelPrefix + reverseString("gpt-6.1-sol")
	cloakedOther := claudeCloakedModelPrefix + reverseString("gpt-5.5")
	body := []byte(`{"data":[{"id":"claude-haiku-5-5","type":"model"},{"id":"` + cloakedOther + `","type":"model"},{"id":"` + cloakedGPT + `","type":"model"},{"id":"claude-3-5-haiku-20241022","type":"model"}],"has_more":false,"first_id":"claude-haiku-5-5","last_id":"claude-3-5-haiku-20241022"}`)
	out, changed := FilterModelListBody(body, allowOnly("claude-haiku-5-5", "gpt-6.1-sol"))
	if !changed {
		t.Fatal("expected change")
	}
	if got, want := listIDs(t, out, "data"), []string{"claude-haiku-5-5", cloakedGPT}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	var top map[string]any
	_ = json.Unmarshal(out, &top)
	if top["first_id"] != "claude-haiku-5-5" || top["last_id"] != cloakedGPT || top["has_more"] != false {
		t.Fatalf("cursor not refreshed: %s", out)
	}
}

func TestFilterModelListGemini(t *testing.T) {
	body := []byte(`{"models":[{"name":"models/gemini-3-pro","displayName":"Gemini 3 Pro"},{"name":"models/gemini-3-flash"}]}`)
	out, changed := FilterModelListBody(body, allowOnly("gemini-3-flash"))
	if !changed {
		t.Fatal("expected change")
	}
	if got, want := listIDs(t, out, "models"), []string{"models/gemini-3-flash"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
}

func TestFilterModelListNoChangeCases(t *testing.T) {
	cases := map[string][]byte{
		"all allowed":    []byte(`{"data":[{"id":"fast"}]}`),
		"not a list":     []byte(`{"id":"fast","object":"model"}`),
		"invalid json":   []byte(`not json`),
		"empty body":     nil,
		"entries no ids": []byte(`{"data":[{"object":"model"}]}`),
	}
	for name, body := range cases {
		if out, changed := FilterModelListBody(body, allowOnly("fast")); changed || out != nil {
			t.Fatalf("%s: changed=%v out=%s", name, changed, out)
		}
	}
	if _, changed := FilterModelListBody([]byte(`{"data":[{"id":"x"}]}`), nil); changed {
		t.Fatal("nil predicate must not change the body")
	}
}

func TestFilterModelListEmptyAllowlistReturnsEmptyList(t *testing.T) {
	out, changed := FilterModelListBody([]byte(`{"object":"list","data":[{"id":"a"},{"id":"b"}]}`), allowOnly())
	if !changed {
		t.Fatal("expected change")
	}
	if got := listIDs(t, out, "data"); len(got) != 0 {
		t.Fatalf("ids = %v, want empty", got)
	}
}

func TestStoreModelListAllowed(t *testing.T) {
	store, plain := newTestStore(t)
	allowed, ok := store.ModelListAllowed(http.Header{"Authorization": {"Bearer " + plain}})
	if !ok {
		t.Fatal("plugin key must be recognized")
	}
	if !allowed("fast") || !allowed("FAST") || allowed("gpt-5-codex") || allowed("other") {
		t.Fatal("allowlist must match aliases only, case-insensitively")
	}
	if _, ok := store.ModelListAllowed(http.Header{"Authorization": {"Bearer not-a-plugin-key"}}); ok {
		t.Fatal("unknown keys must keep the global list")
	}
	if _, ok := store.ModelListAllowed(http.Header{}); ok {
		t.Fatal("requests without a key must keep the global list")
	}
	if _, ok := store.ModelListAllowed(http.Header{"X-Api-Key": {plain}}); !ok {
		t.Fatal("X-Api-Key header must be recognized like at authentication")
	}
}
