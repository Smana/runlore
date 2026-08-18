// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/Smana/runlore/internal/providers"
)

// errNoGroups is what client-go reports when discovery came back with nothing usable.
var errNoGroups = errors.New("unable to retrieve the complete list of server APIs")

// TestKindScopeAnswersFromDiscovery pins the fact this reader already had and nobody
// downstream could see: ServerPreferredResources reports Namespaced per resource, so
// the API server itself settles a question the renderer was guessing from a hardcoded
// list of kind NAMES.
//
// The two ACK cases are the reason the guess had to go. rds.services.k8s.aws serves a
// NAMESPACED DBInstance spelled exactly like the RDS instance an alert names, and the
// list of "kinds that are not Kubernetes objects" cannot tell them apart. Discovery
// can: on a cluster running ACK the kind resolves, on one that is not it does not.
func TestKindScopeAnswersFromDiscovery(t *testing.T) {
	tests := []struct {
		name  string
		disco fakeDiscovery
		kind  string
		want  providers.ResourceScope
	}{
		{
			name:  "a namespaced kind is reported namespaced",
			disco: discoServing("v1", metav1.APIResource{Name: "pods", Kind: "Pod", Namespaced: true}),
			kind:  "Pod",
			want:  providers.ScopeNamespaced,
		},
		{
			name:  "a cluster-scoped kind is reported cluster-scoped",
			disco: discoServing("v1", metav1.APIResource{Name: "nodes", Kind: "Node", Namespaced: false}),
			kind:  "Node",
			want:  providers.ScopeClusterScoped,
		},
		{
			name: "ACK's DBInstance is namespaced — the cloud-kind list would have called it scopeless",
			disco: discoServing("rds.services.k8s.aws/v1alpha1",
				metav1.APIResource{Name: "dbinstances", Kind: "DBInstance", Namespaced: true}),
			kind: "DBInstance",
			want: providers.ScopeNamespaced,
		},
		{
			name:  "a kind this cluster does not serve is UNKNOWN, not cluster-scoped",
			disco: discoServing("v1", metav1.APIResource{Name: "pods", Kind: "Pod", Namespaced: true}),
			kind:  "DBInstance",
			want:  providers.ScopeUnknown,
		},
		{
			name:  "matching is case-insensitive, like every other kind lookup here",
			disco: discoServing("v1", metav1.APIResource{Name: "nodes", Kind: "Node", Namespaced: false}),
			kind:  "node",
			want:  providers.ScopeClusterScoped,
		},
		{
			name:  "an empty kind asks nothing and answers unknown",
			disco: discoServing("v1", metav1.APIResource{Name: "pods", Kind: "Pod", Namespaced: true}),
			kind:  "",
			want:  providers.ScopeUnknown,
		},
		{
			name: "a subresource is not an object and cannot answer for the kind",
			disco: discoServing("v1",
				metav1.APIResource{Name: "pods/log", Kind: "Pod", Namespaced: true}),
			kind: "Pod",
			want: providers.ScopeUnknown,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := NewSpecReader(nil, tc.disco).KindScope(context.Background(), tc.kind)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("KindScope(%q) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}

// TestKindScopeRefusesToGuessWhenGroupsDisagree keeps the ambiguity honest.
//
// Two API groups can serve one Kind — NetworkPolicy is served by networking.k8s.io and
// by crd.projectcalico.org on any cluster running Calico — and Workload carries no
// group, so nothing here can say which one an alert meant. When the two disagree about
// namespacing, picking either would be the same quiet wrongness ResourceSpec refuses to
// commit; unknown sends the renderer back to its conservative default instead.
//
// Agreement is a different case: both groups saying "namespaced" IS an answer, and
// withholding it would throw away knowledge for no gain.
func TestKindScopeRefusesToGuessWhenGroupsDisagree(t *testing.T) {
	disagree := fakeDiscovery{lists: []*metav1.APIResourceList{
		{GroupVersion: "hypershift.openshift.io/v1beta1", APIResources: []metav1.APIResource{
			{Name: "nodepools", Kind: "NodePool", Namespaced: true}}},
		{GroupVersion: "karpenter.sh/v1", APIResources: []metav1.APIResource{
			{Name: "nodepools", Kind: "NodePool", Namespaced: false}}},
	}}
	got, err := NewSpecReader(nil, disagree).KindScope(context.Background(), "NodePool")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != providers.ScopeUnknown {
		t.Errorf("KindScope with disagreeing groups = %v, want ScopeUnknown", got)
	}

	agree := fakeDiscovery{lists: []*metav1.APIResourceList{
		{GroupVersion: "networking.k8s.io/v1", APIResources: []metav1.APIResource{
			{Name: "networkpolicies", Kind: "NetworkPolicy", Namespaced: true}}},
		{GroupVersion: "crd.projectcalico.org/v1", APIResources: []metav1.APIResource{
			{Name: "networkpolicies", Kind: "NetworkPolicy", Namespaced: true}}},
	}}
	got, err = NewSpecReader(nil, agree).KindScope(context.Background(), "NetworkPolicy")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != providers.ScopeNamespaced {
		t.Errorf("KindScope with agreeing groups = %v, want ScopeNamespaced", got)
	}
}

// TestKindScopeSurvivesDiscoveryFailure proves the degrade path is the conservative
// one at every level: a total discovery failure reports unknown WITH the error (the
// caller keeps whatever it did before), and a PARTIAL failure — one broken aggregated
// APIService, which is routine — still answers from the lists that did come back.
func TestKindScopeSurvivesDiscoveryFailure(t *testing.T) {
	total := fakeDiscovery{err: errNoGroups}
	got, err := NewSpecReader(nil, total).KindScope(context.Background(), "Node")
	if err == nil {
		t.Fatal("a total discovery failure must be reported, not swallowed")
	}
	if got != providers.ScopeUnknown {
		t.Errorf("scope on discovery failure = %v, want ScopeUnknown", got)
	}

	partial := fakeDiscovery{
		lists: []*metav1.APIResourceList{{GroupVersion: "v1", APIResources: []metav1.APIResource{
			{Name: "nodes", Kind: "Node", Namespaced: false}}}},
		err: errNoGroups,
	}
	got, err = NewSpecReader(nil, partial).KindScope(context.Background(), "Node")
	if err != nil {
		t.Fatalf("a partial discovery failure must still answer: %v", err)
	}
	if got != providers.ScopeClusterScoped {
		t.Errorf("scope on partial discovery = %v, want ScopeClusterScoped", got)
	}
}
