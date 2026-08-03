package nativeaccess

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNativeKeyIsSingleIdentitySource(t *testing.T) {
	dir := t.TempDir()
	keysFile := filepath.Join(dir, "config.yaml")
	stateFile := filepath.Join(dir, "state.json")
	const key = "sk-native-one"
	if err := os.WriteFile(keysFile, []byte("api-keys:\n  - "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(keysFile, stateFile)
	if err != nil {
		t.Fatal(err)
	}
	policy := Policy{
		KeyHash: HashKey(key),
		Enabled: true,
		Grants: []Grant{{Provider: "codex", Model: "codex-csil-gpt-5.6-sol"}},
	}
	if err := store.Upsert(policy); err != nil {
		t.Fatal(err)
	}
	if got := store.Authenticate(key, "codex-csil-gpt-5.6-sol", false); !got.Allowed {
		t.Fatalf("expected allowed, got %#v", got)
	}
	if got := store.Authenticate(key, "gpt-5.6-sol", false); got.Allowed || got.Reason != "model_not_allowed" {
		t.Fatalf("expected model denial, got %#v", got)
	}
	if err := os.WriteFile(keysFile, []byte("api-keys: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := store.Authenticate(key, "codex-csil-gpt-5.6-sol", false); got.Known {
		t.Fatalf("deleted native key must be revoked immediately, got %#v", got)
	}
}

func TestPolicyCannotCreateAKey(t *testing.T) {
	dir := t.TempDir()
	keysFile := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(keysFile, []byte("api-keys: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(keysFile, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	err = store.Upsert(Policy{
		KeyHash: HashKey("not-native"),
		Enabled: true,
		Grants:  []Grant{{Provider: "codex", Model: "gpt-5.6-sol"}},
	})
	if err == nil {
		t.Fatal("policy must not create or authorize a non-native key")
	}
}

func TestBulkApplyIsAtomic(t *testing.T) {
	dir := t.TempDir()
	keysFile := filepath.Join(dir, "config.yaml")
	const active = "sk-active"
	if err := os.WriteFile(keysFile, []byte("api-keys:\n  - "+active+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(keysFile, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	valid := Policy{
		KeyHash: HashKey(active),
		Enabled: true,
		Grants:  []Grant{{Provider: "codex", Model: "gpt-5.6-sol"}},
	}
	invalid := Policy{
		KeyHash: HashKey("not-active"),
		Enabled: true,
		Grants:  []Grant{{Provider: "kimi", Model: "kimi-k2.5"}},
	}
	if _, err := store.Apply([]Policy{valid, invalid}, true, false); err == nil {
		t.Fatal("expected the complete transaction to fail")
	}
	if len(store.Policies()) != 0 {
		t.Fatal("failed bulk validation must not partially persist policies")
	}
	if _, err := store.Apply([]Policy{valid}, true, true); err != nil {
		t.Fatal(err)
	}
	if len(store.Policies()) != 0 {
		t.Fatal("dry run must not mutate policies")
	}
	if _, err := store.Apply([]Policy{valid}, true, false); err != nil {
		t.Fatal(err)
	}
	if len(store.Policies()) != 1 {
		t.Fatal("validated replacement should persist exactly one policy")
	}
}

func TestWildcardGrantsAndProviderConstraint(t *testing.T) {
	dir := t.TempDir()
	keysFile := filepath.Join(dir, "config.yaml")
	const key = "sk-wildcard"
	if err := os.WriteFile(keysFile, []byte("api-keys:\n  - "+key+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := New(keysFile, filepath.Join(dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Upsert(Policy{
		KeyHash: HashKey(key),
		Enabled: true,
		Grants: []Grant{
			{Provider: "kimi", Model: "*"},
			{Provider: "codex", Model: "gpt-5.6-*"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if got := store.Authenticate(key, "gpt-5.6-sol", false); !got.Allowed || got.Provider != "codex" {
		t.Fatalf("expected codex wildcard grant, got %#v", got)
	}
	if provider, ok := store.RouteProvider(key, "gpt-5.6-sol"); !ok || provider != "codex" {
		t.Fatalf("expected provider constraint codex, got %q %v", provider, ok)
	}
	if got := store.Authenticate(key, "kimi-k2.5", false); !got.Allowed || got.Provider != "kimi" {
		t.Fatalf("expected kimi all-model grant, got %#v", got)
	}
	if got := store.Authenticate(key, "deepseek-v4", false); got.Allowed {
		t.Fatalf("unexpected wildcard overreach: %#v", got)
	}
	models, handled := store.FilterModels(key, []map[string]any{
		{"id": "gpt-5.6-sol"},
		{"id": "kimi-k2.5"},
		{"id": "deepseek-v4"},
	})
	if !handled || len(models) != 2 {
		t.Fatalf("expected two visible models, handled=%v models=%#v", handled, models)
	}
}
