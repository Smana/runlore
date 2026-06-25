package curator

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestDraftKBEntryHasDecisionCardAndSections(t *testing.T) {
	inv := providers.Investigation{
		Title:      "HarborRegistryDown",
		Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{
			Summary:         "IAM AccessKeysPerUser:2 quota → Secret missing username",
			Evidence:        []string{"accesskey/xplane-harbor failed", "CreateContainerConfigError"},
			SuggestedAction: "delete an old IAM access key", Reversible: false, ChangeRef: "crossplane/xplane-harbor",
		}},
		Unresolved: []string{"which key to delete"},
	}
	e := draftKBEntry(inv)
	if e.Type != "Incident" || e.Title != "HarborRegistryDown" {
		t.Fatalf("meta: %+v", e)
	}
	body := e.Body
	for _, want := range []string{
		"## Decision", "why keep", "confidence", // decision card
		"## Symptom", "## Investigate", "## Cause", "## Resolution", // OKF sections
		"IAM AccessKeysPerUser:2", "delete an old IAM access key", "crossplane/xplane-harbor",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestDraftKBEntryTypeIsDerived(t *testing.T) {
	// A resource-pinned finding is a point-in-time Incident. A finding with no
	// concrete resource ref but a reusable suggested action is generalized,
	// resource-pattern knowledge — a Playbook. (Postmortem is intentionally NOT a
	// type the curator emits: it is not in the validator vocabulary.)
	tests := []struct {
		name string
		inv  providers.Investigation
		want string
	}{
		{
			name: "resource-pinned incident",
			inv: providers.Investigation{
				Title:      "Harbor down",
				Resource:   providers.Workload{Namespace: "tooling", Name: "harbor-core"},
				RootCauses: []providers.Hypothesis{{Summary: "valkey down", SuggestedAction: "restart valkey"}},
			},
			want: "Incident",
		},
		{
			name: "resourceless actionable finding is a playbook",
			inv: providers.Investigation{
				Title:      "HelmRelease upgrade failures after a chart bump",
				RootCauses: []providers.Hypothesis{{Summary: "chart bump adds a failing migration job", SuggestedAction: "roll the chart back to the prior revision"}},
			},
			want: "Playbook",
		},
		{
			name: "resourceless WITHOUT an action stays incident",
			inv: providers.Investigation{
				Title:      "Something odd happened",
				RootCauses: []providers.Hypothesis{{Summary: "unclear cause"}},
			},
			want: "Incident",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := draftKBEntry(tt.inv).Type; got != tt.want {
				t.Fatalf("Type = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDraftKBEntrySetsResource(t *testing.T) {
	inv := providers.Investigation{
		Title: "Harbor down", Confidence: 0.9,
		Resource:   providers.Workload{Namespace: "tooling", Name: "harbor-core"},
		RootCauses: []providers.Hypothesis{{Summary: "valkey down", Confidence: 0.9}},
	}
	if e := draftKBEntry(inv); e.Resource != "tooling/harbor-core" {
		t.Fatalf("KBEntry.Resource = %q, want tooling/harbor-core", e.Resource)
	}
}

func TestResourceStringNamespaceOnly(t *testing.T) {
	if got := (providers.Workload{Namespace: "apps"}).Ref(); got != "apps" {
		t.Fatalf("namespace-only resource = %q, want apps", got)
	}
	if got := (providers.Workload{}).Ref(); got != "" {
		t.Fatalf("empty workload resource = %q, want empty", got)
	}
}

func TestDraftKBEntrySetsFingerprint(t *testing.T) {
	inv := providers.Investigation{
		Title:      "apps/web crash",
		Confidence: 0.9,
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "image tag rollout broke readiness", Evidence: []string{"e"}}},
	}
	if got := draftKBEntry(inv).Fingerprint; got != DupFingerprint(inv) {
		t.Fatalf("drafted entry fingerprint = %q, want %q", got, DupFingerprint(inv))
	}
}
