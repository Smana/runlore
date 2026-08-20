// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"fmt"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	k8stesting "k8s.io/client-go/testing"

	"github.com/Smana/runlore/internal/providers"
)

var cnpGVR = schema.GroupVersionResource{Group: "cilium.io", Version: "v2", Resource: "ciliumnetworkpolicies"}

func cnp(name, ns string) *unstructured.Unstructured {
	return &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "cilium.io/v2",
		"kind":       "CiliumNetworkPolicy",
		"metadata":   map[string]any{"name": name, "namespace": ns},
	}}
}

func cnpReader(t *testing.T, objs ...runtime.Object) *SpecReader {
	t.Helper()
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cnpGVR: "CiliumNetworkPolicyList"}, objs...)
	disco := discoServing("cilium.io/v2",
		metav1.APIResource{Name: cnpGVR.Resource, Kind: "CiliumNetworkPolicy", Namespaced: true})
	return NewSpecReader(client, disco)
}

// TestResourceListNamesObjectsThatCannotBeGuessed is the whole reason the lister exists.
// The by-name reader can only answer about a name somebody already produced; a policy
// called "orders-api-allow-payments-only" is not a name a model guesses, and a real
// investigation burned 32 tool calls proving that.
func TestResourceListNamesObjectsThatCannotBeGuessed(t *testing.T) {
	r := cnpReader(t, cnp("orders-api-allow-payments-only", "demo"), cnp("default-deny", "demo"))
	got, err := r.ResourceList(context.Background(), providers.ResourceListQuery{
		Kind: "CiliumNetworkPolicy", Namespace: "demo",
	})
	if err != nil {
		t.Fatalf("ResourceList: %v", err)
	}
	if got.Outcome != providers.ResourceFound {
		t.Fatalf("outcome = %s, want found", got.Outcome)
	}
	if len(got.Items) != 2 {
		t.Fatalf("got %d items, want 2: %+v", len(got.Items), got.Items)
	}
	found := false
	for _, it := range got.Items {
		if it.Name == "orders-api-allow-payments-only" && it.Namespace == "demo" {
			found = true
		}
	}
	if !found {
		t.Fatalf("the unguessable name was not listed: %+v", got.Items)
	}
	if got.APIVersion != "cilium.io/v2" {
		t.Fatalf("APIVersion = %q, want cilium.io/v2", got.APIVersion)
	}
}

// TestEmptyListingIsFoundNotAbsent pins the distinction the tool's rendering depends on:
// zero objects is a SUCCESSFUL listing, and it is evidence. If this returned absent or
// forbidden, the tool would disclaim the one answer that is actually informative.
func TestEmptyListingIsFoundNotAbsent(t *testing.T) {
	r := cnpReader(t)
	got, err := r.ResourceList(context.Background(), providers.ResourceListQuery{
		Kind: "CiliumNetworkPolicy", Namespace: "demo",
	})
	if err != nil {
		t.Fatalf("ResourceList: %v", err)
	}
	if got.Outcome != providers.ResourceFound {
		t.Fatalf("an empty listing must be found (it is evidence), got %s", got.Outcome)
	}
	if len(got.Items) != 0 {
		t.Fatalf("want no items, got %+v", got.Items)
	}
}

// TestForbiddenListingIsNotAnEmptyNamespace is the security-relevant one: an RBAC denial
// rendered as "there are none" invents a fact about the cluster.
func TestForbiddenListingIsNotAnEmptyNamespace(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{cnpGVR: "CiliumNetworkPolicyList"})
	client.PrependReactor("list", "ciliumnetworkpolicies", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(cnpGVR.GroupResource(), "", fmt.Errorf("no permission"))
	})
	disco := discoServing("cilium.io/v2",
		metav1.APIResource{Name: cnpGVR.Resource, Kind: "CiliumNetworkPolicy", Namespaced: true})
	r := NewSpecReader(client, disco)

	got, err := r.ResourceList(context.Background(), providers.ResourceListQuery{
		Kind: "CiliumNetworkPolicy", Namespace: "demo",
	})
	if err != nil {
		t.Fatalf("ResourceList: %v", err)
	}
	if got.Outcome != providers.ResourceForbidden {
		t.Fatalf("an RBAC denial must be forbidden, not %s — otherwise it reads as an empty namespace", got.Outcome)
	}
}

