// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// recordingForge records the label added per PR number. Shared by the resolution
// and recurrence tests.
type recordingForge struct {
	prs     []providers.CuratedIssue
	issues  []providers.CuratedIssue // returned by ListIssuesByLabel
	relabel map[int]string           // number -> added label
}

func (f *recordingForge) ListPRsByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return f.prs, nil
}
func (f *recordingForge) ListIssuesByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return f.issues, nil
}
func (f *recordingForge) Comment(context.Context, int, string) error { return nil }
func (f *recordingForge) ReplaceLabel(_ context.Context, n int, _, add string) error {
	if f.relabel == nil {
		f.relabel = map[int]string{}
	}
	f.relabel[n] = add
	return nil
}
func (f *recordingForge) Close(context.Context, int) error { return nil }
func (f *recordingForge) OpenIssue(context.Context, providers.Investigation) (providers.Ref, error) {
	return providers.Ref{}, nil
}

// fakeChecker reports a fixed resolution per PR number.
type fakeChecker struct{ resolved map[int]bool }

func (c fakeChecker) IsResolved(_ context.Context, pr providers.CuratedIssue) (bool, error) {
	return c.resolved[pr.Number], nil
}

// fakeLedger is a fixed Episodes() source for resolution/recurrence tests.
type fakeLedger struct{ eps []outcome.Episode }

func (f fakeLedger) Episodes() ([]outcome.Episode, error) { return f.eps, nil }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestResolutionQueuesResolvedOnly(t *testing.T) {
	f := &recordingForge{prs: []providers.CuratedIssue{
		{Number: 48, Title: "KB: HarborRegistryDown", Labels: []string{"runlore", "solved"}},
		{Number: 50, Title: "KB: StillBroken", Labels: []string{"runlore", "solved"}},
	}}
	q := Queue{Forge: f, Checker: fakeChecker{resolved: map[int]bool{48: true, 50: false}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := q.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.relabel[48] != "ready-to-merge" {
		t.Fatalf("resolved PR 48 should be ready-to-merge, got %q", f.relabel[48])
	}
	if _, queued := f.relabel[50]; queued {
		t.Fatalf("unresolved PR 50 must NOT be auto-queued (waits for human accepted)")
	}
}

func TestResolutionRespectsHumanAccepted(t *testing.T) {
	f := &recordingForge{prs: []providers.CuratedIssue{
		{Number: 48, Title: "KB: AcceptedButUnresolved", Labels: []string{"runlore", "solved", "accepted"}},
	}}
	q := Queue{Forge: f, Checker: fakeChecker{resolved: map[int]bool{48: false}},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := q.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.relabel[48] != "ready-to-merge" {
		t.Fatalf("human-accepted PR should be ready-to-merge even if unresolved, got %q", f.relabel[48])
	}
}

func TestLedgerResolutionCheckerResolvedTitle(t *testing.T) {
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{
		{Title: "HarborRegistryDown", Resolved: true},
		{Title: "Other", Resolved: false},
	}}}
	got, err := c.IsResolved(context.Background(), providers.CuratedIssue{Title: "KB: HarborRegistryDown"})
	if err != nil || !got {
		t.Fatalf("a resolved episode with the PR's title must be resolved=true; got %v err=%v", got, err)
	}
}

func TestLedgerResolutionCheckerUnresolved(t *testing.T) {
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{
		{Title: "HarborRegistryDown", Resolved: false}, // opened, never resolved
	}}}
	got, _ := c.IsResolved(context.Background(), providers.CuratedIssue{Title: "KB: HarborRegistryDown"})
	if got {
		t.Fatal("an unresolved episode must yield resolved=false")
	}
	// absent entirely → false
	if got, _ := c.IsResolved(context.Background(), providers.CuratedIssue{Title: "KB: Unknown"}); got {
		t.Fatal("a title with no episode must yield resolved=false")
	}
}

func TestLedgerResolutionCheckerEmptyTitle(t *testing.T) {
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{{Title: "", Resolved: true}}}}
	if got, _ := c.IsResolved(context.Background(), providers.CuratedIssue{Title: "KB: "}); got {
		t.Fatal("an empty title must never match (avoids resolving on a blank episode)")
	}
}

