// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestDraftRecordsAlertResourceWhenDistinct is the write half of the fix.
//
// The alert fires on the HelmRelease tooling/harbor. The investigation correctly
// refines that to the pod tooling/harbor-registry (preferDiscoveredResource lets the
// discovered resource win) and the entry is filed under the pod. Without recording the
// alert side, every later firing of that same alert arrives carrying tooling/harbor and
// misses the entry it produced.
func TestDraftRecordsAlertResourceWhenDistinct(t *testing.T) {
	inv := providers.Investigation{
		Title:         "Harbor Registry Down due to IAM Access Key Quota Limit",
		Confidence:    0.95,
		Resource:      providers.Workload{Namespace: "tooling", Name: "harbor-registry"}, // where the fault IS
		AlertResource: providers.Workload{Namespace: "tooling", Name: "harbor"},          // where the ALERT fired
		RootCauses:    []providers.Hypothesis{{Summary: "AccessKey hit AccessKeysPerUser: 2", Confidence: 0.95}},
	}
	e := draftKBEntry(inv)
	if e.Resource != "tooling/harbor-registry" {
		t.Fatalf("resource must stay the affected resource (the fault locus), got %q", e.Resource)
	}
	if e.AlertResource != "tooling/harbor" {
		t.Fatalf("alert_resource must record the resource the ALERT fired on, got %q", e.AlertResource)
	}
}

// TestDraftOmitsAlertResourceWhenSame proves it is not written as pure duplication: when
// the investigation did not refine the alert's workload, there is nothing to add.
func TestDraftOmitsAlertResourceWhenSame(t *testing.T) {
	w := providers.Workload{Namespace: "tooling", Name: "harbor"}
	e := draftKBEntry(providers.Investigation{
		Title:         "Harbor HelmRelease InstallFailed",
		Resource:      w,
		AlertResource: w,
		RootCauses:    []providers.Hypothesis{{Summary: "x", Confidence: 0.8}},
	})
	if e.AlertResource != "" {
		t.Fatalf("alert_resource must be omitted when identical to resource, got %q", e.AlertResource)
	}
}

// TestDraftOmitsAlertResourceWhenUnset covers non-alert sources (a GitOps failure, a
// manual run) that carry no alert workload at all.
func TestDraftOmitsAlertResourceWhenUnset(t *testing.T) {
	e := draftKBEntry(providers.Investigation{
		Title:      "Some GitOps failure",
		Resource:   providers.Workload{Namespace: "flux-system", Name: "apps"},
		RootCauses: []providers.Hypothesis{{Summary: "x", Confidence: 0.8}},
	})
	if e.AlertResource != "" {
		t.Fatalf("alert_resource must be empty when the source carried no alert workload, got %q", e.AlertResource)
	}
}

// TestDraftNormalizesAlertResourcePodHash proves the alert side gets the same CORE-681
// pod-hash normalisation as resource: a pod-scoped alert arrives with the volatile
// ReplicaSet suffix, which must never reach the frontmatter.
func TestDraftNormalizesAlertResourcePodHash(t *testing.T) {
	e := draftKBEntry(providers.Investigation{
		Title:         "Registry down",
		Resource:      providers.Workload{Namespace: "tooling", Name: "harbor"},
		AlertResource: providers.Workload{Namespace: "tooling", Name: "harbor-registry-59598dbd57-ltkzw"},
		RootCauses:    []providers.Hypothesis{{Summary: "x", Confidence: 0.8}},
	})
	if e.AlertResource != "tooling/harbor-registry" {
		t.Fatalf("alert_resource must be pod-hash normalised like resource, got %q", e.AlertResource)
	}
}
