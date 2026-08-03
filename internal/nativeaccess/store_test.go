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
