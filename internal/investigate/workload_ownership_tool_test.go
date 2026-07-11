// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeOwnerWalker returns a canned owner chain (and optional error) for the tool test.
type fakeOwnerWalker struct {
	oc  providers.OwnerChain
	err error
}

func (f fakeOwnerWalker) WorkloadOwnership(_ context.Context, _, _, _ string) (providers.OwnerChain, error) {
	return f.oc, f.err
}

// NB: fakeInspector (with field `status`) is shared from gitops_tools_test.go.

func chainToKustomization() providers.OwnerChain {
	return providers.OwnerChain{
		Chain: []providers.OwnerLink{
			{Kind: "Pod", Name: "harbor-core-59598dbd57-ltkzw", Namespace: "harbor"},
			{Kind: "ReplicaSet", Name: "harbor-core-59598dbd57", Namespace: "harbor"},
			{Kind: "Deployment", Name: "harbor-core", Namespace: "harbor"},
		},
		Top:                providers.OwnerLink{Kind: "Deployment", Name: "harbor-core", Namespace: "harbor"},
		Engine:             providers.EngineFlux,
		ManagedByKind:      "Kustomization",
		ManagedByNamespace: "flux-system",
		ManagedByName:      "harbor",
	}
}

// TestWorkloadOwnershipToolRendersChain: the owner chain renders engine-agnostically
// down to the owning Kustomization, and a Ready/in-sync owner reports no drift.
func TestWorkloadOwnershipToolRendersChain(t *testing.T) {
	tool := WorkloadOwnershipTool{
		Kube:      fakeOwnerWalker{oc: chainToKustomization()},
		Inspector: fakeInspector{status: providers.ResourceStatus{Ready: "True", Refs: map[string]string{}}},
	}
	out, err := tool.Call(context.Background(), `{"namespace":"harbor","selector":"app=harbor-core"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "Deployment/harbor-core") || !strings.Contains(out, "managed by Kustomization flux-system/harbor") {
		t.Fatalf("chain not rendered as expected:\n%s", out)
	}
	if !strings.Contains(out, "drift: none detected") {
		t.Fatalf("expected no-drift for a Ready owner, got:\n%s", out)
	}
}

// TestWorkloadOwnershipToolArgoOutOfSync: an ArgoCD Application reporting sync=OutOfSync
// surfaces as the authoritative engine drift verdict.
func TestWorkloadOwnershipToolArgoOutOfSync(t *testing.T) {
	oc := providers.OwnerChain{
		Chain:              []providers.OwnerLink{{Kind: "Pod", Name: "guestbook-abc", Namespace: "apps"}, {Kind: "StatefulSet", Name: "guestbook", Namespace: "apps"}},
		Top:                providers.OwnerLink{Kind: "StatefulSet", Name: "guestbook", Namespace: "apps"},
		Engine:             providers.EngineArgoCD,
		ManagedByKind:      "Application",
		ManagedByNamespace: "argocd",
		ManagedByName:      "guestbook",
	}
	tool := WorkloadOwnershipTool{
		Kube:      fakeOwnerWalker{oc: oc},
		Inspector: fakeInspector{status: providers.ResourceStatus{Ready: "True", Refs: map[string]string{"sync": "OutOfSync"}}},
	}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "DRIFTED") || !strings.Contains(out, "argocd-outofsync") {
		t.Fatalf("expected ArgoCD OutOfSync drift, got:\n%s", out)
	}
}

// TestWorkloadOwnershipToolFluxNotReadyDrift: a Flux Kustomization not-Ready with a
// drift/reconcile reason surfaces as the flux-not-ready-drift verdict.
func TestWorkloadOwnershipToolFluxNotReadyDrift(t *testing.T) {
	tool := WorkloadOwnershipTool{
		Kube: fakeOwnerWalker{oc: chainToKustomization()},
		Inspector: fakeInspector{status: providers.ResourceStatus{
			Ready:   "False",
			Reason:  "ReconciliationFailed",
			Message: "drift detected: Deployment/harbor-core changed out of band",
			Refs:    map[string]string{},
		}},
	}
	out, err := tool.Call(context.Background(), `{"namespace":"harbor"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "DRIFTED") || !strings.Contains(out, "flux-not-ready-drift") {
		t.Fatalf("expected Flux not-Ready drift, got:\n%s", out)
	}
}

// TestWorkloadOwnershipToolLastAppliedFallback: with no inspector-reported engine
// drift, the walker's last-applied-configuration fallback verdict is surfaced.
func TestWorkloadOwnershipToolLastAppliedFallback(t *testing.T) {
	oc := chainToKustomization()
	oc.Drift = &providers.DriftVerdict{
		Drifted: true,
		Signal:  "last-applied-configuration",
		Detail:  "Deployment/harbor-core live spec no longer matches its last kubectl-applied configuration",
	}
	tool := WorkloadOwnershipTool{
		Kube:      fakeOwnerWalker{oc: oc},
		Inspector: fakeInspector{status: providers.ResourceStatus{Ready: "True", Refs: map[string]string{}}}, // engine sees no drift
	}
	out, err := tool.Call(context.Background(), `{"namespace":"harbor"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "DRIFTED") || !strings.Contains(out, "last-applied-configuration") {
		t.Fatalf("expected last-applied fallback drift, got:\n%s", out)
	}
}

// TestWorkloadOwnershipToolNoInspector: with no GitOpsInspector wired, the tool still
// renders the chain and honors the walker's last-applied fallback (graceful degrade).
func TestWorkloadOwnershipToolNoInspector(t *testing.T) {
	oc := chainToKustomization()
	tool := WorkloadOwnershipTool{Kube: fakeOwnerWalker{oc: oc}} // Inspector nil
	out, err := tool.Call(context.Background(), `{"namespace":"harbor"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "managed by Kustomization flux-system/harbor") {
		t.Fatalf("chain should render without an inspector, got:\n%s", out)
	}
	if !strings.Contains(out, "drift: none detected") {
		t.Fatalf("expected clean no-drift without inspector, got:\n%s", out)
	}
}

var _ Tool = WorkloadOwnershipTool{}
