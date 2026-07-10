// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func sampleInvestigation() providers.Investigation {
	return providers.Investigation{
		Confidence:  0.82,
		Verdict:     providers.VerdictActionRequired,
		Resource:    providers.Workload{Kind: "HelmRelease", Namespace: "tooling", Name: "harbor"},
		AlertName:   "HarborDown",
		Severity:    "critical",
		Environment: "prod",
		Cluster:     "eu-west-1",
		Tenant:      "platform",
		StartedAt:   time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC),
		RootCauses: []providers.Hypothesis{{
			Summary:    "chart 1.15 enabled DB migrations; harbor-db CrashLoopBackOff",
			Confidence: 0.82, Evidence: []string{"pg_up=0", "migration lock timeout"},
			ChangeRef:       "flux-system/apps: abc123..def456",
			SuggestedAction: "flux rollback hr/harbor", Reversible: true,
		}},
		Unresolved:     []string{"why the migration lock never released"},
		RuledOut:       []string{"network partition disproven by pg reachable from api pod"},
		DataGaps:       []string{"harbor-db disk metrics unavailable (scrape target down)"},
		Occurrences:    3,
		LastOccurrence: time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC),
		PrevCuratedURL: "https://kb.example/entry/prev",
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

// TestFormatVerdictMetadataRecurrence covers the enriched header: the model
// verdict badge, the compact alert-metadata line, incident start time, and the
// recurrence pointer to the previous investigation of the same incident.
func TestFormatVerdictMetadataRecurrence(t *testing.T) {
	out := Format(sampleInvestigation())
	for _, want := range []string{
		"Verdict: Action required",
		"Alert: HarborDown",
		"severity critical",
		"env prod",
		"cluster eu-west-1",
		"tenant platform",
		"Started: 2026-07-03T10:00:00Z",
		"📚 Seen before: ×3",
		"last investigated 2026-06-01T09:00:00Z",
		"Previous conclusion: https://kb.example/entry/prev",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatted message missing %q:\n%s", want, out)
		}
	}
}

// TestFormatPriorKnowledge covers the zero-click payoff: when the completion
// pipeline found the merged KB entry for a recurring incident (Prior), Format
// quotes the previous cause, human-reviewed resolution, and resolve rate
// inline alongside the seen-before counter and link.
func TestFormatPriorKnowledge(t *testing.T) {
	inv := providers.Investigation{
		Title: "t", Confidence: 0.8,
		Occurrences:    3,
		LastOccurrence: time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
		PrevCuratedURL: "https://kb/pr/12",
		Prior: &providers.PriorKnowledge{
			Cause: "ConfigMap truncated after kustomize bump", Resolution: "revert the patch and pin 5.3.2",
			Recalls: 3, Resolved: 3,
		},
	}
	out := Format(inv)
	for _, want := range []string{
		"📚 Seen before: ×3 — last investigated 2026-06-25T10:00:00Z",
		"Prior cause: ConfigMap truncated after kustomize bump",
		"Prior resolution: revert the patch and pin 5.3.2",
		"Resolve rate: 3/3 recalls resolved",
		"Previous conclusion: https://kb/pr/12",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Format missing %q\n---\n%s", want, out)
		}
	}
}

// TestFormatSeenBeforeWithoutPrior asserts that without Prior the block keeps
// today's counter+link shape (no empty labels).
func TestFormatSeenBeforeWithoutPrior(t *testing.T) {
	inv := providers.Investigation{
		Title: "t", Confidence: 0.8,
		Occurrences: 2, LastOccurrence: time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
		PrevCuratedURL: "https://kb/pr/12",
	}
	out := Format(inv)
	if !strings.Contains(out, "📚 Seen before: ×2") {
		t.Errorf("missing seen-before counter:\n%s", out)
	}
	for _, absent := range []string{"Prior cause:", "Prior resolution:", "Resolve rate:"} {
		if strings.Contains(out, absent) {
			t.Errorf("Format must omit %q when Prior is nil:\n%s", absent, out)
		}
	}
}

