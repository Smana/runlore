// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"errors"
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

// TestVerifyUsesVerifyModel routes the adversarial pass to the (cheaper) verify tier's
// model when one is set, leaving the main investigation model for the loop itself. The
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
		Model:      mainM,
		Verifier:   VerifyOn(verifyM, nil),
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Verify:     true,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if mainM.i != 1 {
		t.Fatalf("main model should serve only the loop (1 call), got %d", mainM.i)
	}
	if verifyM.i != 1 {
		t.Fatalf("verify pass should route to the verify tier's model (1 call), got %d", verifyM.i)
	}
	if got == nil || len(got.RootCauses) != 1 || got.RootCauses[0].Confidence != 0.7 {
		t.Fatalf("expected kept cause at verify confidence 0.7, got %+v", got)
	}
}

// TestVerifyFindingsReportsUnverifiedOnModelError pins the "could not run" signal
// verifyFindings owes its callers: a model error must report verified=false, distinct
// from a completed review, so a caller for which verify is the SOLE adversarial check
// (tryRecall, loop.go) can tell "approved" and "never ran" apart and refuse to treat
// the latter as the former. This is the signal the recall-verify fail-open regression
// (poisoned-recall-verify eval scenario) hinged on missing.
func TestVerifyFindingsReportsUnverifiedOnModelError(t *testing.T) {
	model := &errModel{err: errors.New("401 authentication_error: invalid x-api-key")}
	li := &LoopInvestigator{Model: model, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	inv := providers.Investigation{
		Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{Summary: "stale configmap", Confidence: 0.9}},
	}
	got, verified := li.verifyFindings(context.Background(), Request{Title: "x"}, inv, nil, nil)
	if verified {
		t.Fatal("a model error must report verified=false")
	}
	if len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "stale configmap" || got.Confidence != 0.9 {
		t.Fatalf("the investigation must still be returned UNCHANGED on a verify error (callers that ignore verified rely on this), got %+v", got)
	}
}

// TestVerifyFindingsReportsUnverifiedOnNoUsableVerdicts covers the other "review did
// not actually happen" case: the model responded without error but called no tool (or
// an unparseable one), so there is no verdict to apply. Practically identical to a
// model error from a caller's point of view — no adversarial review occurred — so it
// must also report verified=false, not silently pass as approval.
func TestVerifyFindingsReportsUnverifiedOnNoUsableVerdicts(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{{Text: "looks fine to me"}}} // no tool call
	li := &LoopInvestigator{Model: model, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	inv := providers.Investigation{
		Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{Summary: "stale configmap", Confidence: 0.9}},
	}
	got, verified := li.verifyFindings(context.Background(), Request{Title: "x"}, inv, nil, nil)
	if verified {
		t.Fatal("a response with no usable verdicts must report verified=false")
	}
	if len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "stale configmap" {
		t.Fatalf("the investigation must still be returned unchanged, got %+v", got)
	}
}

// TestVerifyFindingsReportsVerifiedOnSuccess is the positive pin: a completed review
// (verdicts applied) reports verified=true.
func TestVerifyFindingsReportsVerifiedOnSuccess(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"keep","confidence":0.8}]}`}}},
	}}
	li := &LoopInvestigator{Model: model, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	inv := providers.Investigation{
		Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{Summary: "oom", Confidence: 0.9}},
	}
	_, verified := li.verifyFindings(context.Background(), Request{Title: "x"}, inv, nil, nil)
	if !verified {
		t.Fatal("a completed review must report verified=true")
	}
}

// TestVerifyFindingsReportsVerifiedWhenNothingToReview covers the trivial early
// return (no root causes to review): there is nothing for an adversarial pass to
// reject, so this is not a failure — it must report verified=true (a caller like
// tryRecall must not force a fall-through here; there is no root cause to lose).
func TestVerifyFindingsReportsVerifiedWhenNothingToReview(t *testing.T) {
	li := &LoopInvestigator{Model: &scriptModel{}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	_, verified := li.verifyFindings(context.Background(), Request{Title: "x"}, providers.Investigation{}, nil, nil)
	if !verified {
		t.Fatal("no root causes to review must report verified=true, not a failure")
	}
}

// TestVerifyOutOfRangeVerdictsCountAsUnreviewed: a non-empty verdict list is not
// the same as a review having happened. applyVerdicts KEEPS any cause the reviewer
// did not mention, so verdicts whose indices all fall outside the root-cause range
// used to review nothing while still returning verified=true.
//
// On the recall path that is the serious one: catalog text short-circuits a live
// incident and the delivered investigation is stamped Verified: true. This is #395's
// fail-closed gate one layer down — the gate fired, but on a review that never
// touched a finding.
//
// Reachable exactly where this repo already documents flaky forced tool_choice
// (local OpenAI-compatible servers), i.e. when fail-closed matters most.
func TestVerifyOutOfRangeVerdictsCountAsUnreviewed(t *testing.T) {
	li := &LoopInvestigator{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	inv := providers.Investigation{
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{{Summary: "oom", Confidence: 0.8}},
	}

	// Index 7 against a one-cause investigation: reviews nothing.
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitVerdictsName, Args: `{"verdicts":[{"index":7,"verdict":"reject"}]}`}}},
	}}
	li.Model = model
	_, verified := li.verifyFindings(context.Background(), Request{Title: "x"}, inv, nil, nil)
	if verified {
		t.Fatal("verdicts that reference no actual root cause must NOT count as a review — " +
			"otherwise recall short-circuits an incident on findings nothing reviewed, stamped Verified: true")
	}

	// Control: an in-range verdict IS a review. Without this, the assertion above
	// would also pass if verifyFindings simply never returned true.
	model2 := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"keep"}]}`}}},
	}}
	li.Model = model2
	if _, ok := li.verifyFindings(context.Background(), Request{Title: "x"}, inv, nil, nil); !ok {
		t.Fatal("an in-range verdict must count as a genuine review")
	}
}

// TestApplyVerdictsKeepsOnUnrecognizedVerdict: the verdict switch had no default,
// so a case variant ("KEEP") or a synonym ("approve") matched no branch and the
// root cause was dropped — silently, with nothing logged. On the full-investigation
// path that discards a real, evidenced finding and forces the verdict inconclusive.
// An unparseable reviewer response is a reviewer malfunction, not a judgement about
// the finding.
func TestApplyVerdictsKeepsOnUnrecognizedVerdict(t *testing.T) {
	li := &LoopInvestigator{Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	for _, v := range []string{"KEEP", " keep ", "Downgrade", "approve", "definitely-not-a-verdict"} {
		t.Run(v, func(t *testing.T) {
			inv := providers.Investigation{
				Confidence: 0.8,
				RootCauses: []providers.Hypothesis{{Summary: "oom", Confidence: 0.8, Evidence: []string{"OOMKilled"}}},
			}
			out := applyVerdicts(li, Request{}, inv, []verdict{{Index: 0, Verdict: v}})
			if len(out.RootCauses) != 1 {
				t.Fatalf("verdict %q dropped the root cause (kept %d) — an unparseable verdict must never delete evidence",
					v, len(out.RootCauses))
			}
		})
	}
}
