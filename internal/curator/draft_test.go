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
	if got := resourceString(providers.Workload{Namespace: "apps"}); got != "apps" {
		t.Fatalf("namespace-only resource = %q, want apps", got)
	}
	if got := resourceString(providers.Workload{}); got != "" {
		t.Fatalf("empty workload resource = %q, want empty", got)
	}
}