func TestLedgerResolutionRekeyedTitleDiffers(t *testing.T) {
	// The resolved episode shares the dedup FINGERPRINT but NOT the title — a reworded
	// re-investigation of the same incident must still resolve the matching PR.
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{
		{Title: "totally different prose this run", DupFingerprint: "dup-xyz", Resolved: true},
	}}}
	pr := providers.CuratedIssue{
		Title: "KB: HarborRegistryDown",
		Body:  "Drafted by RunLore\n\n" + providers.FingerprintMarker("dup-xyz"),
	}
	if got, err := c.IsResolved(context.Background(), pr); err != nil || !got {
		t.Fatalf("a reworded re-investigation sharing the fingerprint must resolve; got %v err=%v", got, err)
	}
	// A PR whose fingerprint matches no resolved episode must NOT resolve.
	other := providers.CuratedIssue{
		Title: "KB: HarborRegistryDown",
		Body:  "Drafted by RunLore\n\n" + providers.FingerprintMarker("dup-nomatch"),
	}
	if got, _ := c.IsResolved(context.Background(), other); got {
		t.Fatal("a non-matching fingerprint must NOT resolve")
	}
}

func TestLedgerResolutionLegacyTitleFallback(t *testing.T) {
	// A PR with NO marker (filed before R6) still resolves via the title join — now
	// whitespace-robust on both sides, so the raw-ledger-vs-trimmed-PR mismatch is gone.
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{
		{Title: "  HarborRegistryDown  ", Resolved: true}, // raw ledger title carries stray spaces
	}}}
	pr := providers.CuratedIssue{Title: "KB: HarborRegistryDown"} // no body marker
	if got, err := c.IsResolved(context.Background(), pr); err != nil || !got {
		t.Fatalf("a markerless PR must resolve via the whitespace-robust title join; got %v err=%v", got, err)
	}
}

func TestLedgerResolutionFingerprintMismatchNoTitleFallthrough(t *testing.T) {
	// A PR WITH a marker that matches no episode fingerprint, but shares a title, must
	// stay unresolved — no fall-through to the brittle title match (the R6 fragility).
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{
		{Title: "HarborRegistryDown", DupFingerprint: "dup-other", Resolved: true},
	}}}
	pr := providers.CuratedIssue{
		Title: "KB: HarborRegistryDown",
		Body:  "Drafted by RunLore\n\n" + providers.FingerprintMarker("dup-want"),
	}
	if got, _ := c.IsResolved(context.Background(), pr); got {
		t.Fatal("a fingerprint mismatch must not fall through to the brittle title match")
	}
}

func TestQueuePromotesResolvedViaLedger(t *testing.T) {
	f := &recordingForge{prs: []providers.CuratedIssue{
		{Number: 48, Title: "KB: HarborRegistryDown", Labels: []string{"runlore", "solved"}},
		{Number: 50, Title: "KB: StillBroken", Labels: []string{"runlore", "solved"}},
	}}
	led := fakeLedger{eps: []outcome.Episode{{Title: "HarborRegistryDown", Resolved: true}}}
	q := Queue{Forge: f, Checker: LedgerResolutionChecker{Ledger: led}, Log: discardLog()}
	if err := q.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.relabel[48] != "ready-to-merge" {
		t.Fatalf("the resolved PR #48 should be queued ready-to-merge, got %v", f.relabel)
	}
	if _, ok := f.relabel[50]; ok {
		t.Fatalf("the unresolved PR #50 must not be queued, got %v", f.relabel)
	}
}

const queueRepo = "acme/kb"

// queueForge lists PRs by their actual labels and keeps the labels it is told to change, so a second
// pass sees the first pass's result. The label calls are recorded as "number:remove>add".
type queueForge struct {
	recordingForge
	calls    []string
	failList string // ListPRsByLabel fails for this label
	failN    int    // ReplaceLabel fails for this PR
}

func (f *queueForge) ListPRsByLabel(_ context.Context, label string) ([]providers.CuratedIssue, error) {
	if label == f.failList {
		return nil, errors.New("list down")
	}
	var out []providers.CuratedIssue
	for _, pr := range f.prs {
		if slices.Contains(pr.Labels, label) {
			out = append(out, pr)
		}
	}
	return out, nil
}

