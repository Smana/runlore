// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// gapForge records the issues it was asked to open (title + full investigation) and
// counts Close calls. It embeds recordingForge (which supplies ListIssuesByLabel from
// its `issues` field) and overrides OpenIssue/Close.
type gapForge struct {
	*recordingForge
	openedTitles []string
	openedInvs   []providers.Investigation
	closes       int
}

func (g *gapForge) OpenIssue(_ context.Context, inv providers.Investigation) (providers.Ref, error) {
	g.openedTitles = append(g.openedTitles, inv.Title)
	g.openedInvs = append(g.openedInvs, inv)
	return providers.Ref{URL: "https://github.com/x/y/issues/1"}, nil
}

func (g *gapForge) Close(context.Context, int) error {
	g.closes++
	return nil
}

// fakeSuppression is a fixed SuppressionSource for recurrence escalation tests.
type fakeSuppression struct{ set map[string]SuppressedEntry }

func (f fakeSuppression) Suppressed(context.Context) (map[string]SuppressedEntry, error) {
	return f.set, nil
}

// suppressedEpisodes builds n unresolved episodes for a pattern all carrying the
// same DupFingerprint (a recurring, previously-rejected incident).
func suppressedEpisodes(pattern, fp string, n int) []outcome.Episode {
	eps := make([]outcome.Episode, n)
	for i := range eps {
		eps[i] = outcome.Episode{Resource: pattern, DupFingerprint: fp, Resolved: false}
	}
	return eps
}

func unresolved(pattern string, n int) []outcome.Episode {
	eps := make([]outcome.Episode, n)
	for i := range eps {
		eps[i] = outcome.Episode{Resource: pattern, Resolved: false}
	}
	return eps
}

func TestRecurrenceOpensGapIssueAtThreshold(t *testing.T) {
	eps := append(unresolved("apps/web", 3), outcome.Episode{Resource: "apps/worker", Resolved: false}) // worker: only 1
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: eps}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 1 || gf.openedTitles[0] != "knowledge-gap: apps/web" {
		t.Fatalf("want one gap issue for apps/web, got %v", gf.openedTitles)
	}
}

func TestRecurrenceBelowThresholdNoIssue(t *testing.T) {
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: unresolved("apps/web", 2)}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 0 {
		t.Fatalf("below threshold must open nothing, got %v", gf.openedTitles)
	}
}

func TestRecurrenceIdempotentWhenIssueExists(t *testing.T) {
	gf := &gapForge{recordingForge: &recordingForge{
		issues: []providers.CuratedIssue{{Title: "knowledge-gap: apps/web"}}, // already open
	}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: unresolved("apps/web", 5)}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 0 {
		t.Fatalf("an existing gap issue must prevent a duplicate, got %v", gf.openedTitles)
	}
}

func TestRecurrenceResolvedEpisodesDoNotCount(t *testing.T) {
	eps := []outcome.Episode{
		{Resource: "apps/web", Resolved: true}, {Resource: "apps/web", Resolved: true},
		{Resource: "apps/web", Resolved: false}, // only 1 unresolved
	}
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: eps}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 0 {
		t.Fatalf("resolved episodes must not count, got %v", gf.openedTitles)
	}
}

func TestRecurrenceEscalatesSuppressedAtThreshold(t *testing.T) {
	eps := suppressedEpisodes("apps/web", "fp-web", 3)
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{
		Forge:      gf,
		Ledger:     fakeLedger{eps: eps},
		Threshold:  3,
		Suppressed: fakeSuppression{set: map[string]SuppressedEntry{"fp-web": {Fingerprint: "fp-web", PRNumber: 7, Reason: "not-kb-worthy"}}},
		Log:        discardLog(),
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedInvs) != 1 {
		t.Fatalf("want exactly one escalation issue, got %d: %v", len(gf.openedInvs), gf.openedTitles)
	}
	inv := gf.openedInvs[0]
	if inv.Title != "knowledge-gap: apps/web" {
		t.Fatalf("title = %q", inv.Title)
	}
	summary := inv.RootCauses[0].Summary
	if !strings.Contains(summary, "#7") {
		t.Fatalf("escalation must link the closed PR #7: %q", summary)
	}
	if !strings.Contains(summary, "3") {
		t.Fatalf("escalation must cite the recurrence count: %q", summary)
	}
	if !strings.Contains(summary, "not-kb-worthy") {
		t.Fatalf("escalation should mention the close reason: %q", summary)
	}
	// Respect the human "no": we escalate via an issue, we do NOT reopen/close the PR.
	if gf.closes != 0 {
		t.Fatalf("suppressed escalation must not close/reopen the PR, got %d closes", gf.closes)
	}
}

