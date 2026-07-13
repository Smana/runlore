// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// The live Harbor incident, reduced to its structural core.
//
// The alert fires on the HelmRelease `tooling/harbor`. The entry that actually
// explains it is filed on the pod `tooling/harbor-registry` — where the fault IS.
// Both resources are correct; they are one step apart in the ownership chain.
// resourceAgrees rejects the pair (rightly, for instant recall). The near-miss must
// not.
var (
	harborAlert   = providers.Workload{Namespace: "tooling", Name: "harbor"}
	harborIAMPath = "iam.md"
)

func harborIAMCatalog() fakeScored {
	return fakeScored{hits: []catalog.ScoredEntry{{
		Entry: catalog.Entry{
			Title:    "Harbor Registry Down due to IAM Access Key Quota Limit",
			Path:     harborIAMPath,
			Resource: "tooling/harbor-registry",
			Body:     "## Cause\nAccessKey/xplane-harbor hit AccessKeysPerUser: 2.\n\n## Resolution\nDelete an unused IAM access key.\n",
		},
		Score: 3.0,
	}}}
}

func harborReq() Request {
	return Request{
		Title:       "HelmRelease/harbor InstallFailed",
		Fingerprint: "fp-gate",
		Workload:    harborAlert,
		Labels:      map[string]string{"alertname": "HelmReleaseInstallFailed"},
	}
}

// TestNearMissAdmitsSameNamespaceDifferentWorkload is the regression test. Before this
// change the entry was invisible to BOTH instant recall and the near-miss, so a full
// paid investigation ran beside a catalog that held the answer.
func TestNearMissAdmitsSameNamespaceDifferentWorkload(t *testing.T) {
	r := &Recall{
		MinScore: 4.0, MarginGap: 2.0, SoloFloor: 4.0, // deliberately unreachable: we want the near-miss path
		Catalog: harborIAMCatalog(),
	}
	nm := r.nearMiss(context.Background(), harborReq())
	if nm == nil {
		t.Fatal("near-miss must admit a same-namespace entry on a different workload: " +
			"an alert on tooling/harbor and a past incident on tooling/harbor-registry are the same failure, one step apart in the ownership chain")
	}
	if nm.Path != harborIAMPath {
		t.Fatalf("wrong near-miss entry: %s", nm.Path)
	}
}

// TestInstantRecallStillRejectsDifferentWorkload is the SAFETY property, and the reason
// this fix does not simply loosen resourceAgrees.
//
// Instant recall short-circuits the loop and presents the entry as the answer. It must
// keep refusing a runbook from a different named workload — auto-applying a pod's
// runbook to a HelmRelease alert would be wrong. Loosening the near-miss must not
// loosen this.
func TestInstantRecallStillRejectsDifferentWorkload(t *testing.T) {
	r := &Recall{
		MinScore: 0.1, MarginGap: 0.0, SoloFloor: 0.1, // wide open: only the STRUCTURAL gate can stop it
		Catalog: harborIAMCatalog(),
	}
	entry, conf := r.lookup(context.Background(), harborReq())
	if entry != nil {
		t.Fatalf("instant recall must still reject a different-workload entry even with every confidence gate wide open "+
			"(got %s @ %.2f) — a near-miss is a lead, recall is an answer", entry.Path, conf)
	}
}

// TestNearMissHonoursRequireWorkloadMatch proves the looser tier is not forced on an
// operator who has explicitly demanded exact structural agreement.
func TestNearMissHonoursRequireWorkloadMatch(t *testing.T) {
	r := &Recall{
		MinScore: 4.0, MarginGap: 2.0, SoloFloor: 4.0,
		RequireWorkloadMatch: true,
		Catalog:              harborIAMCatalog(),
	}
	if nm := r.nearMiss(context.Background(), harborReq()); nm != nil {
		t.Fatalf("require_workload_match: true must suppress the loosened near-miss tier, got %s", nm.Path)
	}
}

// TestNearMissStopsAtTheNamespaceBoundary proves the new tier is scoped, not a
// free-for-all: a hint from an unrelated namespace is noise, not a lead.
func TestNearMissStopsAtTheNamespaceBoundary(t *testing.T) {
	r := &Recall{
		MinScore: 4.0, MarginGap: 2.0, SoloFloor: 4.0,
		Catalog: fakeScored{hits: []catalog.ScoredEntry{{
			Entry: catalog.Entry{
				Title:    "Some unrelated incident in another namespace",
				Path:     "other.md",
				Resource: "observability/vmagent",
				Body:     "## Cause\nUnrelated.\n",
			},
			Score: 3.0,
		}}},
	}
	if nm := r.nearMiss(context.Background(), harborReq()); nm != nil {
		t.Fatalf("near-miss must not cross the namespace boundary, got %s", nm.Path)
	}
}

// TestNearMissAgreesLadder pins the predicate itself, tier by tier, so a future edit to
// resourceAgrees cannot silently change what a lead is allowed to be.
func TestNearMissAgreesLadder(t *testing.T) {
	w := providers.Workload{Namespace: "tooling", Name: "harbor"}
	cases := []struct {
		name          string
		entryResource string
		require       bool
		want          matchStrength
	}{
		{"exact workload", "tooling/harbor", false, matchExact},
		{"bare namespace entry", "tooling", false, matchNamespace},
		{"same ns, different workload — the new tier", "tooling/harbor-registry", false, matchNamespace},
		{"same ns, different workload, require_workload_match", "tooling/harbor-registry", true, matchNone},
		{"different namespace", "observability/vmagent", false, matchNone},
		{"resource-less entry, scoped request", "", false, matchNone},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := nearMissAgrees(w, tc.entryResource, tc.require); got != tc.want {
				t.Fatalf("nearMissAgrees(%q, require=%v) = %v, want %v", tc.entryResource, tc.require, got, tc.want)
			}
		})
	}
}

// TestResourceAgreesUnchangedForDifferentWorkload pins the OTHER half of the contract:
// the strict predicate must be untouched by this change.
func TestResourceAgreesUnchangedForDifferentWorkload(t *testing.T) {
	w := providers.Workload{Namespace: "tooling", Name: "harbor"}
	if got := resourceAgrees(w, "tooling/harbor-registry", false); got != matchNone {
		t.Fatalf("resourceAgrees must keep rejecting two distinct named workloads (it guards instant recall), got %v", got)
	}
}
