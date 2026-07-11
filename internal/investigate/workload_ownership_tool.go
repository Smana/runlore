// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// WorkloadOwnershipTool answers two questions the Git-only what_changed tool cannot
// (G4): (a) WHICH GitOps object owns a failing pod — resolved by walking real
// ownerReferences (Pod → ReplicaSet → Deployment/StatefulSet/DaemonSet/Job) and the
// top controller's Flux/ArgoCD tracking labels, not by name-guessing; and (b) whether
// the workload's LIVE state has DRIFTED from what GitOps applied (the classic
// "someone kubectl-edited it" cause).
//
// Drift is surfaced by the most authoritative, cheapest signal available, in
// priority order:
//  1. the GitOps engine's OWN verdict for the owning object (via GitOpsInspector):
//     ArgoCD sync == OutOfSync, or a Flux object not-Ready with a drift/reconcile
//     reason — the engine already computes this, so it is authoritative and cheap;
//  2. the generic kubectl.kubernetes.io/last-applied-configuration fallback computed
//     during the ownership walk (catches a manual kubectl-apply edit even off-GitOps).
//
// A full live-spec-vs-Git-render diff is deliberately OUT OF SCOPE (too heavy); this
// tool surfaces the drift VERDICT and which signal produced it, not the diff.
type WorkloadOwnershipTool struct {
	Kube      providers.OwnerWalker     // owner-chain walk + last-applied fallback
	Inspector providers.GitOpsInspector // authoritative engine drift verdict on the owning object
}

// Name returns the tool name.
func (t WorkloadOwnershipTool) Name() string { return "workload_ownership" }

// Description returns the tool description.
func (t WorkloadOwnershipTool) Description() string {
	return "Given a failing workload (namespace + label selector, or a specific pod), walk its " +
		"ownerReferences (Pod → ReplicaSet → Deployment/StatefulSet/DaemonSet/Job) to find (a) WHICH " +
		"GitOps object owns it (Flux Kustomization/HelmRelease or Argo CD Application — resolved from the " +
		"controller's tracking labels, not guessed by name) and (b) whether the LIVE state has DRIFTED " +
		"from what GitOps applied (a manual `kubectl edit`/scale). Use this when a workload is failing and " +
		"you need to know its GitOps owner and whether an out-of-band change caused it — what_changed only " +
		"sees Git, not live drift."
}

// Schema returns the JSON schema for the arguments.
func (t WorkloadOwnershipTool) Schema() string {
	return `{"type":"object","properties":{"namespace":{"type":"string"},"selector":{"type":"string","description":"optional label selector, e.g. app=harbor-core"},"pod":{"type":"string","description":"optional exact pod name; overrides the selector's first match"}},"required":["namespace"]}`
}

