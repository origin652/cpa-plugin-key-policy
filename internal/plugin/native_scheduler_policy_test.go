package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"cpa-key-policy/internal/nativeaccess"
)

func configureNativeSchedulerApp(t *testing.T, grants []nativeaccess.Grant) (*App, string) {
	t.Helper()
	dir := t.TempDir()
	keysFile := filepath.Join(dir, "config.yaml")
	stateFile := filepath.Join(dir, "native-state.json")
	const key = "sk-server-side-routing"
	if err := os.WriteFile(keysFile, []byte("api-keys:\n  - "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := nativeaccess.New(keysFile, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(nativeaccess.Policy{
		KeyHash: nativeaccess.HashKey(key),
		Enabled: true,
		Grants:  grants,
	}); err != nil {
		t.Fatal(err)
	}
	app := NewApp()
	configYAML := []byte(
		"enabled: true\n" +
			"mode: native-access\n" +
			"state_file: " + filepath.ToSlash(filepath.Join(dir, "legacy-unused.json")) + "\n" +
			"native_keys_file: " + filepath.ToSlash(keysFile) + "\n" +
			"native_state_file: " + filepath.ToSlash(stateFile) + "\n" +
			"classify_rules:\n" +
			"  - name: dongwu-oauth\n" +
			"    field: filename\n" +
			"    pattern: dongwu\n" +
			"    group: dongwu\n" +
			"    enabled: true\n" +
			"  - name: csil-oauth\n" +
			"    field: filename\n" +
			"    pattern: csil\n" +
			"    group: csil\n" +
			"    enabled: true\n",
	)
	request, _ := json.Marshal(LifecycleRequest{ConfigYAML: configYAML})
	if _, err := app.HandleMethod(MethodPluginReconfigure, request); err != nil {
		t.Fatal(err)
	}
	return app, nativeaccess.HashKey(key)
}

func runNativeScheduler(t *testing.T, app *App, keyHash string, candidates []SchedulerAuthCandidate) (SchedulerPickResponse, Envelope) {
	t.Helper()
	request, _ := json.Marshal(SchedulerPickRequest{
		Model: "gpt-5.6-sol",
		Options: SchedulerPickOptions{Metadata: map[string]any{
			"key_hash":        keyHash,
			"requested_model": "gpt-5.6-sol",
		}},
		Candidates: candidates,
	})
	raw, err := app.HandleMethod(MethodSchedulerPick, request)
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	var response SchedulerPickResponse
	if envelope.OK {
		if err := json.Unmarshal(envelope.Result, &response); err != nil {
			t.Fatal(err)
		}
	}
	return response, envelope
}

func TestNativeSchedulerLocksKeyToOneCredentialGroup(t *testing.T) {
	app, keyHash := configureNativeSchedulerApp(t, []nativeaccess.Grant{{
		Provider: "codex",
		Model:    "gpt-5.6-sol",
		Group:    "classify:csil",
	}})
	response, envelope := runNativeScheduler(t, app, keyHash, []SchedulerAuthCandidate{
		{ID: "codex-dongwu.json", Provider: "codex"},
		{ID: "codex-csil.json", Provider: "codex"},
	})
	if !envelope.OK || !response.Handled || response.AuthID != "codex-csil.json" {
		t.Fatalf("expected CSiL-only selection, response=%#v envelope=%#v", response, envelope)
	}
}

func TestNativeSchedulerAllowsUnionOfCredentialGroups(t *testing.T) {
	app, keyHash := configureNativeSchedulerApp(t, []nativeaccess.Grant{
		{Provider: "codex", Model: "gpt-5.6-sol", Group: "classify:dongwu"},
		{Provider: "codex", Model: "gpt-5.6-sol", Group: "classify:csil"},
	})
	response, envelope := runNativeScheduler(t, app, keyHash, []SchedulerAuthCandidate{
		{ID: "codex-dongwu.json", Provider: "codex"},
		{ID: "codex-csil.json", Provider: "codex"},
	})
	if !envelope.OK || response.Handled {
		t.Fatalf("all usable candidates are authorized, so CPA must schedule: response=%#v envelope=%#v", response, envelope)
	}
}

func TestNativeSchedulerFiltersProviderSubset(t *testing.T) {
	app, keyHash := configureNativeSchedulerApp(t, []nativeaccess.Grant{
		{Provider: "codex", Model: "gpt-5.6-sol"},
	})
	response, envelope := runNativeScheduler(t, app, keyHash, []SchedulerAuthCandidate{
		{ID: "codex.json", Provider: "codex"},
		{ID: "copilot.json", Provider: "github-copilot"},
	})
	if !envelope.OK || !response.Handled || response.AuthID != "codex.json" {
		t.Fatalf("expected provider subset selection, response=%#v envelope=%#v", response, envelope)
	}
}

func TestNativeSchedulerMatchesOpenAICompatibleProviderName(t *testing.T) {
	app, keyHash := configureNativeSchedulerApp(t, []nativeaccess.Grant{
		{Provider: "siliconflow", Model: "gpt-5.6-sol"},
	})
	response, envelope := runNativeScheduler(t, app, keyHash, []SchedulerAuthCandidate{
		{ID: "codex.json", Provider: "codex"},
		{ID: "siliconflow.json", Provider: "openai-compatible-siliconflow"},
	})
	if !envelope.OK || !response.Handled || response.AuthID != "siliconflow.json" {
		t.Fatalf("expected OpenAI-compatible provider selection, response=%#v envelope=%#v", response, envelope)
	}
}