func (f *queueForge) ReplaceLabel(_ context.Context, n int, remove, add string) error {
	if n == f.failN {
		return errors.New("label down")
	}
	f.calls = append(f.calls, fmt.Sprintf("%d:%s>%s", n, remove, add))
	for i := range f.prs {
		if f.prs[i].Number != n {
			continue
		}
		f.prs[i].Labels = slices.DeleteFunc(f.prs[i].Labels, func(l string) bool { return l == remove })
		if add != "" {
			f.prs[i].Labels = append(f.prs[i].Labels, add)
		}
	}
	return nil
}

// fakeEvidence is what the outcome ledger answers: the latest observation per fingerprint, and the
// PRs with a standing 👎 by KB link.
type fakeEvidence struct {
	verified map[string]bool
	urls     []string
}

func (e fakeEvidence) Verifications() map[string]bool { return e.verified }
func (e fakeEvidence) ContestedTriggers() []outcome.ContestedTrigger {
	var out []outcome.ContestedTrigger
	for _, u := range e.urls {
		out = append(out, outcome.ContestedTrigger{TriggerKey: "k", CuratedURL: u, Downs: 1})
	}
	return out
}

func prURL(n int) string { return fmt.Sprintf("https://github.com/%s/pull/%d", queueRepo, n) }

