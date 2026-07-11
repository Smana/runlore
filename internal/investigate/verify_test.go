// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestVerifyPromptCarriesToolTranscript pins C2: the adversarial pass must see a
// bounded, redacted excerpt of the tool transcript (so it can check that each root
// cause traces to an actual tool result) and its prompt must carry the
// groundedness instruction. The loop drives one tool call whose output becomes the
// transcript the verify request must include.
func TestVerifyPromptCarriesToolTranscript(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		// step 0: call a tool, producing a tool-role message in history.
		{ToolCalls: []providers.ToolCall{{ID: "c1", Name: "what_changed", Args: "{}"}}},
		// step 1: submit findings.
		{ToolCalls: []providers.ToolCall{{ID: "c2", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"oom","confidence":0.8,"evidence":["OOMKilled"]}]}`}}},
		// verify pass: keep.
		{ToolCalls: []providers.ToolCall{{ID: "c3", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"keep"}]}`}}},
	}}
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{echoTool{name: "what_changed"}}, // Call returns "ok"
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verify:     true,
		OnComplete: func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(model.reqs) != 3 {
		t.Fatalf("want 3 model calls (2 loop + verify), got %d", len(model.reqs))
	}
	verifyUser := model.reqs[2].Messages[0].Content
	// The tool's output ("ok") must appear in the verify prompt as a transcript excerpt.
	if !strings.Contains(verifyUser, "ok") || !strings.Contains(strings.ToLower(verifyUser), "transcript") {
		t.Fatalf("verify prompt missing tool-transcript excerpt, got %q", verifyUser)
	}
	// The groundedness instruction must be present in the system prompt.
	if !strings.Contains(strings.ToLower(model.reqs[2].System), "trace to a tool result") {
		t.Fatalf("verify system prompt missing groundedness instruction, got %q", model.reqs[2].System)
	}
}

// TestTranscriptExcerptSizeCappedAndRedacted asserts the excerpt is hard-capped to
// a byte budget (so feeding it to verify can't blow up tokens/cost) and that it is
// redacted (defense in depth — even though loop history is already redacted).
func TestTranscriptExcerptSizeCappedAndRedacted(t *testing.T) {
	// A large first tool result (to force the cap) plus a small latest result that
	// carries a secret-shaped token WITHIN budget — so redaction, not truncation, is
	// what removes it (the newest result is always kept in full when it fits).
	secret := "AWS_SECRET_ACCESS_KEY=AKIAIOSFODNN7EXAMPLEabcdef1234567890ABCD"
	big := strings.Repeat("A", maxVerifyTranscriptBytes*4)
	msgs := []providers.Message{
		{Role: "user", Content: "seed"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "1", Name: "kube_events", Args: "{}"}, {ID: "2", Name: "pod_logs", Args: "{}"}}},
		{Role: "tool", ToolCallID: "1", Content: big},                         // oldest, oversized
		{Role: "tool", ToolCallID: "2", Content: "recent log line " + secret}, // newest, small, has a secret
	}
	got := transcriptExcerpt(msgs)
	if len(got) > maxVerifyTranscriptBytes {
		t.Fatalf("excerpt not capped: %d bytes > budget %d", len(got), maxVerifyTranscriptBytes)
	}
	if !strings.Contains(got, "recent log line") {
		t.Fatalf("excerpt should keep the newest (most decision-relevant) tool result, got %q", got[:min(len(got), 200)])
	}
	if strings.Contains(got, "AKIAIOSFODNN7EXAMPLEabcdef1234567890ABCD") {
		t.Fatalf("excerpt leaked a secret-shaped value")
	}
}

