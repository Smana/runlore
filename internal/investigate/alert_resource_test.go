// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// The live Harbor incident.
//
// The alert fires on the HelmRelease `tooling/harbor`. The investigation correctly
// refines that to the pod `tooling/harbor-registry`, where the fault actually is, and
// the entry is filed under the pod. Every LATER firing of that same alert then arrives
// carrying `tooling/harbor` — and misses the entry it produced (no_resource_match),
// paying for a full investigation beside a catalog that holds the answer.
//
// alert_resource is the alert-side index that closes the loop.
func harborEntryFiledUnderThePod(alertResource string) catalog.Entry {
	return catalog.Entry{
		Title:         "Harbor Registry Down due to IAM Access Key Quota Limit",
		Path:          "iam.md",
		Resource:      "tooling/harbor-registry", // where the fault IS
		AlertResource: alertResource,             // where the ALERT fired
		Body:          "## Cause\nAccessKey/xplane-harbor hit AccessKeysPerUser: 2.\n\n## Resolution\nDelete an unused IAM access key.\n",
	}
}

func harborAlertReq() Request {
	return Request{
		Title:       "HelmRelease/harbor InstallFailed",
		Fingerprint: "fp-ar",
		Workload:    providers.Workload{Namespace: "tooling", Name: "harbor"},
		Labels:      map[string]string{"alertname": "HelmReleaseInstallFailed"},
	}
}

// TestRecallReachesEntryViaAlertResource is the regression test: with alert_resource
// recorded, the alert that PRODUCED the entry can now recall it.
func TestRecallReachesEntryViaAlertResource(t *testing.T) {
	r := &Recall{
		MinScore: 1.0, MarginGap: 0.5, SoloFloor: 1.0,
		Catalog: fakeScored{hits: []catalog.ScoredEntry{{
			Entry: harborEntryFiledUnderThePod("tooling/harbor"),
			Score: 9.0,
		}}},
	}
	entry, _ := r.lookup(context.Background(), harborAlertReq())
	if entry == nil {
		t.Fatal("an entry carrying alert_resource: tooling/harbor must be reachable from an alert on tooling/harbor, " +
			"even though its affected resource is the deeper tooling/harbor-registry")
	}
	if entry.Path != "iam.md" {
		t.Fatalf("wrong entry: %s", entry.Path)
	}
}

// TestRecallMissesEntryWithoutAlertResource pins the BUG this fixes — the same entry,
// with the field absent (every entry curated before this change), stays unreachable.
// If this ever starts passing, the structural gate has been loosened somewhere else and
// the fix is no longer doing the work.
func TestRecallMissesEntryWithoutAlertResource(t *testing.T) {
	r := &Recall{
		MinScore: 0.1, MarginGap: 0.0, SoloFloor: 0.1, // wide open: only the STRUCTURAL gate can stop it
		Catalog: fakeScored{hits: []catalog.ScoredEntry{{
			Entry: harborEntryFiledUnderThePod(""), // pre-existing entry: no alert_resource
			Score: 9.0,
		}}},
	}
	if entry, _ := r.lookup(context.Background(), harborAlertReq()); entry != nil {
		t.Fatalf("without alert_resource the entry must remain structurally unreachable from tooling/harbor (got %s) — "+
			"this is the bug alert_resource exists to fix, and the old behaviour must be preserved for entries that lack it", entry.Path)
	}
}

// TestAlertResourceIsAdditiveNotAReplacement proves the entry stays reachable from its
// OWN resource too. Indexing by the alert must not cost us the fault-locus index: an
// alert firing directly on the pod (KubePodNotReady) must still hit the same entry.
func TestAlertResourceIsAdditiveNotAReplacement(t *testing.T) {
	r := &Recall{
		MinScore: 1.0, MarginGap: 0.5, SoloFloor: 1.0,
		Catalog: fakeScored{hits: []catalog.ScoredEntry{{
			Entry: harborEntryFiledUnderThePod("tooling/harbor"),
			Score: 9.0,
		}}},
	}
	podAlert := Request{
		Title:       "KubePodNotReady",
		Fingerprint: "fp-pod",
		Workload:    providers.Workload{Namespace: "tooling", Name: "harbor-registry-59598dbd57-ltkzw"},
		Labels:      map[string]string{"alertname": "KubePodNotReady"},
	}
	entry, _ := r.lookup(context.Background(), podAlert)
	if entry == nil {
		t.Fatal("alert_resource must be ADDITIVE: the entry must still be reachable from its own affected resource " +
			"(pod-hash normalised), not only from the alert resource")
	}
}

// TestEntryAgreesKeepsTheStrongerTier pins the predicate: when both resources agree at
// different strengths, the stronger wins — a weaker alert-side match must never dilute
// a stronger fault-side one (the tier feeds the confidence gate).
func TestEntryAgreesKeepsTheStrongerTier(t *testing.T) {
	w := providers.Workload{Namespace: "tooling", Name: "harbor-registry"}
	e := catalog.Entry{
		Resource:      "tooling/harbor-registry", // exact
		AlertResource: "tooling",                 // bare namespace → weaker
	}
	if got := entryAgrees(w, e, false); got != matchExact {
		t.Fatalf("entryAgrees must keep the STRONGER tier (exact), got %v", got)
	}
}

// TestEntryAgreesUnchangedWhenNoAlertResource proves the predicate is a strict superset:
// with AlertResource empty it must be byte-identical to resourceAgrees.
func TestEntryAgreesUnchangedWhenNoAlertResource(t *testing.T) {
	w := providers.Workload{Namespace: "tooling", Name: "harbor"}
	for _, res := range []string{"tooling/harbor", "tooling", "tooling/harbor-registry", "observability/vmagent", ""} {
		e := catalog.Entry{Resource: res}
		if got, want := entryAgrees(w, e, false), resourceAgrees(w, res, false); got != want {
			t.Fatalf("entryAgrees(resource=%q, no alert_resource) = %v, want resourceAgrees = %v", res, got, want)
		}
	}
}
