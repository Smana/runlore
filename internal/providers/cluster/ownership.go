// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Smana/runlore/internal/providers"
)

// GitOps tracking-label keys read off the TOP controller to name the owning GitOps
// object. Flux stamps kustomize.toolkit.fluxcd.io/{name,namespace}; ArgoCD stamps
// argocd.argoproj.io/instance (its default tracking label) or the shared
// app.kubernetes.io/instance. These are the authoritative, cheap ownership links —
// no vendor SDK, just labels already on the object (G4).
const (
	fluxKustomizeNameLabel = "kustomize.toolkit.fluxcd.io/name"
	fluxKustomizeNSLabel   = "kustomize.toolkit.fluxcd.io/namespace"
	fluxHelmNameLabel      = "helm.toolkit.fluxcd.io/name"
	fluxHelmNSLabel        = "helm.toolkit.fluxcd.io/namespace"
	argoInstanceLabel      = "argocd.argoproj.io/instance"
	appInstanceLabel       = "app.kubernetes.io/instance"

	// lastAppliedAnnotation is the classic `kubectl apply` snapshot: the generic
	// fallback for detecting a manual out-of-band edit when neither GitOps engine's
	// own drift verdict is available.
	lastAppliedAnnotation = "kubectl.kubernetes.io/last-applied-configuration"

	// ownerWalkMaxHops bounds the ownerReferences walk so a (malformed) ownership
	// cycle can never unbound the fetch.
	ownerWalkMaxHops = 8
)

var _ providers.OwnerWalker = (*Reader)(nil)

// ownerMeta is the minimal, engine-agnostic view of an object needed for the walk:
// its identity, its controller owner, its labels (for tracking-label resolution),
// and its annotations + a canonical spec snapshot (for last-applied drift).
type ownerMeta struct {
	kind        string
	name        string
	namespace   string
	controller  *metav1.OwnerReference // the Controller=true owner, if any
	labels      map[string]string
	annotations map[string]string
	spec        any // decoded spec for last-applied comparison; nil when N/A
}

// WorkloadOwnership resolves the owner chain for the pods selected by (namespace,
// labelSelector) — Pod → ReplicaSet → Deployment (or StatefulSet/DaemonSet/Job) — then
// reads the top controller's Flux/ArgoCD tracking labels to name the owning GitOps
// object, and computes the generic last-applied-configuration drift signal on the top
// controller. It picks podName when set, else the first pod the selector returns. A
// selector matching no pods is an error (missing data), not a silent empty chain.
func (r *Reader) WorkloadOwnership(ctx context.Context, namespace, labelSelector, podName string) (providers.OwnerChain, error) {
	pods, err := r.client.CoreV1().Pods(namespace).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return providers.OwnerChain{}, fmt.Errorf("list pods (%s/%s): %w", namespace, labelSelector, err)
	}
	if len(pods.Items) == 0 {
		return providers.OwnerChain{}, fmt.Errorf("no pods in namespace %q matching selector %q", namespace, labelSelector)
	}
	start := &pods.Items[0]
	if podName != "" {
		found := false
		for i := range pods.Items {
			if pods.Items[i].Name == podName {
				start = &pods.Items[i]
				found = true
				break
			}
		}
		if !found {
			return providers.OwnerChain{}, fmt.Errorf("pod %q not found in namespace %q (selector %q)", podName, namespace, labelSelector)
		}
	}

	var oc providers.OwnerChain
	cur := ownerMeta{
		kind:       "Pod",
		name:       start.Name,
		namespace:  start.Namespace,
		controller: controllerRef(start.OwnerReferences),
	}
	oc.Chain = append(oc.Chain, providers.OwnerLink{Kind: cur.kind, Name: cur.name, Namespace: cur.namespace})

	// Walk up controller owners until there is no further controller (the top).
	seen := map[string]bool{}
	for hop := 0; cur.controller != nil && hop < ownerWalkMaxHops; hop++ {
		next := cur.controller
		key := next.Kind + "/" + next.Name
		if seen[key] {
			break // malformed ownership cycle guard
		}
		seen[key] = true
		om, err := r.fetchOwner(ctx, next.Kind, namespace, next.Name)
		if err != nil {
			// A broken link (RBAC, GC race) stops the walk — we keep the chain we have
			// rather than failing the whole tool; the last hop is treated as the top.
			break
		}
		oc.Chain = append(oc.Chain, providers.OwnerLink{Kind: om.kind, Name: om.name, Namespace: om.namespace})
		cur = om
	}

	// The last hop past the Pod is the top controller (zero-valued for a bare pod).
	if len(oc.Chain) > 1 {
		oc.Top = oc.Chain[len(oc.Chain)-1]
		oc.Engine, oc.ManagedByKind, oc.ManagedByNamespace, oc.ManagedByName = trackingOwner(cur.labels, oc.Top.Namespace)
		oc.Drift = lastAppliedDrift(cur)
	}
	return oc, nil
}

// controllerRef returns the Controller=true ownerReference, or nil when none.
func controllerRef(refs []metav1.OwnerReference) *metav1.OwnerReference {
	for i := range refs {
		if refs[i].Controller != nil && *refs[i].Controller {
			return &refs[i]
		}
	}
	return nil
}

