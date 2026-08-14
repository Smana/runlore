// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeConfirmTool is a confirmatory Tool that records the args it was called with.
type fakeConfirmTool struct {
	name    string
	out     string
	err     error
	gotArgs string
}

func (f *fakeConfirmTool) Name() string        { return f.name }
func (f *fakeConfirmTool) Description() string { return "" }
func (f *fakeConfirmTool) Schema() string      { return "{}" }
func (f *fakeConfirmTool) Call(_ context.Context, args string) (string, error) {
	f.gotArgs = args
	return f.out, f.err
}

func recalledInv() providers.Investigation {
	return providers.Investigation{
		Title:      "web down",
		Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{Summary: "image tag rollout", Confidence: 0.9,
			Evidence: []string{"instant recall: matched knowledge-base entry \"x\""}}},
		Resource: providers.Workload{Namespace: "apps", Name: "web"},
	}
}

func TestConfirmRecallAppendsCurrentState(t *testing.T) {
	ps := &fakeConfirmTool{name: "pod_status", out: "web CrashLoopBackOff"}
	li := &LoopInvestigator{Tools: []Tool{ps}}
	req := Request{Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	inv, _, gathered := li.confirmRecall(context.Background(), req, recalledInv())
	if !gathered {
		t.Fatal("expected gathered=true when a confirm tool returns output")
	}
	joined := strings.Join(inv.RootCauses[0].Evidence, "\n")
	if !strings.Contains(joined, "CrashLoopBackOff") || !strings.Contains(joined, "pod_status") {
		t.Fatalf("current-state evidence not appended: %q", joined)
	}
}

// confirmRecall gathers state namespace-wide, NOT scoped to the workload object: a
// recalled cause often lives on a sibling resource (a Crossplane AccessKey, a
// dependency), so an object filter would hide the cause and make verify downgrade a
// correct recall. kube_events must therefore carry the namespace and NO `object`.
func TestConfirmRecallIsNamespaceWideNotObjectScoped(t *testing.T) {
	ps := &fakeConfirmTool{name: "pod_status", out: "ok"}
	ev := &fakeConfirmTool{name: "kube_events", out: "Warning"}
	li := &LoopInvestigator{Tools: []Tool{ps, ev}}
	req := Request{Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	if _, _, gathered := li.confirmRecall(context.Background(), req, recalledInv()); !gathered {
		t.Fatal("expected gathered=true")
	}
	if !strings.Contains(ps.gotArgs, `"namespace":"apps"`) {
		t.Fatalf("pod_status not scoped to namespace: %q", ps.gotArgs)
	}
	if !strings.Contains(ev.gotArgs, `"namespace":"apps"`) {
		t.Fatalf("kube_events not scoped to namespace: %q", ev.gotArgs)
	}
	if strings.Contains(ev.gotArgs, `"object"`) {
		t.Fatalf("kube_events must NOT scope to the workload object (would hide cross-resource causes): %q", ev.gotArgs)
	}
}

func TestConfirmRecallNoNamespaceSkips(t *testing.T) {
	ps := &fakeConfirmTool{name: "pod_status", out: "x"}
	li := &LoopInvestigator{Tools: []Tool{ps}}
	req := Request{Workload: providers.Workload{}} // no namespace
	inv, _, gathered := li.confirmRecall(context.Background(), req, recalledInv())
	if gathered {
		t.Fatal("no namespace must skip confirmation")
	}
	if ps.gotArgs != "" {
		t.Fatalf("tool must not be called without a namespace, got args %q", ps.gotArgs)
	}
	if len(inv.RootCauses[0].Evidence) != 1 {
		t.Fatalf("evidence must be unchanged, got %v", inv.RootCauses[0].Evidence)
	}
}

func TestConfirmRecallToolsAbsentSkips(t *testing.T) {
	li := &LoopInvestigator{Tools: []Tool{&fakeConfirmTool{name: "what_changed", out: "x"}}}
	req := Request{Workload: providers.Workload{Namespace: "apps"}}
	if _, _, gathered := li.confirmRecall(context.Background(), req, recalledInv()); gathered {
		t.Fatal("no confirm tools present must yield gathered=false")
	}
}

func TestConfirmRecallToolErrorTolerated(t *testing.T) {
	bad := &fakeConfirmTool{name: "pod_status", err: context.DeadlineExceeded}
	good := &fakeConfirmTool{name: "kube_events", out: "Warning FailedMount"}
	li := &LoopInvestigator{Tools: []Tool{bad, good}}
	req := Request{Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	inv, _, gathered := li.confirmRecall(context.Background(), req, recalledInv())
	if !gathered {
		t.Fatal("one tool erroring must not prevent the other from confirming")
	}
	if !strings.Contains(strings.Join(inv.RootCauses[0].Evidence, "\n"), "FailedMount") {
		t.Fatal("the surviving tool's output should be appended")
	}
}

// TestConfirmRecallRedactsBeforeTheModelSeesIt pins the egress boundary on this path.
//
// confirmRecall calls its tools DIRECTLY rather than through runTool/dispatchTools, so
// it does not inherit the loop's `truncateOutput(redact.Secrets(...))` hook, and the
// only other scrubber (redactInvestigation) runs in deliver() — AFTER verify. Both the
// evidence bullet and the transcript reach the verify model via renderForReview, so
// without masking here a secret in a pod's terminated message or a FailedMount event
// ships to the provider verbatim. pod_status advertises exactly that content ("the
// message names the exact cause, e.g. a missing Secret/ConfigMap key").
func TestConfirmRecallRedactsBeforeTheModelSeesIt(t *testing.T) {
	const secret = "ghp_0123456789abcdefghijklmnopqrstuvwx"
	ps := &fakeConfirmTool{name: "pod_status", out: "web CrashLoopBackOff: bad token " + secret}
	li := &LoopInvestigator{Tools: []Tool{ps}}
	req := Request{Workload: providers.Workload{Namespace: "apps", Name: "web"}}

	inv, transcript, gathered := li.confirmRecall(context.Background(), req, recalledInv())
	if !gathered {
		t.Fatal("expected gathered=true")
	}
	// Both forms, and the assembled review that carries them to the provider.
	if joined := strings.Join(inv.RootCauses[0].Evidence, "\n"); strings.Contains(joined, secret) {
		t.Errorf("evidence bullet leaks the token: %q", joined)
	}
	if review := renderForReview(req, inv, transcript); strings.Contains(review, secret) {
		t.Errorf("the message sent to the verify model leaks the token:\n%s", review)
	}
	// The surrounding diagnostic text must survive — masking, not dropping.
	if !strings.Contains(strings.Join(inv.RootCauses[0].Evidence, "\n"), "CrashLoopBackOff") {
		t.Error("redaction must mask the secret, not discard the tool output")
	}
}

func TestCapRecallConfidenceOnlyLowers(t *testing.T) {
	inv := providers.Investigation{Confidence: 0.9, RootCauses: []providers.Hypothesis{{Confidence: 0.9}, {Confidence: 0.5}}}
	out := capRecallConfidence(inv, 0.70)
	if out.Confidence != 0.70 || out.RootCauses[0].Confidence != 0.70 {
		t.Fatalf("values above the ceiling must be lowered: %+v", out)
	}
	if out.RootCauses[1].Confidence != 0.5 {
		t.Fatalf("a value already below the ceiling must be untouched, got %v", out.RootCauses[1].Confidence)
	}
}

// TestConfirmRecallGroundsTheReview pins the fix for recalled findings being
// downgraded on principle rather than on their merits.
//
// The verify prompt's central rule is "each cited piece of evidence must trace to a
// tool result in the transcript excerpt below … if it cannot be found in the
// transcript, treat it as unverified — reject or downgrade it". The recall path
// passed a nil transcript, so renderForReview emitted NO excerpt section at all and
// that rule was unsatisfiable by construction — the reviewer was told to check
// against a document that did not exist, and correctly downgraded every recall.
// Measured live: the only recall that ever fired went 0.72 -> 0.45, and
// runlore_recall_hits_total{result="downgraded"} was 1 of 1.
//
// The confirm output was always there, but as an evidence bullet — an assertion by
// the author, not tool output the reviewer may treat as fact. That is exactly the
// distinction the groundedness rule draws, so it has to be handed over as both.
func TestConfirmRecallGroundsTheReview(t *testing.T) {
	ps := &fakeConfirmTool{name: "pod_status", out: "web-abc  Running ready=1/1"}
	ev := &fakeConfirmTool{name: "kube_events", out: "(no Warning events)"}
	li := &LoopInvestigator{Tools: []Tool{ps, ev}}
	req := Request{Title: "WebDown", Workload: providers.Workload{Namespace: "apps", Name: "web"}}

	inv, transcript, gathered := li.confirmRecall(context.Background(), req, recalledInv())
	if !gathered {
		t.Fatal("expected gathered=true")
	}

	// The transcript must be the shape transcriptExcerpt walks: an assistant turn
	// carrying the calls (so each result can be labelled with its tool) plus one tool
	// turn per result. Without the assistant turn the excerpt renders "[tool]" for
	// everything and the reviewer cannot tell pod_status from kube_events.
	if len(transcript) != 3 {
		t.Fatalf("want 1 assistant turn + 2 tool results, got %d messages", len(transcript))
	}
	if transcript[0].Role != "assistant" || len(transcript[0].ToolCalls) != 2 {
		t.Fatalf("first message must be the assistant turn carrying both calls, got %+v", transcript[0])
	}
	for _, m := range transcript[1:] {
		if m.Role != "tool" || m.ToolCallID == "" {
			t.Fatalf("result messages must be tool turns with a call id, got %+v", m)
		}
	}

	// The property that actually matters: the review the model sees now CONTAINS a
	// transcript excerpt, with each result attributed to the tool that produced it.
	review := renderForReview(req, inv, transcript)
	if !strings.Contains(review, "Tool transcript excerpt") {
		t.Fatalf("the review must carry a transcript excerpt, else the prompt's groundedness "+
			"rule is unsatisfiable and every recall is downgraded on principle:\n%s", review)
	}
	for _, want := range []string{"[pod_status] web-abc  Running ready=1/1", "[kube_events] (no Warning events)"} {
		if !strings.Contains(review, want) {
			t.Errorf("review must attribute each result to its tool; missing %q:\n%s", want, review)
		}
	}
}

// TestConfirmRecallNoTranscriptWhenNothingGathered keeps the degraded path honest: if
// no current state could be gathered there is nothing the reviewer may treat as
// verified, so it must get NO transcript rather than an empty or fabricated one. The
// caller de-rates that case via recallUnconfirmedCap.
func TestConfirmRecallNoTranscriptWhenNothingGathered(t *testing.T) {
	li := &LoopInvestigator{Tools: []Tool{&fakeConfirmTool{name: "pod_status", out: "  "}}}
	req := Request{Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	_, transcript, gathered := li.confirmRecall(context.Background(), req, recalledInv())
	if gathered {
		t.Fatal("blank tool output must not count as gathered")
	}
	if transcript != nil {
		t.Fatalf("no gathered state must yield a nil transcript, got %+v", transcript)
	}
	// And a nil transcript must still render a reviewable message (no excerpt section).
	if review := renderForReview(req, recalledInv(), nil); strings.Contains(review, "Tool transcript excerpt") {
		t.Fatalf("a nil transcript must not render an empty excerpt section:\n%s", review)
	}
}
