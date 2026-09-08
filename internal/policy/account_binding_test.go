package policy

import (
	"net/http"
	"path/filepath"
	"testing"
)

func TestCallerScopeForKeyMatchesCLIProxyAPI(t *testing.T) {
	const want = "07fd5bb88ccc0e9b46658eeadd4c0931be7daf2c5e02bc08e2c397558b01d0e7"
	if got := CallerScopeForKey("team-a"); got != want {
		t.Fatalf("CallerScopeForKey() = %q, want %q", got, want)
	}
}

func TestAccountBindingNormalizesAndMatchesCaseSensitively(t *testing.T) {
	binding := &AccountBinding{Allow: []string{"codex-*.json", "codex-*.json"}}
	if err := normalizeAccountBinding(binding); err != nil {
		t.Fatal(err)
	}
	if binding.Strategy != BindingStrategyWeightedRoundRobin {
		t.Fatalf("default strategy = %q", binding.Strategy)
	}
	if len(binding.Allow) != 1 || !binding.Matches("codex-team.json") {
		t.Fatalf("normalized binding = %+v", binding)
	}
	if binding.Matches("CODEX-team.json") {
		t.Fatal("credential ID matching must remain case-sensitive")
	}
	if err := normalizeAccountBinding(&AccountBinding{Allow: []string{"["}}); err == nil {
		t.Fatal("invalid glob was accepted")
	}
}

func TestAccountBindingEmptyAllowIsRestrictive(t *testing.T) {
	binding := &AccountBinding{}
	if err := normalizeAccountBinding(binding); err != nil {
		t.Fatal(err)
	}
	if binding.Matches("any-auth") {
		t.Fatal("present empty binding must match no credentials")
	}
}

func TestHeaderCredentialsExposeConflicts(t *testing.T) {
	headers := http.Header{
		"Authorization": {"Bearer key-a"},
		"X-Api-Key":    {"key-b"},
	}
	values := HeaderCredentials(headers)
	if len(values) != 2 || values[0] != "key-a" || values[1] != "key-b" {
		t.Fatalf("HeaderCredentials() = %#v", values)
	}
}

