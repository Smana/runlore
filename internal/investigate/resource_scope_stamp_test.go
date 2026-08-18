// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// stubScoper answers from a fixed table, so the loop's use of discovery is testable
// without an API server. err, when set, models discovery being unreachable.
type stubScoper struct {
	scopes map[string]providers.ResourceScope
	err    error
	calls  int
}

func (s *stubScoper) KindScope(_ context.Context, kind string) (providers.ResourceScope, error) {
	s.calls++
	if s.err != nil {
		return providers.ScopeUnknown, s.err
	}
	return s.scopes[kind], nil
}

// scopeLoop runs one investigation whose submit_findings names args, and returns the
// resource the loop delivered.
func scopeLoop(t *testing.T, ks providers.KindScoper, findings string, origin providers.Workload) providers.Workload {
	t.Helper()
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: findings}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		KindScope:  ks,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	req := Request{
		Title:       "RDSInstanceHighCPU",
		Fingerprint: "fp-scope",
		Workload:    origin,
		Labels:      map[string]string{"alertname": "RDSInstanceHighCPU"},
	}
	if err := li.Investigate(context.Background(), req); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("OnComplete not called")
	}
	return got.Resource
}

// TestLoopStampsTheDiscoveredScopeOnTheResource closes the wire this change exists to
// open: discovery already knows whether a kind is namespaced, and until the loop
// stamps that on the delivered Workload, the renderer has nothing to read and falls
// back to guessing from the kind's NAME.
//
// It is the same class of gap TestLoopStampsAlertResourceFromRequest closes for
// AlertResource — both halves can be green with the wire between them cut.
func TestLoopStampsTheDiscoveredScopeOnTheResource(t *testing.T) {
	ks := &stubScoper{scopes: map[string]providers.ResourceScope{
		"DBInstance": providers.ScopeNamespaced, // an ACK CRD, not an RDS instance
		"Node":       providers.ScopeClusterScoped,
	}}
	origin := providers.Workload{Namespace: "observability"}

	got := scopeLoop(t, ks, `{"confidence":0.9,"affected_resource":{"kind":"DBInstance","namespace":"ack-system","name":"datagrok"},"root_causes":[{"summary":"x","confidence":0.9}]}`, origin)
	if got.Scope != providers.ScopeNamespaced {
		t.Errorf("Resource.Scope = %v, want ScopeNamespaced (discovery said the CRD is namespaced)", got.Scope)
	}
	if got.Ref() != "ack-system/datagrok" {
		t.Errorf("scoping must not disturb the workload's identity; got %q", got.Ref())
	}

	got = scopeLoop(t, ks, `{"confidence":0.9,"affected_resource":{"kind":"Node","name":"ip-10-11-132-8"},"root_causes":[{"summary":"x","confidence":0.9}]}`, origin)
	if got.Scope != providers.ScopeClusterScoped {
		t.Errorf("Resource.Scope = %v, want ScopeClusterScoped", got.Scope)
	}
}

// TestLoopScopesTheAlertResourceItFellBackTo covers the other half of
// preferDiscoveredResource: when the model names no resource the alert's workload IS
// the delivered resource, and it needs the same answer. Scoping only the model's
// resource would leave every alert-only card guessing.
func TestLoopScopesTheAlertResourceItFellBackTo(t *testing.T) {
	ks := &stubScoper{scopes: map[string]providers.ResourceScope{"Node": providers.ScopeClusterScoped}}
	origin := providers.Workload{Kind: "Node", Namespace: "observability", Name: "ip-10-11-132-8"}

	got := scopeLoop(t, ks, `{"confidence":0.9,"root_causes":[{"summary":"x","confidence":0.9}]}`, origin)
	if got.Scope != providers.ScopeClusterScoped {
		t.Errorf("Resource.Scope = %v, want ScopeClusterScoped on the fallen-back-to alert workload", got.Scope)
	}
}

// TestLoopLeavesTheScopeUnknownWhenDiscoveryCannotAnswer is the conservative half, and
// the one that must not regress: unknown is NOT cluster-scoped, and every path that
// cannot reach a real answer has to say so rather than assert one.
//
// A nil scoper is the ordinary case, not an edge one — no cluster access, an eval run,
// the demo. An erroring scoper is a live API server that is having a bad day. Neither
// may fail the investigation, and neither may put a fact on the workload.
func TestLoopLeavesTheScopeUnknownWhenDiscoveryCannotAnswer(t *testing.T) {
	findings := `{"confidence":0.9,"affected_resource":{"kind":"DBInstance","namespace":"observability","name":"datagrok"},"root_causes":[{"summary":"x","confidence":0.9}]}`
	origin := providers.Workload{Namespace: "observability"}

	for _, tc := range []struct {
		name string
		ks   providers.KindScoper
	}{
		{"no scoper wired at all", nil},
		{"discovery unreachable", &stubScoper{err: errors.New("connection refused")}},
		{"this cluster serves no such kind", &stubScoper{scopes: map[string]providers.ResourceScope{}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := scopeLoop(t, tc.ks, findings, origin)
			if got.Scope != providers.ScopeUnknown {
				t.Errorf("Resource.Scope = %v, want ScopeUnknown — an unanswerable question must not "+
					"become an answer", got.Scope)
			}
			if got.Ref() != "observability/datagrok" {
				t.Errorf("the workload's identity must survive a failed scope lookup; got %q", got.Ref())
			}
		})
	}
}

// TestLoopAsksDiscoveryNothingForANamelessResource keeps the lookup off the hot path
// when there is nothing to ask about: a finding that names no kind has no kind to
// resolve, and a discovery round trip for "" would be pure cost.
func TestLoopAsksDiscoveryNothingForANamelessResource(t *testing.T) {
	ks := &stubScoper{scopes: map[string]providers.ResourceScope{}}
	scopeLoop(t, ks, `{"confidence":0.9,"root_causes":[{"summary":"x","confidence":0.9}]}`,
		providers.Workload{Namespace: "observability"})
	if ks.calls != 0 {
		t.Errorf("KindScope called %d times for a resource with no kind, want 0", ks.calls)
	}
}