// TestFormatMatchedKnowledge covers the shared text used by Matrix + webhook: a full
// investigation whose kb_search matched a known runbook (MatchedKnowledge set, Prior
// nil) renders a visible "Matches known runbook" line with the path (or URL). It is
// suppressed when Prior is set (recurrence already covers it) and absent when unset.
func TestFormatMatchedKnowledge(t *testing.T) {
	// Path shown when no URL is derivable.
	out := Format(providers.Investigation{
		Title: "t", Confidence: 0.8,
		MatchedKnowledge: &providers.MatchedEntry{Title: "Harbor probe runbook", Path: "runbooks/harbor.md", Score: 6},
	})
	if !strings.Contains(out, "📚 Matches known runbook: Harbor probe runbook — runbooks/harbor.md") {
		t.Errorf("expected matched-runbook line with path:\n%s", out)
	}
	// URL preferred over path when present.
	outURL := Format(providers.Investigation{
		Title: "t", Confidence: 0.8,
		MatchedKnowledge: &providers.MatchedEntry{Title: "R", Path: "p.md", URL: "https://kb/p.md", Score: 6},
	})
	if !strings.Contains(outURL, "📚 Matches known runbook: R — https://kb/p.md") {
		t.Errorf("expected URL preferred over path:\n%s", outURL)
	}
	// Suppressed when Prior is set (don't double-render with Seen-before).
	outPrior := Format(providers.Investigation{
		Title: "t", Confidence: 0.8, Occurrences: 2,
		LastOccurrence:   time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
		Prior:            &providers.PriorKnowledge{Cause: "c"},
		MatchedKnowledge: &providers.MatchedEntry{Title: "R", Path: "p.md", Score: 6},
	})
	if strings.Contains(outPrior, "Matches known runbook") {
		t.Errorf("must suppress the matched-runbook line when Prior is set:\n%s", outPrior)
	}
	// Absent when unset.
	if strings.Contains(Format(providers.Investigation{Title: "t"}), "Matches known runbook") {
		t.Error("matched-runbook line must be absent when MatchedKnowledge is nil")
	}
}

// TestFormatRuledOutAndDataGaps asserts the two honest-limits sections render
// their bullets, mirroring the Unresolved section's shape.
func TestFormatRuledOutAndDataGaps(t *testing.T) {
	out := Format(sampleInvestigation())
	for _, want := range []string{
		"*Ruled out:*",
		"network partition disproven",
		"*Data gaps:*",
		"harbor-db disk metrics unavailable",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatted message missing %q:\n%s", want, out)
		}
	}
}

// TestFormatEnrichedOmissions proves every new element is dropped when its
// source field is empty/zero, so the message never prints an empty label.
func TestFormatEnrichedOmissions(t *testing.T) {
	// A bare investigation: no verdict, no metadata, no recurrence, no sections.
	bare := Format(providers.Investigation{RootCauses: []providers.Hypothesis{{Summary: "x"}}})
	for _, unwanted := range []string{
		"Verdict:", "Alert:", "severity ", "cluster ", "tenant ",
		"Started:", "📚 Seen before:", "Previous conclusion:", "*Ruled out:*", "*Data gaps:*",
	} {
		if strings.Contains(bare, unwanted) {
			t.Fatalf("bare investigation must not render %q:\n%s", unwanted, bare)
		}
	}

	// Occurrences ≤ 1 is a first sighting: no recurrence line.
	first := sampleInvestigation()
	first.Occurrences = 1
	if strings.Contains(Format(first), "📚 Seen before:") {
		t.Fatalf("first sighting (Occurrences=1) must omit the recurrence line:\n%s", Format(first))
	}
}

// TestFormatScaffoldingHasNoMrkdwnMeta guards the fallback-escape invariant: the
// slack fallback is escapeMrkdwn(Format(inv)), and TestSlackMessageFallbackEscaped
// relies on Format's own scaffolding carrying none of & < > (only user-injected
// evidence should). With a fully-populated investigation whose user strings are
// themselves free of those three chars, the WHOLE output must be free of them.
func TestFormatScaffoldingHasNoMrkdwnMeta(t *testing.T) {
	inv := sampleInvestigation()
	inv.Occurrences = 2
	inv.LastOccurrence = time.Now()
	inv.PrevCuratedURL = "https://kb/pr/1"
	inv.Prior = &providers.PriorKnowledge{Cause: "c", Resolution: "r", Recalls: 1, Resolved: 1}
	out := Format(inv)
	for _, ch := range []string{"&", "<", ">"} {
		if strings.Contains(out, ch) {
			t.Fatalf("Format scaffolding must not contain %q (breaks fallback escaping):\n%s", ch, out)
		}
	}
}

// TestVerdictBadge maps every verdict enum to a badge and leaves unknown/empty
// verdicts unrendered (empty label ⇒ Format prints no verdict line).
func TestVerdictBadge(t *testing.T) {
	for _, tc := range []struct {
		v     providers.Verdict
		label string
	}{
		{providers.VerdictNoAction, "No action needed"},
		{providers.VerdictActionSuggested, "Action suggested"},
		{providers.VerdictActionRequired, "Action required"},
		{providers.VerdictInconclusive, "Inconclusive"},
	} {
		emoji, label := verdictBadge(tc.v)
		if label != tc.label {
			t.Errorf("verdictBadge(%q) label = %q, want %q", tc.v, label, tc.label)
		}
		if emoji == "" {
			t.Errorf("verdictBadge(%q) emoji is empty", tc.v)
		}
	}
	if emoji, label := verdictBadge(""); emoji != "" || label != "" {
		t.Errorf(`verdictBadge("") = (%q,%q), want ("","")`, emoji, label)
	}
	if emoji, label := verdictBadge("bogus"); emoji != "" || label != "" {
		t.Errorf(`verdictBadge("bogus") = (%q,%q), want ("","")`, emoji, label)
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
