package plugin

import (
	"encoding/json"
	"testing"
)

func TestSchedulerPickNoGroupDefers(t *testing.T) {
	app, _ := configureTestApp(t)
	req, _ := json.Marshal(SchedulerPickRequest{
		Provider: "codex",
		Model:    "gpt-5-codex",
		Options:  SchedulerPickOptions{Metadata: map[string]any{}},
		Candidates: []SchedulerAuthCandidate{
			{ID: "codex-a-free", Provider: "codex", Attributes: map[string]string{"plan_type": "free"}},
			{ID: "codex-b-team", Provider: "codex", Attributes: map[string]string{"plan_type": "team"}},
		},
	})
	raw, err := app.HandleMethod(MethodSchedulerPick, req)
	if err != nil {
		t.Fatal(err)
	}
	var env Envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatal(err)
	}
	var resp SchedulerPickResponse
	if err := json.Unmarshal(env.Result, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Handled {
		t.Fatalf("expected Handled=false when no group, got %+v", resp)
	}
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

// Isolation guarantee: when a tier group has no matching candidate, we must NOT
// fall back to a different tier. Returning Handled=true with empty AuthID
// signals "we decided — no usable auth" so the host surfaces the failure rather
// than leaking onto the wrong tier.
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
	var resp SchedulerPickResponse
	if err := unmarshalOK(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.Handled || resp.AuthID != "" {
		t.Fatalf("expected Handled=true empty AuthID (no tier leak), got %+v", resp)
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
