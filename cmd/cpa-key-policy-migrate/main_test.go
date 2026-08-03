package main

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"

	"cpa-key-policy/internal/nativeaccess"
	"cpa-key-policy/internal/policy"
)

func TestRunStagesLosslessMigration(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "legacy.json")
	configPath := filepath.Join(dir, "config.yaml")
	outputConfig := filepath.Join(dir, "staged", "config.yaml")
	outputPolicy := filepath.Join(dir, "staged", "native.json")
	const plain = "sk-issued"
	hash, _ := policy.HashKey(plain)
	aliases := []policy.AliasMapping{{
		Alias: "codex-csil-gpt-5.6-sol",
		Targets: []policy.AliasTarget{{
			Provider: "codex", TargetModel: "codex-csil/gpt-5.6-sol", Group: "classify:csil",
		}},
	}}
	keys := []policy.KeyConfig{{
		ID: "issued", PlainKey: plain, KeyHash: hash, Enabled: true, RPM: 12,
		Aliases: []policy.KeyAliasRef{{Alias: aliases[0].Alias}},
	}}
	if err := policy.SaveState(legacyPath, keys, nil, aliases, nil); err != nil {
		t.Fatal(err)
	}
	source := []byte("port: 8317\napi-keys: []\nforce-model-prefix: true\n")
	if err := os.WriteFile(configPath, source, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := run(legacyPath, configPath, outputConfig, outputPolicy, false); err != nil {
		t.Fatal(err)
	}
	unchanged, _ := os.ReadFile(configPath)
	if string(unchanged) != string(source) {
		t.Fatal("source config was mutated")
	}
	staged, _ := os.ReadFile(outputConfig)
	if !bytes.Contains(staged, []byte("api-keys:\n  - \"")) {
		t.Fatalf("staged api-keys must use a block sequence")
	}
	store, err := nativeaccess.New(outputConfig, outputPolicy)
	if err != nil {
		t.Fatal(err)
	}
	for _, model := range []string{"gpt-5.6-sol", "codex-csil-gpt-5.6-sol"} {
		decision := store.Authenticate(plain, model, false)
		if !decision.Allowed || decision.TargetModel != "codex-csil/gpt-5.6-sol" {
			t.Fatalf("staged policy rejected %q: %#v", model, decision)
		}
	}
}

func TestRunRejectsUnmanagedExistingNativeKey(t *testing.T) {
	dir := t.TempDir()
	legacyPath := filepath.Join(dir, "legacy.json")
	configPath := filepath.Join(dir, "config.yaml")
	const plain = "sk-issued"
	hash, _ := policy.HashKey(plain)
	if err := policy.SaveState(legacyPath, []policy.KeyConfig{{
		ID: "issued", PlainKey: plain, KeyHash: hash, Enabled: true,
		Models: []policy.ModelRule{{Alias: "m", Provider: "p", TargetModel: "m"}},
	}}, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("api-keys:\n  - \"sk-unmanaged\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run(
		legacyPath,
		configPath,
		filepath.Join(dir, "out.yaml"),
		filepath.Join(dir, "out.json"),
		false,
	)
	if err == nil {
		t.Fatal("unmanaged existing native key must fail closed")
	}
}
