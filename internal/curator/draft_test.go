package curator

import (
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/kbvalidate"
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

// TestDraftKBEntryCapsLongTitle proves a free-form investigation title that runs
// past kbvalidate's 120-byte single-line merge gate is capped by construction:
// single line, ≤120 bytes, ellipsis-terminated, and structurally valid.
func TestDraftKBEntryCapsLongTitle(t *testing.T) {
	long := "Harbor registry is completely down across every environment because the IAM AccessKeysPerUser quota was exceeded and the credential secret could not be provisioned"
	if len(long) <= 120 {
		t.Fatalf("test fixture must exceed 120 bytes, got %d", len(long))
	}
	inv := providers.Investigation{
		Title:      long,
		Confidence: 0.9,
		Resource:   providers.Workload{Namespace: "tooling", Name: "harbor-core"},
		RootCauses: []providers.Hypothesis{{Summary: "IAM quota exceeded", Evidence: []string{"e"}, SuggestedAction: "delete an old key"}},
	}
	e := draftKBEntry(inv)
	if got := len(e.Title); got > 120 {
		t.Fatalf("title byte length = %d, want <= 120: %q", got, e.Title)
	}
	if strings.ContainsAny(e.Title, "\r\n") {
		t.Fatalf("title must be a single line: %q", e.Title)
	}
	if !strings.HasSuffix(e.Title, "…") {
		t.Fatalf("capped title should end with an ellipsis: %q", e.Title)
	}
	if !utf8.ValidString(e.Title) {
		t.Fatalf("capped title must remain valid UTF-8: %q", e.Title)
	}
	// The drafted entry must clear the title field of the structural merge gate.
	for _, iss := range kbvalidate.ValidateStructural(toCatalogEntry(e)) {
		if iss.Field == "title" && iss.Severity == kbvalidate.SeverityError {
			t.Fatalf("drafted title failed the merge gate: %s", iss.Message)
		}
	}
}

// TestDraftKBEntryCollapsesNewlinesInTitle proves an embedded newline (which the
// validator rejects as a hard error) is collapsed to a single line.
func TestDraftKBEntryCollapsesNewlinesInTitle(t *testing.T) {
	inv := providers.Investigation{
		Title:      "Harbor down\nsecond line\ttabbed",
		RootCauses: []providers.Hypothesis{{Summary: "s"}},
	}
	e := draftKBEntry(inv)
	if strings.ContainsAny(e.Title, "\r\n\t") {
		t.Fatalf("title must be collapsed to a single line: %q", e.Title)
	}
	if e.Title != "Harbor down second line tabbed" {
		t.Fatalf("title = %q, want collapsed single-space form", e.Title)
	}
}

// TestDraftKBEntryShortTitleUnchanged proves a within-limit title is passed
// through untouched.
func TestDraftKBEntryShortTitleUnchanged(t *testing.T) {
	inv := providers.Investigation{
		Title:      "HarborRegistryDown",
		RootCauses: []providers.Hypothesis{{Summary: "s"}},
	}
	if e := draftKBEntry(inv); e.Title != "HarborRegistryDown" {
		t.Fatalf("short title = %q, want it unchanged", e.Title)
	}
}

// TestDraftKBEntryMultibyteTitleNotCorrupted proves capping a title full of
// multibyte runes never cuts mid-rune (the result stays valid UTF-8 and ≤120
// bytes).
func TestDraftKBEntryMultibyteTitleNotCorrupted(t *testing.T) {
	// Each "é" is 2 bytes; a long run of them pushes well past 120 bytes.
	long := strings.Repeat("café éclair ", 20)
	inv := providers.Investigation{
		Title:      long,
		RootCauses: []providers.Hypothesis{{Summary: "s"}},
	}
	e := draftKBEntry(inv)
	if len(e.Title) > 120 {
		t.Fatalf("title byte length = %d, want <= 120", len(e.Title))
	}
	if !utf8.ValidString(e.Title) {
		t.Fatalf("capped multibyte title must remain valid UTF-8: %q", e.Title)
	}
}

// toCatalogEntry mirrors a drafted KBEntry into the catalog.Entry shape that
// kbvalidate.ValidateStructural consumes, so tests can run the real merge gate.
func toCatalogEntry(e providers.KBEntry) catalog.Entry {
	return catalog.Entry{
		Type:        e.Type,
		Title:       e.Title,
		Description: e.Description,
		Resource:    e.Resource,
		Tags:        e.Tags,
		Body:        e.Body,
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
