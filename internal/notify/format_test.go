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

// TestFormatRecall asserts a recall renders the explicit "⚡ Instant recall"
// block (known answer + resolve rate + entry) regardless of the occurrence counter,
// and never the "Seen before" fresh-recurrence framing.
func TestFormatRecall(t *testing.T) {
	inv := providers.Investigation{
		Title: "HarborRegistryDown", Confidence: 0.6,
		Recalled: true, RecalledEntry: "harbor-registry-down.md",
		Occurrences: 1, // fresh trigger key
		Prior: &providers.PriorKnowledge{
			Cause: "AccessKey hit IAM quota", Resolution: "delete an unused access key",
			EntryPath: "harbor-registry-down.md", Recalls: 3, Resolved: 3,
		},
	}
	out := Format(inv)
	for _, want := range []string{
		"⚡ Instant recall — answered from your knowledge base, no investigation was run",
		"Known cause: AccessKey hit IAM quota",
		"Validated resolution: delete an unused access key",
		"Resolve rate: 3/3 recalls resolved",
		"Knowledge-base entry: harbor-registry-down.md",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Format recall missing %q\n---\n%s", want, out)
		}
	}
	if strings.Contains(out, "Seen before") {
		t.Errorf("recall must not use the Seen-before framing\n%s", out)
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

// TestFormatTitleOnce proves the CLI/Matrix text names the incident (a gap the
// Slack card's header always covered but Format never did), and only once.
func TestFormatTitleOnce(t *testing.T) {
	out := Format(providers.Investigation{Title: "HarborRegistryDown", Confidence: 0.5})
	if !strings.Contains(out, "🔍 HarborRegistryDown") {
		t.Fatalf("expected the title on its own header-style line:\n%s", out)
	}
	if strings.Count(out, "HarborRegistryDown") != 1 {
		t.Fatalf("title must render exactly once, got %d:\n%s", strings.Count(out, "HarborRegistryDown"), out)
	}
}

// TestFormatConfidenceMatchesSlack proves Format and the Slack card headline
// the SAME confidence number for the same investigation (both now call
// confidenceBadge) — before this fix Format used the raw, un-maxed
// inv.Confidence while Slack maxed it against the top root cause, so a model
// that left the top-level field at 0 while ranking an 80%-confidence cause
// would show "0%" on the terminal and "80%" on Slack for the same incident.
func TestFormatConfidenceMatchesSlack(t *testing.T) {
	inv := providers.Investigation{
		Title:      "t",
		Confidence: 0,                                                       // model left top-level at 0 …
		RootCauses: []providers.Hypothesis{{Summary: "x", Confidence: 0.8}}, // … but the root cause is 80%
	}
	out := Format(inv)
	if !strings.Contains(out, "confidence · 80%") {
		t.Fatalf("Format must headline the maxed confidence (80%%), matching Slack:\n%s", out)
	}
	if strings.Contains(out, "confidence · 0%") {
		t.Fatalf("Format must not show the raw, un-maxed 0%%:\n%s", out)
	}
}

// TestUnstatedConfidenceIsNotLowConfidence pins the distinction between "the model
// judged this very unlikely" and "the model judged nothing".
//
// `confidence` used to be optional everywhere in the findings schema, so a model that
// omitted it on the top level AND every root cause produced a delivered 0 — and the
// verify pass, which may only LOWER, pinned it there. The card then read "🔴 Low
// confidence · 0%" on an investigation that named a concrete cause, quoted metrics for
// it, and passed the adversarial review. Observed live on NodeSystemSaturation. A red
// 0% badge on sound work teaches the on-call to ignore the number entirely.
func TestUnstatedConfidenceIsNotLowConfidence(t *testing.T) {
	unstated := providers.Investigation{
		Title:      "NodeSystemSaturation",
		RootCauses: []providers.Hypothesis{{Summary: "workspace pod consuming 7 of 8 cores"}},
		Verified:   true,
	}
	out := Format(unstated)
	if strings.Contains(out, "0%") {
		t.Fatalf("an unstated confidence must not be rendered as a number:\n%s", out)
	}
	if !strings.Contains(out, "confidence not stated") {
		t.Fatalf("an unstated confidence must say so:\n%s", out)
	}
	if strings.Contains(out, "Low confidence") {
		t.Fatalf("absent is not low — the two must not render alike:\n%s", out)
	}

	// Guard against over-correcting: a genuine low confidence still reads as Low.
	low := providers.Investigation{
		Title:      "NodeSystemSaturation",
		RootCauses: []providers.Hypothesis{{Summary: "maybe", Confidence: 0.1}},
	}
	if out := Format(low); !strings.Contains(out, "Low confidence · 10%") {
		t.Fatalf("a stated low confidence must still render as Low:\n%s", out)
	}
}

// TestFormatMetadataBelowAnswer proves the reordering: trigger-time metadata
// (Resource/alert facts/Started) now renders AFTER the root causes, mirroring
// the Slack card's move — orienting detail is not the thing to lead with.
func TestFormatMetadataBelowAnswer(t *testing.T) {
	inv := providers.Investigation{
		Title:      "t",
		Confidence: 0.8,
		Resource:   providers.Workload{Kind: "Pod", Namespace: "ns", Name: "p"},
		StartedAt:  time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC),
		RootCauses: []providers.Hypothesis{{Summary: "the root cause", Confidence: 0.8}},
	}
	out := Format(inv)
	whyIdx := strings.Index(out, "the root cause")
	resourceIdx := strings.Index(out, "Resource:")
	if whyIdx == -1 || resourceIdx == -1 || resourceIdx < whyIdx {
		t.Fatalf("Resource (idx %d) must come AFTER the root cause (idx %d):\n%s", resourceIdx, whyIdx, out)
	}
}

// TestFormatRecallConfidenceDisagreement mirrors
// TestSlackRecallConfidenceDisagreement: the CLI/Matrix text must disclose a
// recalled entry's own confidence when it diverges from the delivered one,
// exactly like Slack — the two notifiers must never diverge in what they claim.
func TestFormatRecallConfidenceDisagreement(t *testing.T) {
	inv := providers.Investigation{
		Title:      "HarborRegistryDown",
		Recalled:   true,
		Confidence: 0.78,
		RootCauses: []providers.Hypothesis{{Summary: "AccessKey hit IAM quota", Confidence: 0.95}},
	}
	out := Format(inv)
	if !strings.Contains(out, "78%") {
		t.Fatalf("delivered confidence (78%%) must headline:\n%s", out)
	}
	if !strings.Contains(out, "entry's own confidence 95%, adjusted to 78% by its track record") {
		t.Fatalf("must disclose the entry's own confidence, labeled:\n%s", out)
	}
}

// TestFormatVerifiedFooter proves the verify signal — visible on the Slack
// card's footer — is also surfaced in the shared text, so CLI/Matrix readers
// aren't missing information Slack users have.
func TestFormatVerifiedFooter(t *testing.T) {
	inv := providers.Investigation{Title: "t", Confidence: 0.8, Verified: true}
	if out := Format(inv); !strings.Contains(out, "✓ verified") {
		t.Fatalf("expected the verified marker:\n%s", out)
	}
	inv.Verified = false
	if out := Format(inv); strings.Contains(out, "✓ verified") {
		t.Fatalf("must omit the verified marker when Verified is false:\n%s", out)
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
