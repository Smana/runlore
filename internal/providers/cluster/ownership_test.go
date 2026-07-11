// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/Smana/runlore/internal/providers"
)

// ownerRef builds a controller ownerReference (Controller=true) for the walk.
func ownerRef(kind, name string) metav1.OwnerReference {
	ctrl := true
	return metav1.OwnerReference{Kind: kind, Name: name, Controller: &ctrl}
}

// TestWorkloadOwnershipPodToDeploymentToKustomization is the core owner-chain walk:
// Pod → ReplicaSet → Deployment, then the Flux tracking labels on the Deployment name
// the owning Kustomization (engine-agnostic strings — no Flux types leak out).
func TestWorkloadOwnershipPodToDeploymentToKustomization(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "harbor-core-59598dbd57-ltkzw",
			Namespace:       "harbor",
			Labels:          map[string]string{"app": "harbor-core"},
			OwnerReferences: []metav1.OwnerReference{ownerRef("ReplicaSet", "harbor-core-59598dbd57")},
		},
	}
	rs := &appsv1.ReplicaSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "harbor-core-59598dbd57",
			Namespace:       "harbor",
			OwnerReferences: []metav1.OwnerReference{ownerRef("Deployment", "harbor-core")},
		},
	}
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "harbor-core",
			Namespace: "harbor",
			Labels: map[string]string{
				"kustomize.toolkit.fluxcd.io/name":      "harbor",
				"kustomize.toolkit.fluxcd.io/namespace": "flux-system",
			},
		},
	}
	r := New(fake.NewSimpleClientset(pod, rs, dep))

	oc, err := r.WorkloadOwnership(context.Background(), "harbor", "app=harbor-core", "")
	if err != nil {
		t.Fatalf("WorkloadOwnership: %v", err)
	}
	if oc.Top.Kind != "Deployment" || oc.Top.Name != "harbor-core" {
		t.Fatalf("top controller = %+v, want Deployment/harbor-core", oc.Top)
	}
	if oc.Engine != providers.EngineFlux {
		t.Fatalf("engine = %q, want flux", oc.Engine)
	}
	if oc.ManagedByKind != "Kustomization" || oc.ManagedByName != "harbor" || oc.ManagedByNamespace != "flux-system" {
		t.Fatalf("managed-by = %s %s/%s, want Kustomization flux-system/harbor",
			oc.ManagedByKind, oc.ManagedByNamespace, oc.ManagedByName)
	}
	// The chain records every hop: pod first, top controller last.
	if len(oc.Chain) != 3 || oc.Chain[0].Kind != "Pod" || oc.Chain[2].Kind != "Deployment" {
		t.Fatalf("chain = %+v, want Pod→ReplicaSet→Deployment", oc.Chain)
	}
}

// TestWorkloadOwnershipArgoInstanceLabel: an ArgoCD tracking label on the top
// controller names the owning Application (engine=argocd).
func TestWorkloadOwnershipArgoInstanceLabel(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "guestbook-abc",
			Namespace:       "apps",
			OwnerReferences: []metav1.OwnerReference{ownerRef("StatefulSet", "guestbook")},
		},
	}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "guestbook",
			Namespace: "apps",
			Labels:    map[string]string{"argocd.argoproj.io/instance": "guestbook"},
		},
	}
	r := New(fake.NewSimpleClientset(pod, sts))

	oc, err := r.WorkloadOwnership(context.Background(), "apps", "", "")
	if err != nil {
		t.Fatalf("WorkloadOwnership: %v", err)
	}
	if oc.Top.Kind != "StatefulSet" || oc.Top.Name != "guestbook" {
		t.Fatalf("top = %+v, want StatefulSet/guestbook", oc.Top)
	}
	if oc.Engine != providers.EngineArgoCD || oc.ManagedByKind != "Application" || oc.ManagedByName != "guestbook" {
		t.Fatalf("managed-by = %s %q engine=%q, want Application guestbook argocd",
			oc.ManagedByKind, oc.ManagedByName, oc.Engine)
	}
}

