package investigate

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestVerifyRejectsCorrelationFinding(t *testing.T) {
	// Mirrors the real PR #38 failure: a high-confidence root cause backed only by
	// "started after change X" with the diff unread. The reviewer rejects it.
	model := &scriptModel{responses: []providers.CompletionResponse{
		// step 0: the investigator submits a correlation-only finding
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"failing due to a recent change to crds-actions-runner-controller","confidence":0.8,"evidence":["started after the change","exact diff unknown"]}]}`}}},
		// verify pass: reject it
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"reject","confidence":0.1,"reason":"correlation only; diff never read; unrelated component"}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verify:     true,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "HarborInstallFailed", Workload: providers.Workload{Namespace: "tooling"}}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("OnComplete not called")
	}
	if len(got.RootCauses) != 0 {
		t.Fatalf("rejected root cause should be removed, got %+v", got.RootCauses)
	}
	if got.Verified {
		t.Fatal("a finding with no surviving cause must not be marked Verified")
	}
	if got.Confidence != 0 {
		t.Fatalf("overall confidence should drop to 0 with no surviving root cause, got %v", got.Confidence)
	}
	found := false
	for _, u := range got.Unresolved {
		if strings.Contains(u, "Rejected hypothesis") && strings.Contains(u, "crds-actions-runner-controller") {
			found = true
		}
	}
	if !found {
		t.Fatalf("rejected hypothesis should be recorded in unresolved, got %v", got.Unresolved)
	}
	if model.i != 2 {
		t.Fatalf("expected 2 model calls (findings + verify), got %d", model.i)
	}
}

// TestApplyVerdictsClampsConfidence checks that an out-of-range verdict
// confidence from the verify pass is clamped to [0,1] before it is applied to a
// root cause's score — on both the keep and downgrade branches — and that the
// recomputed overall confidence stays in range too. The hypothesis enters at the
// ceiling (1.0) so the never-raise floor (min with the entering score) does not
// mask the clamp: min(1.0, clamp01(1.7)) == 1. NaN is not reachable here (the
// `v.Confidence > 0` guard skips it: NaN > 0 is false); NaN clamping is covered
// at the model-JSON boundary in tools_test.
func TestApplyVerdictsClampsConfidence(t *testing.T) {
	li := &LoopInvestigator{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cases := []struct {
		name    string
		verdict string
		conf    float64
		want    float64
	}{
		{"keep above one", "keep", 1.7, 1},
		{"downgrade above one", "downgrade", 1.4, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := providers.Investigation{Confidence: 1, RootCauses: []providers.Hypothesis{{Summary: "x", Confidence: 1}}}
			out := applyVerdicts(li, Request{}, inv, []verdict{{Index: 0, Verdict: tc.verdict, Confidence: tc.conf}})
			if len(out.RootCauses) != 1 || out.RootCauses[0].Confidence != tc.want {
				t.Fatalf("root-cause confidence = %v, want %v", out.RootCauses[0].Confidence, tc.want)
			}
			if out.Confidence != tc.want {
				t.Fatalf("overall confidence = %v, want %v", out.Confidence, tc.want)
			}
		})
	}
}

// TestVerifyNeverRaisesConfidence pins the design invariant (docs/design.md:203):
// the adversarial verify pass may only keep confidence equal or lower it, never
// raise — both per-hypothesis and for the overall investigation confidence. A
// `keep` verdict carrying a HIGHER confidence than the hypothesis entered with
// must not promote it; a `keep` with a lower confidence still lowers it.
func TestVerifyNeverRaisesConfidence(t *testing.T) {
	li := &LoopInvestigator{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	cases := []struct {
		name    string
		enter   float64
		verdict string
		conf    float64
		want    float64
	}{
		{"keep does not raise", 0.5, "keep", 0.9, 0.5},
		{"keep lowers", 0.5, "keep", 0.3, 0.3},
		{"downgrade does not raise", 0.5, "downgrade", 0.9, 0.5},
		{"downgrade lowers", 0.5, "downgrade", 0.3, 0.3},
		{"keep with zero conf leaves original", 0.5, "keep", 0, 0.5},
		{"downgrade with zero conf halves", 0.5, "downgrade", 0, 0.25},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := providers.Investigation{
				Confidence: tc.enter,
				RootCauses: []providers.Hypothesis{{Summary: "x", Confidence: tc.enter}},
			}
			out := applyVerdicts(li, Request{}, inv, []verdict{{Index: 0, Verdict: tc.verdict, Confidence: tc.conf}})
			if len(out.RootCauses) != 1 || out.RootCauses[0].Confidence != tc.want {
				t.Fatalf("root-cause confidence = %v, want %v", out.RootCauses[0].Confidence, tc.want)
			}
			// Overall must never exceed the pre-verify overall.
			if out.Confidence > tc.enter {
				t.Fatalf("overall confidence %v raised above pre-verify %v", out.Confidence, tc.enter)
			}
			if out.Confidence != tc.want {
				t.Fatalf("overall confidence = %v, want %v", out.Confidence, tc.want)
			}
		})
	}
}

func TestVerifyDowngradesUnproven(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.9,"root_causes":[{"summary":"db migration stalled","confidence":0.9,"evidence":["migration lock held"]}]}`}}},
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"downgrade","confidence":0.4,"reason":"plausible but not confirmed"}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{Model: model, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Verify: true,
		OnComplete: func(inv providers.Investigation) { got = &inv }}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil || len(got.RootCauses) != 1 || got.RootCauses[0].Confidence != 0.4 || got.Confidence != 0.4 {
		t.Fatalf("expected downgraded confidence 0.4, got %+v", got)
	}
	if !got.Verified {
		t.Fatal("a finding with a surviving reviewed cause must be marked Verified")
	}
}

// TestVerifyUsesVerifyModel routes the adversarial pass to the (cheaper) VerifyModel
// when one is set, leaving the main investigation model for the loop itself. The
// scriptModel stubs panic if called more than scripted, so wrong routing fails loudly.
func TestVerifyUsesVerifyModel(t *testing.T) {
	mainM := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"oom","confidence":0.8,"evidence":["OOMKilled in events"]}]}`}}},
	}}
	verifyM := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"keep","confidence":0.7}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:       mainM,
		VerifyModel: verifyM,
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verify:      true,
		OnComplete:  func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if mainM.i != 1 {
		t.Fatalf("main model should serve only the loop (1 call), got %d", mainM.i)
	}
	if verifyM.i != 1 {
		t.Fatalf("verify pass should route to VerifyModel (1 call), got %d", verifyM.i)
	}
	if got == nil || len(got.RootCauses) != 1 || got.RootCauses[0].Confidence != 0.7 {
		t.Fatalf("expected kept cause at verify confidence 0.7, got %+v", got)
	}
}