// fetchOwner reads a controller object by kind/namespace/name and reduces it to the
// engine-agnostic ownerMeta. Only the workload kinds a pod can be owned by are
// handled (ReplicaSet/Deployment/StatefulSet/DaemonSet/Job); an unknown kind returns
// an error so the walk stops cleanly at the last resolvable hop.
func (r *Reader) fetchOwner(ctx context.Context, kind, namespace, name string) (ownerMeta, error) {
	get := metav1.GetOptions{}
	switch kind {
	case "ReplicaSet":
		o, err := r.client.AppsV1().ReplicaSets(namespace).Get(ctx, name, get)
		if err != nil {
			return ownerMeta{}, err
		}
		return ownerMeta{kind, o.Name, o.Namespace, controllerRef(o.OwnerReferences), o.Labels, o.Annotations, o.Spec}, nil
	case "Deployment":
		o, err := r.client.AppsV1().Deployments(namespace).Get(ctx, name, get)
		if err != nil {
			return ownerMeta{}, err
		}
		return ownerMeta{kind, o.Name, o.Namespace, controllerRef(o.OwnerReferences), o.Labels, o.Annotations, o.Spec}, nil
	case "StatefulSet":
		o, err := r.client.AppsV1().StatefulSets(namespace).Get(ctx, name, get)
		if err != nil {
			return ownerMeta{}, err
		}
		return ownerMeta{kind, o.Name, o.Namespace, controllerRef(o.OwnerReferences), o.Labels, o.Annotations, o.Spec}, nil
	case "DaemonSet":
		o, err := r.client.AppsV1().DaemonSets(namespace).Get(ctx, name, get)
		if err != nil {
			return ownerMeta{}, err
		}
		return ownerMeta{kind, o.Name, o.Namespace, controllerRef(o.OwnerReferences), o.Labels, o.Annotations, o.Spec}, nil
	case "Job":
		o, err := r.client.BatchV1().Jobs(namespace).Get(ctx, name, get)
		if err != nil {
			return ownerMeta{}, err
		}
		return ownerMeta{kind, o.Name, o.Namespace, controllerRef(o.OwnerReferences), o.Labels, o.Annotations, o.Spec}, nil
	default:
		return ownerMeta{}, fmt.Errorf("unhandled controller kind %q", kind)
	}
}

// trackingOwner reads the GitOps engine's tracking labels off a top controller and
// returns the engine-agnostic owning-object identity. Flux's kustomize/helm tracking
// labels win first (they carry an explicit namespace); ArgoCD's instance label names
// an Application (defaulting its namespace to the controller's — Argo runs the app in
// its own namespace, so this is only a hint). Returns "" engine + kind when no
// tracking label is present.
func trackingOwner(labels map[string]string, defaultNS string) (providers.Engine, string, string, string) {
	if n := labels[fluxKustomizeNameLabel]; n != "" {
		return providers.EngineFlux, "Kustomization", labels[fluxKustomizeNSLabel], n
	}
	if n := labels[fluxHelmNameLabel]; n != "" {
		return providers.EngineFlux, "HelmRelease", labels[fluxHelmNSLabel], n
	}
	if n := labels[argoInstanceLabel]; n != "" {
		return providers.EngineArgoCD, "Application", defaultNS, n
	}
	if n := labels[appInstanceLabel]; n != "" {
		// app.kubernetes.io/instance is shared (Helm/kustomize also set it), so it is a
		// weaker signal than the Argo-specific label above — treated as ArgoCD only when
		// the Argo-specific label was absent.
		return providers.EngineArgoCD, "Application", defaultNS, n
	}
	return "", "", "", ""
}

// lastAppliedDrift compares a top controller's LIVE spec against the spec captured in
// its kubectl.kubernetes.io/last-applied-configuration annotation — the generic
// fallback that catches a manual `kubectl edit`/`kubectl scale` when no GitOps-engine
// verdict is available. Returns nil when the annotation is absent (nothing to compare)
// or the specs match; a non-nil Drifted verdict otherwise. It compares the SPEC
// subtree only (status/metadata churn is not drift) and never emits a full diff.
func lastAppliedDrift(om ownerMeta) *providers.DriftVerdict {
	raw := om.annotations[lastAppliedAnnotation]
	if raw == "" || om.spec == nil {
		return nil
	}
	var applied map[string]any
	if err := json.Unmarshal([]byte(raw), &applied); err != nil {
		return nil // unparseable annotation — no reliable signal
	}
	appliedSpec, ok := applied["spec"]
	if !ok {
		return nil // last-applied carried no spec (e.g. metadata-only apply)
	}
	// Normalize the live typed spec through JSON so it compares like-for-like against
	// the annotation's decoded JSON (numbers become float64, keys become strings).
	liveJSON, err := json.Marshal(om.spec)
	if err != nil {
		return nil
	}
	var liveSpec any
	if err := json.Unmarshal(liveJSON, &liveSpec); err != nil {
		return nil
	}
	// last-applied is a SUBSET of the live spec (it records only fields the user set;
	// defaults are filled in server-side). Drift = a field the user applied whose live
	// value now differs — so we check the applied spec is still a subset of live.
	if specSubsetMatches(appliedSpec, liveSpec) {
		return nil
	}
	return &providers.DriftVerdict{
		Drifted: true,
		Signal:  "last-applied-configuration",
		Detail: fmt.Sprintf("%s/%s live spec no longer matches its last kubectl-applied configuration "+
			"(a manual out-of-band edit); a GitOps reconcile has not reverted it", om.kind, om.name),
	}
}

// specSubsetMatches reports whether every field present in the last-applied spec
// (want) still holds the same value in the live spec (got). Maps recurse key-by-key;
// slices and scalars must equal. A field the user applied that live no longer matches
// means someone edited it out-of-band → drift. Server-added fields (present in live,
// absent in want) are ignored — they are defaults, not drift.
func specSubsetMatches(want, got any) bool {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return false
		}
		for k, wv := range w {
			gv, ok := g[k]
			if !ok {
				return false // an applied field vanished from live → drift
			}
			if !specSubsetMatches(wv, gv) {
				return false
			}
		}
		return true
	default:
		return reflect.DeepEqual(want, got)
	}
}