// TestWorkloadOwnershipLastAppliedDrift: the live Deployment differs from its
// kubectl.kubernetes.io/last-applied-configuration annotation — a manual kubectl
// edit — so the generic fallback drift signal fires.
func TestWorkloadOwnershipLastAppliedDrift(t *testing.T) {
	// last-applied recorded 3 replicas; the live spec says 5 (someone kubectl-scaled it).
	const lastApplied = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"apps"},"spec":{"replicas":3}}`
	five := int32(5)
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "web",
			Namespace: "apps",
			Annotations: map[string]string{
				"kubectl.kubernetes.io/last-applied-configuration": lastApplied,
			},
		},
		Spec: appsv1.DeploymentSpec{Replicas: &five},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "web-0",
			Namespace:       "apps",
			OwnerReferences: []metav1.OwnerReference{ownerRef("Deployment", "web")},
		},
	}
	r := New(fake.NewSimpleClientset(pod, dep))

	oc, err := r.WorkloadOwnership(context.Background(), "apps", "", "")
	if err != nil {
		t.Fatalf("WorkloadOwnership: %v", err)
	}
	if oc.Drift == nil || !oc.Drift.Drifted {
		t.Fatalf("expected last-applied drift verdict, got %+v", oc.Drift)
	}
	if oc.Drift.Signal != "last-applied-configuration" {
		t.Fatalf("drift signal = %q, want last-applied-configuration", oc.Drift.Signal)
	}
}

// TestWorkloadOwnershipNoDriftWhenMatching: when the live spec matches last-applied,
// no drift is reported (nil verdict).
func TestWorkloadOwnershipNoDriftWhenMatching(t *testing.T) {
	three := int32(3)
	const lastApplied = `{"apiVersion":"apps/v1","kind":"Deployment","metadata":{"name":"web","namespace":"apps"},"spec":{"replicas":3}}`
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web",
			Namespace:   "apps",
			Annotations: map[string]string{"kubectl.kubernetes.io/last-applied-configuration": lastApplied},
		},
		Spec: appsv1.DeploymentSpec{Replicas: &three},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "web-0",
			Namespace:       "apps",
			OwnerReferences: []metav1.OwnerReference{ownerRef("Deployment", "web")},
		},
	}
	r := New(fake.NewSimpleClientset(pod, dep))

	oc, err := r.WorkloadOwnership(context.Background(), "apps", "", "")
	if err != nil {
		t.Fatalf("WorkloadOwnership: %v", err)
	}
	if oc.Drift != nil {
		t.Fatalf("expected no drift when live matches last-applied, got %+v", oc.Drift)
	}
}

// TestWorkloadOwnershipBarePod: a pod with no controller owner resolves to a chain of
// one and a zero-valued Top (no GitOps owner), without error.
func TestWorkloadOwnershipBarePod(t *testing.T) {
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "lonely", Namespace: "apps"}}
	r := New(fake.NewSimpleClientset(pod))

	oc, err := r.WorkloadOwnership(context.Background(), "apps", "", "")
	if err != nil {
		t.Fatalf("WorkloadOwnership: %v", err)
	}
	if oc.Top.Kind != "" {
		t.Fatalf("bare pod should have no top controller, got %+v", oc.Top)
	}
	if len(oc.Chain) != 1 || oc.Chain[0].Kind != "Pod" {
		t.Fatalf("chain = %+v, want single Pod hop", oc.Chain)
	}
}

// TestWorkloadOwnershipNoPods: a selector matching no pods returns a not-found error
// (the tool renders it as missing data, not silence).
func TestWorkloadOwnershipNoPods(t *testing.T) {
	r := New(fake.NewSimpleClientset())
	_, err := r.WorkloadOwnership(context.Background(), "apps", "app=nope", "")
	if err == nil {
		t.Fatal("expected an error when no pods match the selector")
	}
}

var _ providers.OwnerWalker = (*Reader)(nil)
