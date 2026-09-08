package plugin

import (
	"encoding/json"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
)

func TestSchedulerPickNoGroupUsesGlobalWeightedRoundRobin(t *testing.T) {
	app, plain := configureTestApp(t)
	request := SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Options: SchedulerPickOptions{
			Headers:  map[string][]string{"Authorization": {"Bearer " + plain}},
			Metadata: map[string]any{"requested_model": "fast"},
		},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-plus", Provider: "codex", Weight: 4, Attributes: map[string]string{"plan_type": "plus"}},
			{ID: "codex-prolite", Provider: "codex", Weight: 1, Attributes: map[string]string{"plan_type": "prolite"}},
		},
	}

	counts := map[string]int{}
	for index := 0; index < 10; index++ {
		resp := schedulerPickForTest(t, app, request)
		counts[resp.AuthID]++
	}
	if counts["codex-plus"] != 8 || counts["codex-prolite"] != 2 {
		t.Fatalf("期望无组时在全部凭证中按 4:1 分配，实际为 %v", counts)
	}
}

func TestSchedulerPickNoGroupDefersNativeKey(t *testing.T) {
	app, _ := configureTestApp(t)
	request := SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Options: SchedulerPickOptions{
			Headers: map[string][]string{"Authorization": {"Bearer native-key"}},
		},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-a", Provider: "codex", Weight: 4},
			{ID: "codex-b", Provider: "codex", Weight: 1},
		},
	}

	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rawResponse, err := app.HandleMethod(MethodSchedulerPick, rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var response SchedulerPickResponse
	if err := unmarshalOK(rawResponse, &response); err != nil {
		t.Fatal(err)
	}
	if response.Handled {
		t.Fatalf("原生密钥不应被插件的无组调度接管，实际为 %+v", response)
	}
}

func TestSchedulerPickGlobalModeIgnoresReceivedGroup(t *testing.T) {
	app, plain := configureGlobalSchedulerApp(t)
	request := SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Options: SchedulerPickOptions{
			Headers:  map[string][]string{"Authorization": {"Bearer " + plain}},
			Metadata: map[string]any{"group": "plus"},
		},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-plus", Provider: "codex", Weight: 4, Attributes: map[string]string{"plan_type": "plus"}},
			{ID: "codex-prolite", Provider: "codex", Weight: 1, Attributes: map[string]string{"plan_type": "prolite"}},
		},
	}

	counts := map[string]int{}
	for index := 0; index < 10; index++ {
		response := schedulerPickForTest(t, app, request)
		counts[response.AuthID]++
	}
	if counts["codex-plus"] != 8 || counts["codex-prolite"] != 2 {
		t.Fatalf("开启全局模式后应忽略 group 并按 4:1 分配，实际为 %v", counts)
	}
}

func TestSchedulerPickGlobalModeDefersNativeKey(t *testing.T) {
	app, _ := configureGlobalSchedulerApp(t)
	request := SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Options: SchedulerPickOptions{
			Headers:  map[string][]string{"Authorization": {"Bearer native-key"}},
			Metadata: map[string]any{"group": "plus"},
		},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-plus", Provider: "codex", Weight: 4, Attributes: map[string]string{"plan_type": "plus"}},
			{ID: "codex-prolite", Provider: "codex", Weight: 1, Attributes: map[string]string{"plan_type": "prolite"}},
		},
	}

	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rawResponse, err := app.HandleMethod(MethodSchedulerPick, rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var response SchedulerPickResponse
	if err := unmarshalOK(rawResponse, &response); err != nil {
		t.Fatal(err)
	}
	if response.Handled {
		t.Fatalf("全局模式不应接管 CPA 原生密钥，实际为 %+v", response)
	}
}

func configureGlobalSchedulerApp(t *testing.T) (*App, string) {
	t.Helper()
	app := NewApp()
	plain := "cpa_global_weighted_test"
	hash := hashForTest(t, plain)
	configYAML := []byte(`
enabled: true
global_weighted_round_robin: true
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
keys:
  - id: global-weighted
    name: Global Weighted
    enabled: true
    key_hash: "` + hash + `"
    key_preview: "cpa_glo..._test"
    models:
      - alias: fast
        provider: codex
        target_model: gpt-5-codex
`)
	request, err := json.Marshal(LifecycleRequest{ConfigYAML: configYAML})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := app.HandleMethod(MethodPluginReconfigure, request); err != nil {
		t.Fatalf("配置全局加权调度测试应用失败: %v", err)
	}
	if !app.store.GlobalWeightedRoundRobin() {
		t.Fatal("期望全局加权轮询开关已启用")
	}
	return app, plain
}

