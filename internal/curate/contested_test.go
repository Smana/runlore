// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/outcome"
)

// contestedForge is a scripted ContestedForge: PR open-states and existing
// comment bodies are fixtures; posted comments and call counts are recorded.
type contestedForge struct {
	openPRs    map[int]bool     // number → open?
	comments   map[int][]string // number → existing comment bodies
	stateErr   map[int]error    // number → IsPROpen failure
	commentErr map[int]error    // number → Comment failure

	posted     map[int][]string // number → bodies posted this run
	stateCalls int
	listCalls  int
}

func (f *contestedForge) IsPROpen(_ context.Context, n int) (bool, error) {
	f.stateCalls++
	if err := f.stateErr[n]; err != nil {
		return false, err
	}
	return f.openPRs[n], nil
}

func (f *contestedForge) ListIssueCommentBodies(_ context.Context, n int) ([]string, error) {
	f.listCalls++
	return f.comments[n], nil
}

func (f *contestedForge) Comment(_ context.Context, n int, body string) error {
	if err := f.commentErr[n]; err != nil {
		return err
	}
	if f.posted == nil {
		f.posted = map[int][]string{}
	}
	f.posted[n] = append(f.posted[n], body)
	return nil
}

// fakeContested is a fixed ContestedSource.
type fakeContested struct{ cts []outcome.ContestedTrigger }

func (f fakeContested) ContestedTriggers() []outcome.ContestedTrigger { return f.cts }

func contested(t *testing.T, f *contestedForge, cts ...outcome.ContestedTrigger) Contested {
	t.Helper()
	return Contested{Forge: f, Ledger: fakeContested{cts: cts}, KBRepo: "o/r", Log: discardLog()}
}

func TestContestedCommentsOnOpenKBPR(t *testing.T) {
	f := &contestedForge{openPRs: map[int]bool{7: true}}
	p := contested(t, f, outcome.ContestedTrigger{
		TriggerKey: "alert:web-down", CuratedURL: "https://github.com/o/r/pull/7",
		Downs: 2, Last: time.Unix(50000, 0),
	})
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.posted[7]) != 1 {
		t.Fatalf("want exactly one comment on PR 7, got %+v", f.posted)
	}
	body := f.posted[7][0]
	for _, want := range []string{
		"2 standing 👎",                    // the vote count the reviewer weighs
		"alert:web-down",                  // the trigger key, so the vote is attributable
		"before merging",                  // the actionable ask
		"re-arm",                          // the cooldown side effect: a fresher conclusion may follow
		contestedMarker("alert:web-down"), // the idempotency marker
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("comment missing %q:\n%s", want, body)
		}
	}
}

// TestContestedIdempotentWhenMarkerPresent: an existing comment carrying this
// trigger's marker means the warning was already surfaced — never re-post, even
// though the votes still stand on every later run.
func TestContestedIdempotentWhenMarkerPresent(t *testing.T) {
	f := &contestedForge{
		openPRs:  map[int]bool{7: true},
		comments: map[int][]string{7: {"older note\n" + contestedMarker("k")}},
	}
	p := contested(t, f, outcome.ContestedTrigger{TriggerKey: "k", CuratedURL: "https://github.com/o/r/pull/7", Downs: 1})
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.posted) != 0 {
		t.Fatalf("marker present: must not re-post, got %+v", f.posted)
	}
}

// TestContestedSkipsForeignAndNonPRURLs: only a pull-request URL inside the
// configured kb_repo is actionable. Foreign repos, plain issues, and malformed
// URLs are skipped without any forge round-trip.
func TestContestedSkipsForeignAndNonPRURLs(t *testing.T) {
	f := &contestedForge{openPRs: map[int]bool{7: true}}
	p := contested(t, f,
		outcome.ContestedTrigger{TriggerKey: "k1", CuratedURL: "https://github.com/other/repo/pull/7", Downs: 1},
		outcome.ContestedTrigger{TriggerKey: "k2", CuratedURL: "https://github.com/o/r/issues/7", Downs: 1},
		outcome.ContestedTrigger{TriggerKey: "k3", CuratedURL: "://not-a-url", Downs: 1},
		outcome.ContestedTrigger{TriggerKey: "k4", CuratedURL: "https://github.com/o/r/pull/zero", Downs: 1},
	)
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.stateCalls != 0 || f.listCalls != 0 || len(f.posted) != 0 {
		t.Fatalf("foreign/non-PR URLs must cost no forge calls: state=%d list=%d posted=%+v", f.stateCalls, f.listCalls, f.posted)
	}
}