// TestTranscriptExcerptEmptyWhenNoTools returns empty for a transcript with no tool
// results (e.g. the recall short-circuit path, where no loop ran).
func TestTranscriptExcerptEmptyWhenNoTools(t *testing.T) {
	if got := transcriptExcerpt(nil); got != "" {
		t.Fatalf("nil transcript should yield empty excerpt, got %q", got)
	}
	msgs := []providers.Message{{Role: "user", Content: "seed"}, {Role: "assistant", Content: "thinking"}}
	if got := transcriptExcerpt(msgs); got != "" {
		t.Fatalf("transcript with no tool results should yield empty excerpt, got %q", got)
	}
}

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
	for _, u := range got.RuledOut {
		if strings.Contains(u, "crds-actions-runner-controller") {
			found = true
		}
	}
	if !found {
		t.Fatalf("rejected hypothesis should be recorded in ruled_out, got %v", got.RuledOut)
	}
	for _, u := range got.Unresolved {
		if strings.Contains(u, "Rejected hypothesis") {
			t.Fatalf("rejected hypothesis must no longer land in unresolved, got %v", got.Unresolved)
		}
	}
	if model.i != 2 {
		t.Fatalf("expected 2 model calls (findings + verify), got %d", model.i)
	}
}

// TestApplyVerdictsRejectedGoesToRuledOut pins the honesty contract: a rejected
// hypothesis is a fact about what was disproven, not an open question for a human,
// so it lands in RuledOut (formatted "<summary> — <reason>") rather than
// Unresolved. And when the adversarial pass refutes every hypothesis, an
// actionable verdict has no surviving support, so it downgrades to inconclusive.
func TestApplyVerdictsRejectedGoesToRuledOut(t *testing.T) {
	li := &LoopInvestigator{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	inv := providers.Investigation{
		Confidence: 0.8,
		Verdict:    providers.VerdictNoAction,
		RootCauses: []providers.Hypothesis{{Summary: "crds-actions-runner-controller change", Confidence: 0.8}},
	}
	out := applyVerdicts(li, Request{}, inv, []verdict{{Index: 0, Verdict: "reject", Confidence: 0.1, Reason: "correlation only; diff never read"}})

	if len(out.RuledOut) != 1 {
		t.Fatalf("rejected hypothesis should be recorded in RuledOut, got %v", out.RuledOut)
	}
	if !strings.Contains(out.RuledOut[0], "crds-actions-runner-controller") {
		t.Fatalf("RuledOut entry should name the hypothesis summary, got %q", out.RuledOut[0])
	}
	if !strings.Contains(out.RuledOut[0], "correlation only") {
		t.Fatalf("RuledOut entry should carry the rejection reason, got %q", out.RuledOut[0])
	}
	for _, u := range out.Unresolved {
		if strings.Contains(u, "Rejected hypothesis") {
			t.Fatalf("rejected hypothesis must no longer land in Unresolved, got %v", out.Unresolved)
		}
	}
	if out.Verdict != providers.VerdictInconclusive {
		t.Fatalf("rejecting every hypothesis should downgrade verdict to inconclusive, got %q", out.Verdict)
	}
}

// TestVerifyForcesSubmitVerdicts asserts the adversarial pass forces the model to
// call submit_verdicts (ToolChoice) — a reviewer that rambles in prose instead of
// recording verdicts silently skips the honesty check, so prose is never allowed.
func TestVerifyForcesSubmitVerdicts(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"oom","confidence":0.8,"evidence":["OOMKilled"]}]}`}}},
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"keep"}]}`}}},
	}}
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verify:     true,
		OnComplete: func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(model.reqs) != 2 {
		t.Fatalf("expected 2 model calls (loop + verify), got %d", len(model.reqs))
	}
	if model.reqs[0].ToolChoice != "" {
		t.Fatalf("the investigation step must not force a tool, got ToolChoice=%q", model.reqs[0].ToolChoice)
	}
	if model.reqs[1].ToolChoice != submitVerdictsName {
		t.Fatalf("verify pass must force %q, got ToolChoice=%q", submitVerdictsName, model.reqs[1].ToolChoice)
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