func TestSchedulerPickFiltersByPlanType(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Options: SchedulerPickOptions{Metadata: map[string]any{
			"group": "team",
		}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-a-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
			{ID: "codex-b-team", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}},
			{ID: "codex-c-plus", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
		},
	})
	raw, err := app.HandleMethod(MethodSchedulerPick, req)
	if err != nil {
		t.Fatal(err)
	}
	var resp SchedulerPickResponse
	if err := unmarshalOK(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || resp.AuthID != "codex-b-team" {
		t.Fatalf("expected team-only pick, got %+v", resp)
	}
}

func TestSchedulerPickPriorityTiebreaksByID(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(SchedulerPickRequest{
		Provider: "codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "team"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-z-team", Provider: "codex", Priority: 5, Attributes: map[string]string{"plan_type": "team"}},
			{ID: "codex-a-team", Provider: "codex", Priority: 5, Attributes: map[string]string{"plan_type": "team"}},
			{ID: "codex-m-team", Provider: "codex", Priority: 9, Attributes: map[string]string{"plan_type": "team"}},
		},
	})
	raw, _ := app.HandleMethod(MethodSchedulerPick, req)
	var resp SchedulerPickResponse
	if err := unmarshalOK(raw, &resp); err != nil {
		t.Fatal(err)
	}
	// Higher priority wins.
	if resp.AuthID != "codex-m-team" {
		t.Fatalf("expected highest priority, got %q", resp.AuthID)
	}

	// Equal priority → lowest ID.
	req2, _ := json.Marshal(SchedulerPickRequest{
		Options: SchedulerPickOptions{Metadata: map[string]any{"group": "team"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-z-team", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}},
			{ID: "codex-a-team", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}},
		},
	})
	raw2, _ := app.HandleMethod(MethodSchedulerPick, req2)
	var resp2 SchedulerPickResponse
	if err := unmarshalOK(raw2, &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2.AuthID != "codex-a-team" {
		t.Fatalf("expected lowest ID tiebreak, got %q", resp2.AuthID)
	}
}

func TestSchedulerPickSmoothWeightedRoundRobin(t *testing.T) {
	app, _ := configureTestApp(t)
	request := SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "team"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-heavy", Provider: "codex", Weight: 5, Attributes: map[string]string{"plan_type": "team"}},
			{ID: "codex-light", Provider: "codex", Weight: 1, Attributes: map[string]string{"plan_type": "team"}},
		},
	}

	counts := map[string]int{}
	for index := 0; index < 12; index++ {
		resp := schedulerPickForTest(t, app, request)
		counts[resp.AuthID]++
	}
	if counts["codex-heavy"] != 10 || counts["codex-light"] != 2 {
		t.Fatalf("期望 12 次请求按 5:1 分配，实际为 %v", counts)
	}
}

func TestSchedulerPickReadsWeightFromAttributesAndMetadata(t *testing.T) {
	app, _ := configureTestApp(t)
	request := SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "team"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "from-attributes", Provider: "codex", Attributes: map[string]string{"plan_type": "team", "weight": "3"}},
			{ID: "from-metadata", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}, Metadata: map[string]any{"Weight": float64(1)}},
		},
	}

	counts := map[string]int{}
	for index := 0; index < 8; index++ {
		resp := schedulerPickForTest(t, app, request)
		counts[resp.AuthID]++
	}
	if counts["from-attributes"] != 6 || counts["from-metadata"] != 2 {
		t.Fatalf("期望从候选字段读取 3:1 权重，实际为 %v", counts)
	}
}

func TestSchedulerPickWeightZeroIsExcluded(t *testing.T) {
	app, _ := configureTestApp(t)
	request := SchedulerPickRequest{
		Provider: "codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "team"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "paused", Provider: "codex", Priority: 20, Weight: 0, Attributes: map[string]string{"plan_type": "team"}},
			{ID: "active", Provider: "codex", Priority: 10, Weight: 1, Attributes: map[string]string{"plan_type": "team"}},
		},
	}

	resp := schedulerPickForTest(t, app, request)
	if resp.AuthID != "active" {
		t.Fatalf("零权重凭证不应阻塞低优先级的可用凭证，实际选择 %q", resp.AuthID)
	}
}