// TestContestedSkipsClosedPR: a merged/closed KB PR has left review — the votes
// keep weighing recall trust, but there is no reviewer left to warn.
func TestContestedSkipsClosedPR(t *testing.T) {
	f := &contestedForge{openPRs: map[int]bool{7: false}}
	p := contested(t, f, outcome.ContestedTrigger{TriggerKey: "k", CuratedURL: "https://github.com/o/r/pull/7", Downs: 1})
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.listCalls != 0 || len(f.posted) != 0 {
		t.Fatalf("closed PR must get no comment (list=%d posted=%+v)", f.listCalls, f.posted)
	}
}

// TestContestedDisabledLedgerNoop: with the outcome feature off, the pass costs
// nothing — the disabled ledger's ContestedTriggers is nil, so no forge call is
// ever made (the "no new config" contract).
func TestContestedDisabledLedgerNoop(t *testing.T) {
	f := &contestedForge{}
	disabled, err := outcome.New("")
	if err != nil {
		t.Fatalf("outcome.New: %v", err)
	}
	p := Contested{Forge: f, Ledger: disabled, KBRepo: "o/r", Log: discardLog()}
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if f.stateCalls != 0 || f.listCalls != 0 || len(f.posted) != 0 {
		t.Fatalf("disabled ledger must be a total no-op, got state=%d list=%d posted=%+v", f.stateCalls, f.listCalls, f.posted)
	}
}

// TestContestedPerItemFailuresDoNotAbort: a state-check or comment failure on
// one trigger is logged and skipped; the remaining triggers still get their
// warning (best-effort, like every other pass).
func TestContestedPerItemFailuresDoNotAbort(t *testing.T) {
	f := &contestedForge{
		openPRs:    map[int]bool{7: true, 8: true, 9: true},
		stateErr:   map[int]error{7: errors.New("boom")},
		commentErr: map[int]error{8: errors.New("boom")},
	}
	p := contested(t, f,
		outcome.ContestedTrigger{TriggerKey: "k1", CuratedURL: "https://github.com/o/r/pull/7", Downs: 1},
		outcome.ContestedTrigger{TriggerKey: "k2", CuratedURL: "https://github.com/o/r/pull/8", Downs: 1},
		outcome.ContestedTrigger{TriggerKey: "k3", CuratedURL: "https://github.com/o/r/pull/9", Downs: 1},
	)
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.posted[9]) != 1 || len(f.posted[7]) != 0 || len(f.posted[8]) != 0 {
		t.Fatalf("only PR 9 should carry a comment, got %+v", f.posted)
	}
}

// TestContestedTwoTriggersOnePR: two contested triggers whose newest entries
// coalesced onto ONE PR each get their own (distinctly markered) comment, and
// the PR's state/comments are fetched once, not per trigger.
func TestContestedTwoTriggersOnePR(t *testing.T) {
	f := &contestedForge{openPRs: map[int]bool{7: true}}
	p := contested(t, f,
		outcome.ContestedTrigger{TriggerKey: "k1", CuratedURL: "https://github.com/o/r/pull/7", Downs: 1},
		outcome.ContestedTrigger{TriggerKey: "k2", CuratedURL: "https://github.com/o/r/pull/7", Downs: 3},
	)
	if err := p.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.posted[7]) != 2 {
		t.Fatalf("want one comment per trigger, got %+v", f.posted)
	}
	if f.stateCalls != 1 || f.listCalls != 1 {
		t.Fatalf("state/comments must be fetched once per PR, got state=%d list=%d", f.stateCalls, f.listCalls)
	}
	if contestedMarker("k1") == contestedMarker("k2") {
		t.Fatal("markers must be distinct per trigger")
	}
}

// TestContestedBadKBRepoErrors: a kb_repo that is not owner/name is a wiring
// bug, surfaced as the pass error (RunCurate validates it too — belt and braces).
func TestContestedBadKBRepoErrors(t *testing.T) {
	p := Contested{Forge: &contestedForge{}, Ledger: fakeContested{cts: []outcome.ContestedTrigger{{TriggerKey: "k", CuratedURL: "https://x/o/r/pull/1", Downs: 1}}}, KBRepo: "just-a-name", Log: discardLog()}
	if err := p.Run(context.Background()); err == nil {
		t.Fatal("malformed KBRepo must error")
	}
}
