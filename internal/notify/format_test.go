package notify

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func sampleInvestigation() providers.Investigation {
	return providers.Investigation{
		Confidence: 0.82,
		Resource:   providers.Workload{Kind: "HelmRelease", Namespace: "tooling", Name: "harbor"},
		RootCauses: []providers.Hypothesis{{
			Summary:    "chart 1.15 enabled DB migrations; harbor-db CrashLoopBackOff",
			Confidence: 0.82, Evidence: []string{"pg_up=0", "migration lock timeout"},
			ChangeRef:       "flux-system/apps: abc123..def456",
			SuggestedAction: "flux rollback hr/harbor", Reversible: true,
		}},
		Unresolved: []string{"why the migration lock never released"},
	}
}

func TestFormat(t *testing.T) {
	out := Format(sampleInvestigation())
	for _, want := range []string{"82%", "chart 1.15", "pg_up=0", "flux rollback hr/harbor", "reversible", "why the migration lock"} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatted message missing %q:\n%s", want, out)
		}
	}
}

// TestFormatResourceAndChange asserts the shared message names the affected
// resource (which workload is this about?) and each root cause's change ref
// (what changed?) — the two anchors an on-call reads first. Both are omitted
// when unknown so the message never prints empty labels.
func TestFormatResourceAndChange(t *testing.T) {
	out := Format(sampleInvestigation())
	if !strings.Contains(out, "HelmRelease tooling/harbor") {
		t.Fatalf("formatted message missing the affected resource:\n%s", out)
	}
	if !strings.Contains(out, "flux-system/apps: abc123..def456") {
		t.Fatalf("formatted message missing the root cause's change ref:\n%s", out)
	}

	empty := Format(providers.Investigation{RootCauses: []providers.Hypothesis{{Summary: "x"}}})
	if strings.Contains(empty, "Resource:") || strings.Contains(empty, "What changed:") {
		t.Fatalf("empty resource/change must not render labels:\n%s", empty)
	}
}

// TestFormatProgress covers the shared interim status line: title, step counter,
// name-sorted tools-used summary, and interim text. Empty fields are omitted.
func TestFormatProgress(t *testing.T) {
	out := FormatProgress(providers.ProgressUpdate{
		Title:     "HarborDown",
		Step:      5,
		MaxSteps:  20,
		ToolsUsed: map[string]int{"what_changed": 2, "kb_search": 1},
		Interim:   "narrowed to harbor-db",
	})
	for _, want := range []string{"HarborDown", "step 5/20", "kb_search×1", "what_changed×2", "narrowed to harbor-db"} {
		if !strings.Contains(out, want) {
			t.Fatalf("progress line missing %q:\n%s", want, out)
		}
	}
	// Sorted order: kb_search before what_changed.
	if strings.Index(out, "kb_search") > strings.Index(out, "what_changed") {
		t.Fatalf("tools-used not name-sorted:\n%s", out)
	}
	// No tools + no interim ⇒ just the header line, no "Tools used:" label.
	bare := FormatProgress(providers.ProgressUpdate{Title: "x", Step: 1, MaxSteps: 20})
	if strings.Contains(bare, "Tools used:") {
		t.Fatalf("empty tools map must omit the label:\n%s", bare)
	}
}

// TestFormatUsageFooter covers the one-line cost footer: token summary always,
// dollar figure only when priced, and omission when no model call was made.
func TestFormatUsageFooter(t *testing.T) {
	inv := sampleInvestigation()

	// Priced: token line + dollar figure.
	inv.Usage = providers.UsageTotals{ModelCalls: 4, InputTokens: 10000, OutputTokens: 500, CachedInputTokens: 2500, CostUSD: 0.14, Priced: true}
	out := Format(inv)
	for _, want := range []string{"4 model calls", "10000 in / 500 out tokens", "(25% cached)", "~$0.14"} {
		if !strings.Contains(out, want) {
			t.Fatalf("priced footer missing %q:\n%s", want, out)
		}
	}

	// Unpriced: token line, no dollar figure.
	inv.Usage = providers.UsageTotals{ModelCalls: 4, InputTokens: 10000, OutputTokens: 500, CachedInputTokens: 2500}
	out = Format(inv)
	if !strings.Contains(out, "4 model calls") {
		t.Fatalf("unpriced footer must still show the token summary:\n%s", out)
	}
	if strings.Contains(out, "$") {
		t.Fatalf("unpriced footer must not show a dollar figure:\n%s", out)
	}

	// No model calls (pure recall): no footer at all.
	inv.Usage = providers.UsageTotals{}
	out = Format(inv)
	if strings.Contains(out, "model calls") {
		t.Fatalf("a zero-usage investigation must omit the footer:\n%s", out)
	}
}