func TestSchedulerPickUsesOnlyHighestPositivePriority(t *testing.T) {
	app, _ := configureTestApp(t)
	request := SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "team"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "primary-a", Provider: "codex", Priority: 10, Weight: 2, Attributes: map[string]string{"plan_type": "team"}},
			{ID: "primary-b", Provider: "codex", Priority: 10, Weight: 1, Attributes: map[string]string{"plan_type": "team"}},
			{ID: "fallback", Provider: "codex", Priority: 1, Weight: 100, Attributes: map[string]string{"plan_type": "team"}},
		},
	}

	counts := map[string]int{}
	for index := 0; index < 9; index++ {
		resp := schedulerPickForTest(t, app, request)
		counts[resp.AuthID]++
	}
	if counts["primary-a"] != 6 || counts["primary-b"] != 3 || counts["fallback"] != 0 {
		t.Fatalf("期望只在最高优先级层按 2:1 分配，实际为 %v", counts)
	}
}

func TestSchedulerPickWeightedStateIsConcurrent(t *testing.T) {
	app, _ := configureTestApp(t)
	request := SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "team"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "a", Provider: "codex", Weight: 2, Attributes: map[string]string{"plan_type": "team"}},
			{ID: "b", Provider: "codex", Weight: 1, Attributes: map[string]string{"plan_type": "team"}},
		},
	}

	const total = 300
	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int{}
	var countsMu sync.Mutex
	var workers sync.WaitGroup
	errors := make(chan error, total)
	for index := 0; index < total; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			rawResponse, callErr := app.HandleMethod(MethodSchedulerPick, rawRequest)
			if callErr != nil {
				errors <- callErr
				return
			}
			var resp SchedulerPickResponse
			if callErr = unmarshalOK(rawResponse, &resp); callErr != nil {
				errors <- callErr
				return
			}
			if !resp.Handled || resp.AuthID == "" {
				errors <- fmt.Errorf("unexpected scheduler response: %+v", resp)
				return
			}
			countsMu.Lock()
			counts[resp.AuthID]++
			countsMu.Unlock()
		}()
	}
	workers.Wait()
	close(errors)
	for callErr := range errors {
		t.Fatal(callErr)
	}
	if counts["a"] != 200 || counts["b"] != 100 {
		t.Fatalf("期望并发请求全局按 2:1 分配，实际为 %v", counts)
	}
}