func TestRecurrenceSuppressedBelowThresholdSilent(t *testing.T) {
	eps := suppressedEpisodes("apps/web", "fp-web", 2) // below threshold
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{
		Forge:      gf,
		Ledger:     fakeLedger{eps: eps},
		Threshold:  3,
		Suppressed: fakeSuppression{set: map[string]SuppressedEntry{"fp-web": {Fingerprint: "fp-web", PRNumber: 7}}},
		Log:        discardLog(),
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedInvs) != 0 {
		t.Fatalf("below threshold must escalate nothing, got %v", gf.openedTitles)
	}
}

func TestRecurrenceSuppressedNotDoubleCounted(t *testing.T) {
	// A suppressed fingerprint's episodes must not ALSO feed the generic pattern
	// count: exactly one (enriched) issue, never a plain gap issue on top.
	eps := suppressedEpisodes("apps/web", "fp-web", 4)
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{
		Forge:      gf,
		Ledger:     fakeLedger{eps: eps},
		Threshold:  3,
		Suppressed: fakeSuppression{set: map[string]SuppressedEntry{"fp-web": {Fingerprint: "fp-web", PRNumber: 7}}},
		Log:        discardLog(),
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedInvs) != 1 {
		t.Fatalf("want exactly one issue for a suppressed pattern, got %d: %v", len(gf.openedInvs), gf.openedTitles)
	}
}

func TestRecurrenceSuppressedIdempotent(t *testing.T) {
	eps := suppressedEpisodes("apps/web", "fp-web", 5)
	gf := &gapForge{recordingForge: &recordingForge{
		issues: []providers.CuratedIssue{{Title: "knowledge-gap: apps/web"}}, // already escalated
	}}
	r := Recurrence{
		Forge:      gf,
		Ledger:     fakeLedger{eps: eps},
		Threshold:  3,
		Suppressed: fakeSuppression{set: map[string]SuppressedEntry{"fp-web": {Fingerprint: "fp-web", PRNumber: 7}}},
		Log:        discardLog(),
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedInvs) != 0 {
		t.Fatalf("an existing gap issue must prevent a duplicate escalation, got %v", gf.openedTitles)
	}
}

func TestRecurrenceNonSuppressedUnaffectedBySuppressionSource(t *testing.T) {
	// A recurring pattern whose fingerprint is NOT suppressed still gets the plain
	// generic knowledge-gap issue even when a suppression source is wired.
	eps := suppressedEpisodes("apps/api", "fp-api", 3) // fp-api not in the suppression set
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{
		Forge:      gf,
		Ledger:     fakeLedger{eps: eps},
		Threshold:  3,
		Suppressed: fakeSuppression{set: map[string]SuppressedEntry{"fp-web": {PRNumber: 7}}},
		Log:        discardLog(),
	}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 1 || gf.openedTitles[0] != "knowledge-gap: apps/api" {
		t.Fatalf("non-suppressed pattern should still open a generic gap issue, got %v", gf.openedTitles)
	}
	if s := gf.openedInvs[0].RootCauses[0].Summary; strings.Contains(s, "#7") || strings.Contains(s, "closed") {
		t.Fatalf("generic issue must not reference a closed PR: %q", s)
	}
}

func TestRecurrencePatternFallsBackToTitle(t *testing.T) {
	eps := []outcome.Episode{
		{Title: "DNSFailure", Resolved: false}, {Title: "DNSFailure", Resolved: false}, {Title: "DNSFailure", Resolved: false},
	}
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Ledger: fakeLedger{eps: eps}, Threshold: 3, Log: discardLog()}
	if err := r.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(gf.openedTitles) != 1 || gf.openedTitles[0] != "knowledge-gap: DNSFailure" {
		t.Fatalf("a resource-less episode should group by title, got %v", gf.openedTitles)
	}
}