// Call resolves the owner chain and drift verdict and renders them compactly.
func (t WorkloadOwnershipTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Namespace string `json:"namespace"`
		Selector  string `json:"selector"`
		Pod       string `json:"pod"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	oc, err := t.Kube.WorkloadOwnership(ctx, in.Namespace, in.Selector, in.Pod)
	if err != nil {
		return "", err
	}

	var b strings.Builder
	// The owner chain, engine-agnostic: "pod → Deployment/harbor-core → managed by
	// Kustomization flux-system/harbor".
	b.WriteString(renderChain(oc))
	b.WriteString("\n")

	// A resolved GitOps owner + a top controller we can point the model at server-side.
	if oc.Top.Kind != "" {
		recordObserved(ctx, providers.Workload{Kind: oc.Top.Kind, Name: oc.Top.Name, Namespace: oc.Top.Namespace})
	}

	// Drift: prefer the GitOps engine's OWN verdict for the owning object (authoritative),
	// then fall back to the last-applied-configuration signal from the walk.
	switch v := t.engineDrift(ctx, oc); {
	case v != nil:
		fmt.Fprintf(&b, "drift: DRIFTED — signal=%s; %s\n", v.Signal, v.Detail)
	case oc.Drift != nil && oc.Drift.Drifted:
		fmt.Fprintf(&b, "drift: DRIFTED — signal=%s; %s\n", oc.Drift.Signal, oc.Drift.Detail)
	case oc.ManagedByKind != "":
		b.WriteString("drift: none detected — the owning GitOps object reports in-sync/Ready and the live " +
			"spec matches its last-applied configuration (no out-of-band edit found).\n")
	default:
		b.WriteString("drift: not assessed — no GitOps owner was resolved from the controller's tracking " +
			"labels, so there is no applied baseline to compare against.\n")
	}
	return b.String(), nil
}

// renderChain renders the ownerReferences walk plus the resolved GitOps owner as one
// engine-agnostic line, e.g.:
//
//	pod/harbor-core-… → ReplicaSet/harbor-core-… → Deployment/harbor-core → managed by Kustomization flux-system/harbor
func renderChain(oc providers.OwnerChain) string {
	hops := make([]string, 0, len(oc.Chain))
	for _, l := range oc.Chain {
		hops = append(hops, l.Kind+"/"+l.Name)
	}
	chain := strings.Join(hops, " → ")
	if oc.ManagedByKind == "" {
		if oc.Top.Kind == "" {
			return chain + " (no controller owner — a bare pod; no GitOps object owns it)"
		}
		return chain + " (no GitOps tracking label on the top controller — owner unresolved)"
	}
	owner := oc.ManagedByKind + " "
	if oc.ManagedByNamespace != "" {
		owner += oc.ManagedByNamespace + "/"
	}
	owner += oc.ManagedByName
	return fmt.Sprintf("%s → managed by %s (%s)", chain, owner, oc.Engine)
}

// engineDrift asks the GitOps engine for its OWN drift verdict on the owning object —
// the authoritative signal. ArgoCD: Application status.sync.status == OutOfSync (Argo
// already computes drift). Flux: the Kustomization/HelmRelease is not-Ready with a
// reason that names drift/reconciliation. Returns nil when no inspector is wired, no
// owner was resolved, or the engine reports no drift.
func (t WorkloadOwnershipTool) engineDrift(ctx context.Context, oc providers.OwnerChain) *providers.DriftVerdict {
	if t.Inspector == nil || oc.ManagedByKind == "" {
		return nil
	}
	rs, err := t.Inspector.ResourceStatus(ctx, providers.Workload{
		Kind:      oc.ManagedByKind,
		Name:      oc.ManagedByName,
		Namespace: oc.ManagedByNamespace,
	})
	if err != nil || rs.NotFound {
		return nil // can't confirm drift — let the last-applied fallback speak instead
	}
	// The owning GitOps object exists server-side — record it observed.
	recordObserved(ctx, rs.Workload)

	switch oc.Engine {
	case providers.EngineArgoCD:
		// Argo CD surfaces its computed sync verdict via the "sync" ref (see argocd
		// inspector). OutOfSync == the engine's own drift finding.
		if strings.EqualFold(rs.Refs["sync"], "OutOfSync") {
			return &providers.DriftVerdict{
				Drifted: true,
				Signal:  "argocd-outofsync",
				Detail: fmt.Sprintf("Argo CD Application %s is OutOfSync — the engine computed that live "+
					"state diverged from Git%s", nsName(oc.ManagedByNamespace, oc.ManagedByName), readyDetail(rs)),
			}
		}
	case providers.EngineFlux:
		// Flux marks a drift-triggered reconcile / a failed reconcile via a not-Ready
		// condition whose reason names drift or reconciliation.
		if rs.Ready == "False" && driftyReason(rs.Reason, rs.Message) {
			return &providers.DriftVerdict{
				Drifted: true,
				Signal:  "flux-not-ready-drift",
				Detail: fmt.Sprintf("Flux %s %s is not Ready (%s) — reconciliation/drift correction is failing%s",
					oc.ManagedByKind, nsName(oc.ManagedByNamespace, oc.ManagedByName), emptyDash(rs.Reason), readyDetail(rs)),
			}
		}
	}
	return nil
}

// driftyReason reports whether a Flux not-Ready reason/message names drift or a
// (failed) reconcile — the signal that the live object diverged from the applied
// revision and Flux is trying (or failing) to correct it.
func driftyReason(reason, message string) bool {
	hay := strings.ToLower(reason + " " + message)
	for _, kw := range []string{"drift", "reconcil", "diff", "not applied", "healthcheck"} {
		if strings.Contains(hay, kw) {
			return true
		}
	}
	return false
}

// readyDetail appends the owning object's Ready message when present, for context.
func readyDetail(rs providers.ResourceStatus) string {
	if rs.Message == "" {
		return ""
	}
	return ": " + rs.Message
}

func nsName(ns, name string) string {
	if ns == "" {
		return name
	}
	return ns + "/" + name
}