func TestStoreResolvesPluginAndNativeCallerScopes(t *testing.T) {
	pluginSecret := "plugin-secret"
	nativeSecret := "native-secret"
	pluginHash, _ := HashKey(pluginSecret)
	nativeHash, _ := HashKey(nativeSecret)
	store := NewStore()
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{
			{ID: "plugin", Enabled: true, KeyHash: pluginHash, AccountBinding: &AccountBinding{Allow: []string{"plugin-*"}}},
			{ID: "native", Enabled: true, Native: true, KeyHash: nativeHash, CallerScope: CallerScopeForKey(nativeSecret), AccountBinding: &AccountBinding{Allow: []string{"native-*"}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	plugin := store.ResolveRequestKey(http.Header{"Authorization": {"Bearer " + pluginSecret}}, map[string]any{CallerScopeMetadataKey: CallerScopeForKey("plugin")})
	if plugin.Key == nil || plugin.Key.ID != "plugin" || !plugin.HeaderMatches || plugin.Conflict {
		t.Fatalf("plugin resolution = %+v", plugin)
	}
	native := store.ResolveRequestKey(http.Header{"Authorization": {"Bearer " + nativeSecret}}, map[string]any{CallerScopeMetadataKey: CallerScopeForKey(nativeSecret)})
	if native.Key == nil || native.Key.ID != "native" || !native.HeaderMatches || native.Conflict {
		t.Fatalf("native resolution = %+v", native)
	}
	decision := store.Authenticate("POST", "/v1/chat/completions", http.Header{"Authorization": {"Bearer " + nativeSecret}}, nil, []byte(`{"model":"x"}`))
	if decision.Known || decision.Reason != "native_key" {
		t.Fatalf("native key must remain host-authenticated: %+v", decision)
	}
}

func TestBoundPluginKeyRejectsQueryOnlyAndConflictsBeforeRateLimit(t *testing.T) {
	secret := "bound-secret"
	hash, _ := HashKey(secret)
	store := NewStore()
	if err := store.Configure(Config{
		Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{{ID: "bound", Enabled: true, KeyHash: hash, RPM: 1, AccountBinding: &AccountBinding{Allow: []string{"account-*"}}}},
	}); err != nil {
		t.Fatal(err)
	}
	queryOnly := store.Authenticate("POST", "/v1/chat/completions", nil, map[string][]string{"api_key": {secret}}, []byte(`{"model":"x"}`))
	if !queryOnly.Known || queryOnly.Allowed || queryOnly.Reason != "header_key_required" {
		t.Fatalf("query-only decision = %+v", queryOnly)
	}
	conflict := store.Authenticate("POST", "/v1/chat/completions", http.Header{
		"Authorization": {"Bearer " + secret}, "X-API-Key": {"different"},
	}, nil, []byte(`{"model":"x"}`))
	if !conflict.Known || conflict.Allowed || conflict.Reason != "credential_conflict" {
		t.Fatalf("conflict decision = %+v", conflict)
	}
	headerQueryConflict := store.Authenticate("POST", "/v1/chat/completions", http.Header{
		"Authorization": {"Bearer " + secret},
	}, map[string][]string{"api_key": {"other"}}, []byte(`{"model":"x"}`))
	if !headerQueryConflict.Known || headerQueryConflict.Allowed || headerQueryConflict.Reason != "credential_conflict" {
		t.Fatalf("header/query conflict decision = %+v", headerQueryConflict)
	}
}

func TestNativeKeyConfigurationRequiresBindingAndScope(t *testing.T) {
	hash, _ := HashKey("native-secret")
	base := Config{Enabled: true, Keys: []KeyConfig{{ID: "native", Enabled: true, Native: true, KeyHash: hash}}}
	if err := normalizeConfig(&base); err == nil {
		t.Fatal("native key without binding/scope was accepted")
	}
}

func TestAccountBoundConfigurationCannotDisablePlugin(t *testing.T) {
	hash, _ := HashKey("bound-secret")
	config := Config{Enabled: false, Keys: []KeyConfig{{ID: "bound", Enabled: true, KeyHash: hash, AccountBinding: &AccountBinding{Allow: []string{"account-*"}}}}}
	if err := normalizeConfig(&config); err == nil {
		t.Fatal("disabled plugin accepted an account-bound key")
	}
}

func TestAccountBoundKeyHashMustBeUnique(t *testing.T) {
	hash, _ := HashKey("shared-secret")
	config := Config{Enabled: true, Keys: []KeyConfig{
		{ID: "legacy", Enabled: true, KeyHash: hash},
		{ID: "bound", Enabled: true, KeyHash: hash, AccountBinding: &AccountBinding{Allow: []string{"account-*"}}},
	}}
	if err := normalizeConfig(&config); err == nil {
		t.Fatal("account-bound duplicate key hash was accepted")
	}
}

func TestUpsertAccountBoundKeyHashMustBeUnique(t *testing.T) {
	hash, _ := HashKey("shared-secret")
	store := NewStore()
	if err := store.Configure(Config{Enabled: true, StateFile: filepath.Join(t.TempDir(), "state.json"), Keys: []KeyConfig{{ID: "legacy", Enabled: true, KeyHash: hash}}}); err != nil {
		t.Fatal(err)
	}
	err := store.UpsertKey(KeyConfig{ID: "bound", Enabled: true, KeyHash: hash, AccountBinding: &AccountBinding{Allow: []string{"account-*"}}}, true)
	if err == nil {
		t.Fatal("management upsert accepted a duplicate protected key hash")
	}
}

func TestRotatePluginKeyPreservesBinding(t *testing.T) {
	oldSecret := "old-secret"
	hash, _ := HashKey(oldSecret)
	store := NewStore()
	if err := store.Configure(Config{
		Enabled:   true,
		StateFile: filepath.Join(t.TempDir(), "state.json"),
		Keys: []KeyConfig{{
			ID: "bound", Enabled: true, KeyHash: hash,
			AccountBinding: &AccountBinding{Allow: []string{"account-a"}, Strategy: BindingStrategyRoundRobin},
		}},
	}); err != nil {
		t.Fatal(err)
	}
	newSecret, rotated, err := store.RotateKey("bound")
	if err != nil {
		t.Fatal(err)
	}
	if rotated.AccountBinding == nil || !rotated.AccountBinding.Matches("account-a") {
		t.Fatalf("binding lost during rotation: %+v", rotated.AccountBinding)
	}
	if store.FindByAPIKey(oldSecret) != nil || store.FindByAPIKey(newSecret) == nil {
		t.Fatal("rotation did not replace the matching secret")
	}
}