func TestQueueOnVerifiedEvidence(t *testing.T) {
	const (
		add      = "7:>ready-to-merge"
		withdraw = "7:ready-to-merge>"
		dup      = "## Possible duplicate\n\nflagged\n\n"
	)
	marked := providers.FingerprintMarker
	cases := []struct {
		name       string
		labels     []string
		body       string
		verified   map[string]bool
		contested  bool
		noEvidence bool
		resolved   bool
		wantCalls  []string
		wantReason string
		wantRisks  []Risk // nil: not asserted
	}{
		{
			name: "held: labelled", labels: []string{"runlore"}, body: marked("fpA"), verified: map[string]bool{"fpA": true},
			wantCalls: []string{add}, wantReason: "verified",
		},
		{
			name: "latest failed: waits", labels: []string{"runlore"}, body: marked("fpB"), verified: map[string]bool{"fpB": false},
			wantRisks: []Risk{RiskUnverified},
		},
		{
			name: "possible duplicate: waits", labels: []string{"runlore"}, body: dup + marked("fpC"), verified: map[string]bool{"fpC": true},
			wantRisks: []Risk{RiskContradicts},
		},
		{
			name: "a human's security label: waits", labels: []string{"runlore", "security"}, body: marked("fpD"), verified: map[string]bool{"fpD": true},
			wantRisks: []Risk{RiskSecurity},
		},
		{
			name: "contested: waits", labels: []string{"runlore"}, body: marked("fpE"), verified: map[string]bool{"fpE": true}, contested: true,
			wantRisks: []Risk{RiskContested},
		},
		{
			name: "no marker: waits", labels: []string{"runlore"}, verified: map[string]bool{"fpA": true},
			wantRisks: []Risk{RiskUnverified},
		},
		{
			name: "labelled and held: left", labels: []string{"runlore", "ready-to-merge"}, body: marked("fpA"), verified: map[string]bool{"fpA": true},
		},
		{
			name: "labelled, no observation: unverified alone withdraws nothing", labels: []string{"runlore", "ready-to-merge"}, body: marked("fpA"),
		},
		{
			name: "labelled, latest failed: withdrawn", labels: []string{"runlore", "ready-to-merge"}, body: marked("fpB"), verified: map[string]bool{"fpB": false},
			wantCalls: []string{withdraw}, wantReason: "evidence-withdrawn",
		},
		{
			name: "labelled, contested: withdrawn", labels: []string{"runlore", "ready-to-merge"}, body: marked("fpE"), verified: map[string]bool{"fpE": true}, contested: true,
			wantCalls: []string{withdraw}, wantReason: "evidence-withdrawn",
		},
		{
			name: "labelled, possible duplicate: withdrawn", labels: []string{"runlore", "ready-to-merge"}, body: dup + marked("fpC"), verified: map[string]bool{"fpC": true},
			wantCalls: []string{withdraw}, wantReason: "evidence-withdrawn",
		},
		{
			name: "labelled, security: withdrawn", labels: []string{"runlore", "ready-to-merge", "security"}, body: marked("fpD"), verified: map[string]bool{"fpD": true},
			wantCalls: []string{withdraw}, wantReason: "evidence-withdrawn",
		},
		{
			name: "labelled, accepted, latest failed: a human's override stands", labels: []string{"runlore", "ready-to-merge", "accepted"}, body: marked("fpB"), verified: map[string]bool{"fpB": false},
		},
		{
			name: "labelled, latest failed, no Evidence: adopters keep today's path", labels: []string{"runlore", "ready-to-merge"}, body: marked("fpB"), verified: map[string]bool{"fpB": false}, noEvidence: true,
		},
		{
			name: "a resolve PR stays a human merge", labels: []string{"runlore", resolveLabel}, body: marked("fpA"), verified: map[string]bool{"fpA": true},
		},
		{
			name: "a retire PR stays a human merge", labels: []string{"runlore", retireLabel}, body: marked("fpA"), verified: map[string]bool{"fpA": true},
		},
		{
			name: "a revalidate PR stays a human merge", labels: []string{"runlore", revalidateLabel}, body: marked("fpA"), verified: map[string]bool{"fpA": true},
		},
		{
			name: "an entry edit that is labelled is not touched either", labels: []string{"runlore", resolveLabel, "ready-to-merge"}, body: marked("fpB"), verified: map[string]bool{"fpB": false},
		},
		{
			name: "solved, resolved, no Evidence", labels: []string{"solved"}, resolved: true, noEvidence: true,
			wantCalls: []string{add}, wantReason: "resolved",
		},
		{
			name: "solved, resolved, contested, no Evidence: today's path", labels: []string{"solved"}, resolved: true, noEvidence: true, contested: true,
			wantCalls: []string{add}, wantReason: "resolved",
		},
		{
			name: "solved, resolved, unflagged, Evidence set", labels: []string{"solved"}, body: marked("fp"), resolved: true,
			wantCalls: []string{add}, wantReason: "resolved",
		},
		{name: "solved, unresolved, unflagged, Evidence set", labels: []string{"solved"}, body: marked("fp")},
		{name: "solved, resolved, contested, Evidence set", labels: []string{"solved"}, resolved: true, contested: true},
		{name: "solved, resolved, possible duplicate, Evidence set", labels: []string{"solved"}, body: dup, resolved: true},
		{name: "solved, resolved, security, Evidence set", labels: []string{"solved", "security"}, resolved: true},
		{name: "solved, resolved, latest failed, Evidence set", labels: []string{"solved"}, body: marked("fp"), verified: map[string]bool{"fp": false}, resolved: true},
		{
			name: "solved, accepted, contested, Evidence set", labels: []string{"solved", "accepted"}, contested: true,
			wantCalls: []string{add}, wantReason: "human-accepted",
		},
		{
			name: "a PR on both lists is handled once", labels: []string{"runlore", "solved"}, body: marked("fpA"), verified: map[string]bool{"fpA": true},
			wantCalls: []string{add}, wantReason: "verified",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pr := providers.CuratedIssue{Number: 7, Title: "KB: x", Body: tc.body, Labels: slices.Clone(tc.labels)}
			f := &queueForge{}
			f.prs = []providers.CuratedIssue{pr}
			ev := fakeEvidence{verified: tc.verified}
			if tc.contested {
				ev.urls = []string{prURL(7)}
			}
			var logs bytes.Buffer
			q := Queue{Forge: f, Checker: fakeChecker{resolved: map[int]bool{7: tc.resolved}}, KBRepo: queueRepo,
				Log: slog.New(slog.NewTextHandler(&logs, nil))}
			if !tc.noEvidence {
				q.Evidence = ev
			}
			if err := q.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.calls, tc.wantCalls) {
				t.Fatalf("calls = %v, want %v", f.calls, tc.wantCalls)
			}
			if tc.wantReason != "" && !strings.Contains(logs.String(), "reason="+tc.wantReason) {
				t.Errorf("want reason %q in the log: %s", tc.wantReason, logs.String())
			}
			if tc.wantRisks != nil {
				contested := map[int]bool{7: tc.contested}
				if got := Risks(pr, contested, tc.verified); !slices.Equal(got, tc.wantRisks) {
					t.Errorf("Risks = %v, want %v", got, tc.wantRisks)
				}
			}
		})
	}
}

