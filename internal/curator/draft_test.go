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

// TestDraftKBEntryTagsCarryRecallSignal: tags feed the catalog's BM25+embedding
// corpus (catalog.entryText), so the drafted entry must tag the workload kind and
// namespace — not just the constant [runlore, <type>] pair, which ranks nothing.
func TestDraftKBEntryTagsCarryRecallSignal(t *testing.T) {
	inv := providers.Investigation{
		Title: "Harbor down", Confidence: 0.9,
		Resource:   providers.Workload{Kind: "Deployment", Namespace: "tooling", Name: "harbor-core"},
		RootCauses: []providers.Hypothesis{{Summary: "valkey down", Confidence: 0.9}},
	}
	tags := draftKBEntry(inv).Tags
	for _, want := range []string{"runlore", "incident", "deployment", "tooling"} {
		found := false
		for _, tag := range tags {
			if tag == want {
				found = true
			}
		}
		if !found {
			t.Fatalf("tags missing %q: %v", want, tags)
		}
	}

	// No resource → no empty/duplicate tags sneak in.
	tags = draftKBEntry(providers.Investigation{
		Title: "t", Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{Summary: "s", SuggestedAction: "a"}},
	}).Tags
	seen := map[string]bool{}
	for _, tag := range tags {
		if tag == "" {
			t.Fatalf("empty tag in %v", tags)
		}
		if seen[tag] {
			t.Fatalf("duplicate tag %q in %v", tag, tags)
		}
		seen[tag] = true
	}
}

// TestDraftKBEntrySymptomNamesResource: the Symptom section must carry more than
// a copy of the title — the affected workload (kind + ref) is both what a future
// reader checks first and lexical recall signal in the indexed body.
func TestDraftKBEntrySymptomNamesResource(t *testing.T) {
	inv := providers.Investigation{
		Title: "Harbor down", Confidence: 0.9,
		Resource:   providers.Workload{Kind: "Deployment", Namespace: "tooling", Name: "harbor-core"},
		RootCauses: []providers.Hypothesis{{Summary: "valkey down", Confidence: 0.9}},
	}
	body := draftKBEntry(inv).Body
	if !strings.Contains(body, "Deployment tooling/harbor-core") {
		t.Fatalf("Symptom must name the affected resource:\n%s", body)
	}
}

// TestDraftKBEntryCitations: change provenance belongs in an OKF Citations
// section at the entry bottom (SPEC §8) — numbered references, one per distinct
// ChangeRef — not only squeezed into the decision card bullet.
func TestDraftKBEntryCitations(t *testing.T) {
	inv := providers.Investigation{
		Title: "Harbor down", Confidence: 0.9, Verified: true,
		RootCauses: []providers.Hypothesis{
			{Summary: "quota", Evidence: []string{"e"}, ChangeRef: "crossplane/xplane-harbor"},
			{Summary: "other", Evidence: []string{"e2"}, ChangeRef: "flux/harbor-values"},
		},
	}
	body := draftKBEntry(inv).Body
	i := strings.Index(body, "## Citations")
	if i < 0 {
		t.Fatalf("body missing ## Citations:\n%s", body)
	}
	tail := body[i:]
	if !strings.Contains(tail, "[1] crossplane/xplane-harbor") || !strings.Contains(tail, "[2] flux/harbor-values") {
		t.Fatalf("citations must number the distinct change refs:\n%s", tail)
	}

	// No change refs → no empty Citations section.
	inv.RootCauses = []providers.Hypothesis{{Summary: "s", Evidence: []string{"e"}, SuggestedAction: "a"}}
	if body := draftKBEntry(inv).Body; strings.Contains(body, "## Citations") {
		t.Fatalf("no refs must mean no Citations section:\n%s", body)
	}
}

// TestDraftKBEntrySetsConfidenceAndProvenance: OKF guidance is to put the fields
// you want to query/filter on in frontmatter — confidence and change provenance
// are exactly that, so the drafted KBEntry must carry them structurally (the
// forge serializes them as extension frontmatter keys).
func TestDraftKBEntrySetsConfidenceAndProvenance(t *testing.T) {
	inv := providers.Investigation{
		Title: "Harbor down", Confidence: 0.9, Verified: true,
		RootCauses: []providers.Hypothesis{
			{Summary: "quota", Evidence: []string{"e"}, ChangeRef: "crossplane/xplane-harbor"},
			{Summary: "quota again", Evidence: []string{"e"}, ChangeRef: "crossplane/xplane-harbor"}, // dup ref collapses
		},
	}
	e := draftKBEntry(inv)
	if e.Confidence != 0.9 {
		t.Fatalf("Confidence = %v, want 0.9", e.Confidence)
	}
	if len(e.Provenance) != 1 || e.Provenance[0] != "crossplane/xplane-harbor" {
		t.Fatalf("Provenance = %v, want [crossplane/xplane-harbor]", e.Provenance)
	}
}

// TestDraftKBEntryExcludesCost proves the per-investigation cost/usage carrier
// never leaks into the curated KB body — cost is a delivery-time concern for
// humans, not durable knowledge.
func TestDraftKBEntryExcludesCost(t *testing.T) {
	inv := providers.Investigation{
		Title:      "HarborRegistryDown",
		Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{Summary: "db down", Evidence: []string{"pg_up=0"}}},
		Usage:      providers.UsageTotals{ModelCalls: 5, InputTokens: 42000, OutputTokens: 900, CostUSD: 0.37, Priced: true},
	}
	body := draftKBEntry(inv).Body
	for _, unwanted := range []string{"model calls", "$0.37", "tokens", "cached"} {
		if strings.Contains(body, unwanted) {
			t.Fatalf("KB body must not carry cost/usage text %q:\n%s", unwanted, body)
		}
	}
}
