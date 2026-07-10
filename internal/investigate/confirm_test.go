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
	inv, gathered := li.confirmRecall(context.Background(), req, recalledInv())
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
	if _, gathered := li.confirmRecall(context.Background(), req, recalledInv()); !gathered {
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
	inv, gathered := li.confirmRecall(context.Background(), req, recalledInv())
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
	if _, gathered := li.confirmRecall(context.Background(), req, recalledInv()); gathered {
		t.Fatal("no confirm tools present must yield gathered=false")
	}
}

func TestConfirmRecallToolErrorTolerated(t *testing.T) {
	bad := &fakeConfirmTool{name: "pod_status", err: context.DeadlineExceeded}
	good := &fakeConfirmTool{name: "kube_events", out: "Warning FailedMount"}
	li := &LoopInvestigator{Tools: []Tool{bad, good}}
	req := Request{Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	inv, gathered := li.confirmRecall(context.Background(), req, recalledInv())
	if !gathered {
		t.Fatal("one tool erroring must not prevent the other from confirming")
	}
	if !strings.Contains(strings.Join(inv.RootCauses[0].Evidence, "\n"), "FailedMount") {
		t.Fatal("the surviving tool's output should be appended")
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