func TestQueueContestedLinkMustNameThisPR(t *testing.T) {
	for name, url := range map[string]string{
		"another PR":    prURL(8),
		"another repo":  "https://github.com/acme/other/pull/7",
		"a plain issue": "https://github.com/acme/kb/issues/7",
	} {
		t.Run(name, func(t *testing.T) {
			f := &queueForge{}
			f.prs = []providers.CuratedIssue{{Number: 7, Body: providers.FingerprintMarker("fp"), Labels: []string{"runlore"}}}
			q := Queue{Forge: f, KBRepo: queueRepo, Log: discardLog(),
				Evidence: fakeEvidence{verified: map[string]bool{"fp": true}, urls: []string{url}}}
			if err := q.Run(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.calls, []string{"7:>ready-to-merge"}) {
				t.Fatalf("a foreign link contests nothing: %v", f.calls)
			}
		})
	}
}

// A contested trigger that cannot be joined must not read as "nothing contested".
func TestQueueBadKBRepoPromotesNothing(t *testing.T) {
	f := &queueForge{}
	f.prs = []providers.CuratedIssue{{Number: 7, Body: providers.FingerprintMarker("fp"), Labels: []string{"runlore"}}}
	q := Queue{Forge: f, KBRepo: "no-slash", Log: discardLog(),
		Evidence: fakeEvidence{verified: map[string]bool{"fp": true}, urls: []string{prURL(7)}}}
	if err := q.Run(context.Background()); err == nil {
		t.Error("Run: want an error for a kb_repo that is not owner/name")
	}
	if err := q.PromoteOne(context.Background(), 7); err == nil {
		t.Error("PromoteOne: want an error for a kb_repo that is not owner/name")
	}
	if len(f.calls) != 0 {
		t.Fatalf("no call may follow: %v", f.calls)
	}
}

func TestQueueRunSurvivesOnePRsFailure(t *testing.T) {
	held := map[string]bool{"fp": true}
	body := providers.FingerprintMarker("fp")

	t.Run("a label failure", func(t *testing.T) {
		f := &queueForge{failN: 7}
		f.prs = []providers.CuratedIssue{{Number: 7, Body: body, Labels: []string{"runlore"}}, {Number: 8, Body: body, Labels: []string{"runlore"}}}
		q := Queue{Forge: f, KBRepo: queueRepo, Log: discardLog(), Evidence: fakeEvidence{verified: held}}
		if err := q.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.calls, []string{"8:>ready-to-merge"}) {
			t.Fatalf("the next PR still gets its label: %v", f.calls)
		}
	})
	t.Run("a checker failure", func(t *testing.T) {
		f := &queueForge{}
		f.prs = []providers.CuratedIssue{{Number: 7, Labels: []string{"solved"}}, {Number: 8, Body: body, Labels: []string{"runlore"}}}
		q := Queue{Forge: f, Checker: failingChecker{}, KBRepo: queueRepo, Log: discardLog(), Evidence: fakeEvidence{verified: held}}
		if err := q.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.calls, []string{"8:>ready-to-merge"}) {
			t.Fatalf("the next PR still gets its label: %v", f.calls)
		}
	})
	for _, label := range []string{"solved", "runlore"} {
		t.Run("listing "+label+" fails", func(t *testing.T) {
			q := Queue{Forge: &queueForge{failList: label}, KBRepo: queueRepo, Log: discardLog(), Evidence: fakeEvidence{}}
			if err := q.Run(context.Background()); err == nil {
				t.Fatal("want the listing's error")
			}
		})
	}
}

type failingChecker struct{}

func (failingChecker) IsResolved(context.Context, providers.CuratedIssue) (bool, error) {
	return false, errors.New("ledger down")
}