func TestSchedulerPickAllWeightsPausedReturnsError(t *testing.T) {
	app, _ := configureTestApp(t)
	request := SchedulerPickRequest{
		Provider: "codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "team"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "paused-a", Provider: "codex", Weight: 0, Attributes: map[string]string{"plan_type": "team"}},
			{ID: "paused-b", Provider: "codex", Weight: -1, Attributes: map[string]string{"plan_type": "team"}},
		},
	}
	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rawResponse, err := app.HandleMethod(MethodSchedulerPick, rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var envelope Envelope
	if err := json.Unmarshal(rawResponse, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "auth_not_found" {
		t.Fatalf("所有凭证暂停时应返回 auth_not_found，实际为 %+v", envelope)
	}
}

// Isolation guarantee: when a tier group has no matching candidate, we must NOT
// fall back to a different tier. The plugin must return a structured scheduler
// error because an empty AuthID is invalid and would make the host fall back.
func TestSchedulerPickNoTierMatchRefusesFallback(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(SchedulerPickRequest{
		Options: SchedulerPickOptions{Metadata: map[string]any{"group": "team"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-a-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
			{ID: "codex-b-plus", Provider: "codex", Attributes: map[string]string{"plan_type": "plus"}},
		},
	})
	raw, _ := app.HandleMethod(MethodSchedulerPick, req)
	var envelope Envelope
	if err := json.Unmarshal(raw, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "auth_not_found" || envelope.Error.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("expected auth_not_found scheduler error, got %+v", envelope)
	}
}

// "supported"/"unknown" group matches only untiered candidates: a key pinned to
// a real tier never lands on an untiered file, and an untiered key never stings
// onto a tiered file.
func TestSchedulerPickSupportedMatchesUntieredOnly(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(SchedulerPickRequest{
		Options: SchedulerPickOptions{Metadata: map[string]any{"group": "supported"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-no-claim", Provider: "codex", Attributes: map[string]string{}},
			{ID: "codex-team", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}},
		},
	})
	raw, _ := app.HandleMethod(MethodSchedulerPick, req)
	var resp SchedulerPickResponse
	if err := unmarshalOK(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || resp.AuthID != "codex-no-claim" {
		t.Fatalf("expected untiered pick, got %+v", resp)
	}
}

// Custom classify groups are matched with the classify: prefix so they never
// collide with built-in plan_type values like "free".
func TestSchedulerPickMatchesClassifyPrefix(t *testing.T) {
	app := NewApp()
	yaml := []byte(`
enabled: true
state_file: "` + filepath.ToSlash(filepath.Join(t.TempDir(), "state.json")) + `"
classify_rules:
  - name: vip-files
    field: filename
    pattern: "vip"
    group: vip
    enabled: true
keys: []
`)
	reqCfg, _ := json.Marshal(LifecycleRequest{ConfigYAML: yaml})
	if _, err := app.HandleMethod(MethodPluginReconfigure, reqCfg); err != nil {
		t.Fatal(err)
	}
	req, _ := json.Marshal(SchedulerPickRequest{
		Provider: "codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "classify:vip"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "free-user.json", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
			{ID: "vip-user.json", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
		},
	})
	raw, err := app.HandleMethod(MethodSchedulerPick, req)
	if err != nil {
		t.Fatal(err)
	}
	var resp SchedulerPickResponse
	if err := unmarshalOK(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || resp.AuthID != "vip-user.json" {
		t.Fatalf("expected vip-user via classify:vip, got %+v", resp)
	}

	// Bare "vip" (no prefix) must NOT match — isolation from unprefixed names.
	req2, _ := json.Marshal(SchedulerPickRequest{
		Options: SchedulerPickOptions{Metadata: map[string]any{"group": "vip"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "vip-user.json", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
		},
	})
	raw2, _ := app.HandleMethod(MethodSchedulerPick, req2)
	var envelope Envelope
	if err := json.Unmarshal(raw2, &envelope); err != nil {
		t.Fatal(err)
	}
	if envelope.OK || envelope.Error == nil || envelope.Error.Code != "auth_not_found" {
		t.Fatalf("bare vip must return auth_not_found, got %+v", envelope)
	}
}

func TestCandidateClassifyCacheTracksAttributeChanges(t *testing.T) {
	app := NewApp()
	free := app.candidateGroups(SchedulerAuthCandidate{ID: "same.json", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}})
	team := app.candidateGroups(SchedulerAuthCandidate{ID: "same.json", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}})
	if len(free) != 1 || free[0] != "free" || len(team) != 1 || team[0] != "team" {
		t.Fatalf("cached groups did not track attributes: free=%v team=%v", free, team)
	}
}

func TestCandidateClassifyCacheIsBounded(t *testing.T) {
	app := NewApp()
	for index := 0; index < classifyCacheCapacity+25; index++ {
		app.candidateGroups(SchedulerAuthCandidate{ID: fmt.Sprintf("auth-%d", index), Provider: "codex"})
	}
	app.classifyMu.RLock()
	size := len(app.classifyCache)
	app.classifyMu.RUnlock()
	if size > classifyCacheCapacity {
		t.Fatalf("classify cache size = %d, capacity = %d", size, classifyCacheCapacity)
	}
}

// antigravity uses a "tier" attribute rather than plan_type; same filter logic.
func TestSchedulerPickMatchesAntigravityTier(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(SchedulerPickRequest{
		Provider: "antigravity",
		Options:  SchedulerPickOptions{Metadata: map[string]any{"group": "free-tier"}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "ag-paid", Provider: "antigravity", Attributes: map[string]string{"tier": "paid-tier"}},
			{ID: "ag-free", Provider: "antigravity", Attributes: map[string]string{"tier": "free-tier"}},
		},
	})
	raw, _ := app.HandleMethod(MethodSchedulerPick, req)
	var resp SchedulerPickResponse
	if err := unmarshalOK(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || resp.AuthID != "ag-free" {
		t.Fatalf("expected antigravity free-tier pick, got %+v", resp)
	}
}

func unmarshalOK(raw []byte, v any) error {
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return err
	}
	return json.Unmarshal(env.Result, v)
}

func schedulerPickForTest(t *testing.T, app *App, request SchedulerPickRequest) SchedulerPickResponse {
	t.Helper()
	rawRequest, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	rawResponse, err := app.HandleMethod(MethodSchedulerPick, rawRequest)
	if err != nil {
		t.Fatal(err)
	}
	var response SchedulerPickResponse
	if err := unmarshalOK(rawResponse, &response); err != nil {
		t.Fatal(err)
	}
	if !response.Handled || response.AuthID == "" {
		t.Fatalf("期望调度器接管请求并返回凭证，实际为 %+v", response)
	}
	return response
}
