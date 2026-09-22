// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestIncidentKeyFoldsStatefulSetReplicas is #513 on the WRITE side: the same fault
// on two replicas of one StatefulSet is one incident identity, exactly as it already
// is on two pods of one Deployment — so the recurrence chain, suppression and the
// dedup key stop restarting per replica. The Kind is what licenses the fold: a
// kind-less numbered name is a name, and two databases must not key alike.
func TestIncidentKeyFoldsStatefulSetReplicas(t *testing.T) {
	pod := func(name string) providers.Workload {
		return providers.Workload{Kind: "Pod", Namespace: "observability", Name: name}
	}
	a := IncidentKey("VMAgentHighMemory", pod("vmagent-vmagent-0"), "prod")
	b := IncidentKey("VMAgentHighMemory", pod("vmagent-vmagent-1"), "prod")
	if a == "" || a != b {
		t.Fatalf("two replicas of one StatefulSet must key alike: %q vs %q", a, b)
	}
	if !strings.Contains(a, "|vmagent-vmagent|") {
		t.Errorf("key must carry the StatefulSet family, got %q", a)
	}
	db := func(name string) providers.Workload {
		return providers.Workload{Namespace: "observability", Name: name, Account: "111111111111"}
	}
	if IncidentKey("RDSCPUHigh", db("aurora-serverless-postgres-old-1"), "prod") ==
		IncidentKey("RDSCPUHigh", db("aurora-serverless-postgres-old-2"), "prod") {
		t.Fatal("two databases that differ by a trailing digit must not key alike")
	}
}

// TestDupFingerprintFoldsStatefulSetReplicas: the curator's dedup fingerprint reads
// the same identity, so a finding on replica 1 coalesces with the entry filed from
// replica 0 instead of filing a second KB entry saying the same thing.
func TestDupFingerprintFoldsStatefulSetReplicas(t *testing.T) {
	base := providers.Investigation{
		Resource:   providers.Workload{Kind: "Pod", Namespace: "observability", Name: "vmagent-vmagent-0"},
		RootCauses: []providers.Hypothesis{{Summary: "scrape cardinality grew past the memory limit"}},
	}
	other := base
	other.Resource = providers.Workload{Kind: "Pod", Namespace: "observability", Name: "vmagent-vmagent-1"}
	if fa, fb := DupFingerprint(base), DupFingerprint(other); fa == "" || fa != fb {
		t.Fatalf("same cause on two replicas must fingerprint alike: %q vs %q", fa, fb)
	}
}

// TestDraftKBEntryFoldsStatefulSetReplica is #513 at the stored resource: an entry
// written from a StatefulSet pod is filed under the StatefulSet, not the replica, so
// the human-facing ref stops naming one of N identical pods and the READ side has
// one family to match. A kind-less numbered resource is written as named.
func TestDraftKBEntryFoldsStatefulSetReplica(t *testing.T) {
	cases := []struct {
		name     string
		resource providers.Workload
		want     string
	}{
		{"a StatefulSet pod is filed under its StatefulSet",
			providers.Workload{Kind: "Pod", Namespace: "observability", Name: "vmagent-vmagent-1"},
			"observability/vmagent-vmagent"},
		{"a kind-less numbered resource is its own name",
			providers.Workload{Namespace: "observability", Name: "aurora-serverless-postgres-old-1"},
			"observability/aurora-serverless-postgres-old-1"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			inv := providers.Investigation{
				Title:      "VMAgentHighMemory",
				Confidence: 0.9,
				Resource:   tt.resource,
				RootCauses: []providers.Hypothesis{{Summary: "scrape cardinality grew", Evidence: []string{"e"}}},
			}
			if got := draftKBEntry(inv).Resource; got != tt.want {
				t.Fatalf("KBEntry.Resource = %q, want %q", got, tt.want)
			}
		})
	}
}
