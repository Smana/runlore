// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeClosedPRs is a fixed ClosedPRLister for suppression tests.
type fakeClosedPRs struct{ prs []providers.CuratedIssue }

func (f fakeClosedPRs) ListClosedUnmergedPRsByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return f.prs, nil
}

// body builds a PR body carrying the hidden fingerprint marker, matching what the
// curator stamps at draft time (providers.FingerprintMarker).
func body(fp string) string { return "Drafted by RunLore.\n\n" + providers.FingerprintMarker(fp) }

func TestClosedPRSuppressionRecordsFingerprint(t *testing.T) {
	s := ClosedPRSuppression{Forge: fakeClosedPRs{prs: []providers.CuratedIssue{
		{Number: 7, Title: "KB: apps/web DNS", Body: body("fp-web"), Labels: []string{"runlore"}},
	}}}
	got, err := s.Suppressed(context.Background())
	if err != nil {
		t.Fatalf("Suppressed: %v", err)
	}
	se, ok := got["fp-web"]
	if !ok {
		t.Fatalf("fp-web not recorded, got %+v", got)
	}
	if se.PRNumber != 7 || se.Reason != "" {
		t.Fatalf("entry = %+v, want PRNumber 7, empty reason", se)
	}
}

func TestClosedPRSuppressionCapturesRejectReason(t *testing.T) {
	s := ClosedPRSuppression{Forge: fakeClosedPRs{prs: []providers.CuratedIssue{
		{Number: 9, Body: body("fp-x"), Labels: []string{"runlore", "not-kb-worthy"}},
	}}}
	got, _ := s.Suppressed(context.Background())
	if got["fp-x"].Reason != "not-kb-worthy" {
		t.Fatalf("reason = %q, want not-kb-worthy", got["fp-x"].Reason)
	}
}

func TestClosedPRSuppressionExcludesNeedsWork(t *testing.T) {
	// needs-work is "revise & resubmit", NOT a deliberate rejection: it must not be
	// suppressed (so it is never escalated as a closed-unmerged reconsideration).
	s := ClosedPRSuppression{Forge: fakeClosedPRs{prs: []providers.CuratedIssue{
		{Number: 3, Body: body("fp-revise"), Labels: []string{"runlore", "needs-work"}},
	}}}
	got, _ := s.Suppressed(context.Background())
	if _, ok := got["fp-revise"]; ok {
		t.Fatalf("needs-work must not be suppressed, got %+v", got)
	}
}

func TestClosedPRSuppressionSkipsMarkerlessPR(t *testing.T) {
	// No fingerprint marker → nothing stable to key the suppression on; skip it.
	s := ClosedPRSuppression{Forge: fakeClosedPRs{prs: []providers.CuratedIssue{
		{Number: 5, Title: "KB: legacy", Body: "hand-filed, no marker", Labels: []string{"runlore"}},
	}}}
	got, _ := s.Suppressed(context.Background())
	if len(got) != 0 {
		t.Fatalf("markerless PR must be skipped, got %+v", got)
	}
}

func TestClosedPRSuppressionKeepsMostRecentClose(t *testing.T) {
	// Two closes for one fingerprint: the most recent (highest-numbered) wins.
	s := ClosedPRSuppression{Forge: fakeClosedPRs{prs: []providers.CuratedIssue{
		{Number: 4, Body: body("fp-dup"), Labels: []string{"runlore"}},
		{Number: 11, Body: body("fp-dup"), Labels: []string{"runlore", "wontfix"}},
	}}}
	got, _ := s.Suppressed(context.Background())
	if got["fp-dup"].PRNumber != 11 || got["fp-dup"].Reason != "wontfix" {
		t.Fatalf("entry = %+v, want PR 11 wontfix", got["fp-dup"])
	}
}

func TestSuppressClosesRedraftOfRejectedEntry(t *testing.T) {
	// A human closed PR #7 (fp-web) without merging; the incident recurred and the
	// curator re-drafted it as PR #12. The sweep must comment-then-close #12.
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 12, Title: "KB: apps/web DNS", Body: body("fp-web"), Labels: []string{"runlore"}},
		{Number: 13, Title: "KB: unrelated", Body: body("fp-other"), Labels: []string{"runlore"}},
	}}
	src := ClosedPRSuppression{Forge: fakeClosedPRs{prs: []providers.CuratedIssue{
		{Number: 7, Body: body("fp-web"), Labels: []string{"runlore", "wontfix"}},
	}}}
	s := Suppress{Forge: f, Source: src, Log: discardLog()}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 1 || f.closed[0] != 12 {
		t.Fatalf("want close [12] only, got %v", f.closed)
	}
	if len(f.commented) != 1 || f.commented[0] != 12 {
		t.Fatalf("want a back-ref comment on 12 before closing, got %v", f.commented)
	}
}

func TestSuppressNeverTouchesProtectedOrMarkerless(t *testing.T) {
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 20, Body: body("fp-web"), Labels: []string{"runlore", "investigating"}}, // human-touched
		{Number: 21, Body: "no marker here", Labels: []string{"runlore"}},                // legacy/hand-filed
	}}
	src := ClosedPRSuppression{Forge: fakeClosedPRs{prs: []providers.CuratedIssue{
		{Number: 7, Body: body("fp-web"), Labels: []string{"runlore"}},
	}}}
	s := Suppress{Forge: f, Source: src, Log: discardLog()}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 0 || len(f.commented) != 0 {
		t.Fatalf("protected/markerless PRs must be untouched: closed=%v commented=%v", f.closed, f.commented)
	}
}

func TestSuppressRespectsNeedsWorkAsRevise(t *testing.T) {
	// needs-work is accept-with-changes (suppression.go suppressReviseLabels): a
	// re-draft after such a close is the RESUBMIT — it must stay open.
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 30, Body: body("fp-y"), Labels: []string{"runlore"}},
	}}
	src := ClosedPRSuppression{Forge: fakeClosedPRs{prs: []providers.CuratedIssue{
		{Number: 8, Body: body("fp-y"), Labels: []string{"runlore", "needs-work"}},
	}}}
	s := Suppress{Forge: f, Source: src, Log: discardLog()}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 0 {
		t.Fatalf("a needs-work resubmit must not be suppressed, got closed=%v", f.closed)
	}
}