// TestListRefusesSecretByKindAndByResource keeps the lister failing closed exactly where
// the by-name reader does. Listing Secret NAMES is still listing secrets, and the second
// arm covers a `secrets` resource served under some other Kind.
func TestListRefusesSecretByKindAndByResource(t *testing.T) {
	secretGVR := schema.GroupVersionResource{Version: "v1", Resource: "secrets"}
	for _, tc := range []struct{ name, kind string }{
		{"by kind", "Secret"},
		{"by resolved resource", "Sealed"}, // a Kind of somebody else's choosing over `secrets`
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
				map[schema.GroupVersionResource]string{secretGVR: "SecretList"})
			disco := discoServing("v1",
				metav1.APIResource{Name: "secrets", Kind: tc.kind, Namespaced: true})
			r := NewSpecReader(client, disco)
			got, err := r.ResourceList(context.Background(), providers.ResourceListQuery{
				Kind: tc.kind, Namespace: "demo",
			})
			if err != nil {
				t.Fatalf("ResourceList: %v", err)
			}
			if got.Outcome != providers.ResourceRefused {
				t.Fatalf("listing %s must be refused, got %s", tc.kind, got.Outcome)
			}
		})
	}
}

// TestListAmbiguousKindListsNothing mirrors the by-name reader: acting on the wrong
// group's objects and naming them confidently is worse than answering nothing.
func TestListAmbiguousKindListsNothing(t *testing.T) {
	client := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{
			{Group: "networking.k8s.io", Version: "v1", Resource: "networkpolicies"}: "NetworkPolicyList",
		})
	disco := fakeDiscovery{lists: []*metav1.APIResourceList{
		{GroupVersion: "networking.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "networkpolicies", Kind: "NetworkPolicy", Namespaced: true}}},
		{GroupVersion: "crd.projectcalico.org/v1", APIResources: []metav1.APIResource{
			{Name: "networkpolicies", Kind: "NetworkPolicy", Namespaced: true}}},
	}}
	r := NewSpecReader(client, disco)
	got, err := r.ResourceList(context.Background(), providers.ResourceListQuery{
		Kind: "NetworkPolicy", Namespace: "demo",
	})
	if err != nil {
		t.Fatalf("ResourceList: %v", err)
	}
	if got.Outcome != providers.ResourceKindAmbiguous {
		t.Fatalf("outcome = %s, want kind_ambiguous", got.Outcome)
	}
	if len(got.Items) != 0 {
		t.Fatalf("an ambiguous kind must list nothing, got %+v", got.Items)
	}
}

// TestNamespacedKindRequiresNamespace stops a namespaced listing silently becoming a
// cluster-wide one, which would both widen the read and misreport its scope.
func TestNamespacedKindRequiresNamespace(t *testing.T) {
	r := cnpReader(t)
	_, err := r.ResourceList(context.Background(), providers.ResourceListQuery{Kind: "CiliumNetworkPolicy"})
	if err == nil {
		t.Fatal("a namespaced kind listed without a namespace must error")
	}
}

// TestUnknownKindIsNotAnEmptyListing keeps "this cluster has no such kind" out of the
// answer "there are none of them here".
func TestUnknownKindIsNotAnEmptyListing(t *testing.T) {
	r := cnpReader(t)
	got, err := r.ResourceList(context.Background(), providers.ResourceListQuery{
		Kind: "Widget", Namespace: "demo",
	})
	if err != nil {
		t.Fatalf("ResourceList: %v", err)
	}
	if got.Outcome != providers.ResourceKindUnknown {
		t.Fatalf("outcome = %s, want kind_unknown", got.Outcome)
	}
}