func TestQueuePromoteOne(t *testing.T) {
	ctx := context.Background()
	marked := providers.FingerprintMarker("fp")
	newQueue := func(logs *bytes.Buffer, verified map[string]bool, prs ...providers.CuratedIssue) (Queue, *queueForge) {
		f := &queueForge{}
		f.prs = prs
		// No Checker: BuildFactoryCuration wires none, so a solved PR must not panic here.
		return Queue{Forge: f, KBRepo: queueRepo, Log: slog.New(slog.NewTextHandler(logs, nil)),
			Evidence: fakeEvidence{verified: verified}}, f
	}

	t.Run("held: labelled once", func(t *testing.T) {
		var logs bytes.Buffer
		q, f := newQueue(&logs, map[string]bool{"fp": true},
			providers.CuratedIssue{Number: 6, Body: marked, Labels: []string{"runlore"}},
			providers.CuratedIssue{Number: 7, Body: marked, Labels: []string{"runlore"}})
		for range 2 {
			if err := q.PromoteOne(ctx, 7); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(f.calls, []string{"7:>ready-to-merge"}) {
			t.Fatalf("only PR 7, and only once: %v", f.calls)
		}
		if !strings.Contains(logs.String(), "reason=verified") {
			t.Errorf("want reason verified: %s", logs.String())
		}
	})
	t.Run("latest failed: withdrawn", func(t *testing.T) {
		var logs bytes.Buffer
		q, f := newQueue(&logs, map[string]bool{"fp": false},
			providers.CuratedIssue{Number: 7, Body: marked, Labels: []string{"runlore", "ready-to-merge"}})
		if err := q.PromoteOne(ctx, 7); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.calls, []string{"7:ready-to-merge>"}) {
			t.Fatalf("want the withdrawal: %v", f.calls)
		}
		if !strings.Contains(logs.String(), "reason=evidence-withdrawn") {
			t.Errorf("want reason evidence-withdrawn: %s", logs.String())
		}
	})
	t.Run("not among the open runlore PRs: nil, no call", func(t *testing.T) {
		var logs bytes.Buffer
		q, f := newQueue(&logs, map[string]bool{"fp": true},
			providers.CuratedIssue{Number: 8, Body: marked, Labels: []string{"runlore"}},
			providers.CuratedIssue{Number: 7, Body: marked, Labels: []string{"solved"}})
		if err := q.PromoteOne(ctx, 7); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 0 {
			t.Fatalf("no call: %v", f.calls)
		}
	})
	t.Run("an entry edit: nil, no call", func(t *testing.T) {
		var logs bytes.Buffer
		q, f := newQueue(&logs, map[string]bool{"fp": true},
			providers.CuratedIssue{Number: 7, Body: marked, Labels: []string{"runlore", resolveLabel}})
		if err := q.PromoteOne(ctx, 7); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 0 {
			t.Fatalf("no call: %v", f.calls)
		}
	})
	t.Run("solved and unobserved, with no Checker: nil, no call", func(t *testing.T) {
		var logs bytes.Buffer
		q, f := newQueue(&logs, nil,
			providers.CuratedIssue{Number: 7, Labels: []string{"runlore", "solved"}})
		if err := q.PromoteOne(ctx, 7); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 0 {
			t.Fatalf("no call: %v", f.calls)
		}
	})
	t.Run("failures come back, so PromoteDue retries", func(t *testing.T) {
		var logs bytes.Buffer
		q, f := newQueue(&logs, map[string]bool{"fp": true},
			providers.CuratedIssue{Number: 7, Body: marked, Labels: []string{"runlore"}})
		f.failN = 7
		if err := q.PromoteOne(ctx, 7); err == nil {
			t.Error("a label failure must come back")
		}
		f.failN, f.failList = 0, "runlore"
		if err := q.PromoteOne(ctx, 7); err == nil {
			t.Error("a listing failure must come back")
		}
		f.failList = ""
		q.Checker = failingChecker{}
		q.Evidence = fakeEvidence{}
		f.prs[0].Labels = []string{"runlore", "solved"}
		if err := q.PromoteOne(ctx, 7); err == nil {
			t.Error("a checker failure must come back")
		}
	})
	t.Run("no Evidence: nil, no call", func(t *testing.T) {
		var logs bytes.Buffer
		q, f := newQueue(&logs, nil, providers.CuratedIssue{Number: 7, Labels: []string{"runlore", "solved", "accepted"}})
		q.Evidence = nil
		if err := q.PromoteOne(ctx, 7); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 0 {
			t.Fatalf("no call: %v", f.calls)
		}
	})
}
