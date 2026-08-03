package plugin

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"cpa-key-policy/internal/nativeaccess"
)

func TestNativeCompatibilityPrefixAndCredentialGroup(t *testing.T) {
	dir := t.TempDir()
	keysFile := filepath.Join(dir, "config.yaml")
	stateFile := filepath.Join(dir, "native-state.json")
	const key = "sk-existing-downstream"
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
		Grants: []nativeaccess.Grant{
			{
				Provider:         "codex",
				Model:            "gpt-5.6-*",
				Group:            "classify:csil",
				UpstreamPrefix:   "codex-csil",
				AcceptedPrefixes: []string{"codex-csil-"},
			},
			{
				Provider:         "codex",
				Model:            "gpt-5.3-codex-spark",
				Group:            "classify:csil",
				UpstreamPrefix:   "codex-csil",
				AcceptedPrefixes: []string{"codex-csil-"},
			},
		},
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
			"  - name: csil-oauth\n" +
			"    field: filename\n" +
			"    pattern: csil\n" +
			"    group: csil\n" +
			"    enabled: true\n",
	)
	configRequest, _ := json.Marshal(LifecycleRequest{ConfigYAML: configYAML})
	if _, err := app.HandleMethod(MethodPluginReconfigure, configRequest); err != nil {
		t.Fatal(err)
	}
	registration := app.managementRegistration()
	if len(registration.Resources) != 1 || registration.Resources[0].Path != "/index.html" {
		t.Fatalf("native mode must register its resource UI: %#v", registration.Resources)
	}
	resourceRequest, _ := json.Marshal(ManagementRequest{
		Method: "GET",
		Path:   "/v0/resource/plugins/cpa-key-policy/index.html",
	})
	rawResource, errResource := app.HandleMethod(MethodManagementHandle, resourceRequest)
	if errResource != nil {
		t.Fatal(errResource)
	}
	var resource ManagementResponse
	if err := unmarshalOK(rawResource, &resource); err != nil {
		t.Fatal(err)
	}
	if resource.StatusCode != 200 || len(resource.Body) == 0 {
		t.Fatalf("native resource UI unavailable: %#v", resource)
	}

	requests := map[string]string{
		"gpt-5.6-sol":                    "codex-csil/gpt-5.6-sol",
		"codex-csil-gpt-5.6-sol":         "codex-csil/gpt-5.6-sol",
		"codex-csil-gpt-5.6-terra":       "codex-csil/gpt-5.6-terra",
		"codex-csil-gpt-5.6-luna":        "codex-csil/gpt-5.6-luna",
		"codex-csil-gpt-5.3-codex-spark": "codex-csil/gpt-5.3-codex-spark",
	}
	for requested, expectedTarget := range requests {
		body := []byte(`{"model":"` + requested + `"}`)
		authRequest, _ := json.Marshal(FrontendAuthRequest{
			Method:  "POST",
			Path:    "/v1/responses",
			Headers: map[string][]string{"Authorization": {"Bearer " + key}},
			Body:    body,
		})
		rawAuth, errAuth := app.HandleMethod(MethodFrontendAuthAuthenticate, authRequest)
		if errAuth != nil {
			t.Fatal(errAuth)
		}
		var auth FrontendAuthResponse
		if err := unmarshalOK(rawAuth, &auth); err != nil {
			t.Fatal(err)
		}
		if !auth.Authenticated || auth.Metadata["group"] != "classify:csil" {
			t.Fatalf("unexpected auth for %q: %#v", requested, auth)
		}

		routeRequest, _ := json.Marshal(ModelRouteRequest{
			RequestedModel:     requested,
			Headers:            map[string][]string{"Authorization": {"Bearer " + key}},
			AvailableProviders: []string{"codex"},
		})
		rawRoute, errRoute := app.HandleMethod(MethodModelRoute, routeRequest)
		if errRoute != nil {
			t.Fatal(errRoute)
		}
		var route ModelRouteResponse
		if err := unmarshalOK(rawRoute, &route); err != nil {
			t.Fatal(err)
		}
		if !route.Handled || route.Target != "codex" || route.TargetModel != expectedTarget {
			t.Fatalf("unexpected route for %q: %#v", requested, route)
		}
	}

	schedulerRequest, _ := json.Marshal(SchedulerPickRequest{
		Provider: "codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "classify:csil"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-dongwu.json", Provider: "codex"},
			{ID: "codex-csil.json", Provider: "codex"},
		},
	})
	rawScheduler, errScheduler := app.HandleMethod(MethodSchedulerPick, schedulerRequest)
	if errScheduler != nil {
		t.Fatal(errScheduler)
	}
	var scheduler SchedulerPickResponse
	if err := unmarshalOK(rawScheduler, &scheduler); err != nil {
		t.Fatal(err)
	}
	if !scheduler.Handled || scheduler.AuthID != "codex-csil.json" {
		t.Fatalf("scheduler crossed credential groups: %#v", scheduler)
	}
}
