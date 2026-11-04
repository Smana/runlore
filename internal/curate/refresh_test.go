// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/providers"
)

// fakeRefresh is an open PR list, each PR's state, and the calls made on them, in order. It is a
// whole GuardedForge, so the Queue, the queue issue and Guard can all run over it.
type fakeRefresh struct {
	fakeGuarded
	open       []providers.CuratedIssue
	states     map[int]providers.EntryPRState
	stateErr   error
	labelErr   error
	rebuildErr error
	calls      []string
	issueBody  string
}

func (f *fakeRefresh) ListPRsByLabel(_ context.Context, label string) ([]providers.CuratedIssue, error) {
	var out []providers.CuratedIssue
	for _, pr := range f.open {
		if slices.Contains(pr.Labels, label) {
			out = append(out, pr)
		}
	}
	return out, nil
}

func (f *fakeRefresh) ReplaceLabel(_ context.Context, n int, remove, add string) error {
	if f.labelErr != nil {
		return f.labelErr
	}
	f.calls = append(f.calls, fmt.Sprintf("label %d %s>%s", n, remove, add))
	for i := range f.open {
		if f.open[i].Number != n {
			continue
		}
		f.open[i].Labels = slices.DeleteFunc(slices.Clone(f.open[i].Labels), func(l string) bool { return l == remove })
		if add != "" {
			f.open[i].Labels = append(f.open[i].Labels, add)
		}
	}
	return nil
}

func (f *fakeRefresh) EntryPRState(_ context.Context, n int) (providers.EntryPRState, error) {
	return f.states[n], f.stateErr
}

func (f *fakeRefresh) RebuildEntryPR(_ context.Context, n int, head string) error {
	f.calls = append(f.calls, fmt.Sprintf("rebuild %d %s", n, head))
	return f.rebuildErr
}

func (f *fakeRefresh) CreateIssue(_ context.Context, _, body string, _ []string) (int, providers.Ref, error) {
	f.issueBody = body
	return 7, providers.Ref{URL: "https://forge/issues/7"}, nil
}

func rfBool(b bool) *bool { return &b }

// rfState is a conflicting PR RunLore can rebuild; each table row spoils one thing about it.
func rfState(spoil ...func(*providers.EntryPRState)) providers.EntryPRState {
	st := providers.EntryPRState{
		Branch: "runlore/kb-a", HeadSHA: "abc", Mergeable: rfBool(false),
		CommitAuthors: []string{BotLogin, BotLogin, BotLogin},
		Files:         []string{"incidents/a.md", "index.md", "log.md"},
	}
	for _, s := range spoil {
		s(&st)
	}
	return st
}

func rfPR(n int, labels ...string) providers.CuratedIssue {
	return providers.CuratedIssue{Number: n, Title: "KB: a", Labels: append([]string{"runlore"}, labels...)}
}

func TestRefreshEntries(t *testing.T) {
	const (
		rebuild = "rebuild 7 abc"
		remove  = "label 7 ready-to-merge>"
	)
	conflict := map[int]string{7: "conflict"}
	humanCommit := func(st *providers.EntryPRState) { st.CommitAuthors = []string{BotLogin, "Smana"} }
	cases := []struct {
		name         string
		labels       []string
		state        providers.EntryPRState
		stateErr     error
		labelErr     error
		rebuildErr   error
		wantCalls    []string
		wantRebuilt  []int
		wantRefused  map[int]string
		wantPromoted []int
	}{
		{name: "the bot's own conflicting PR is rebuilt", state: rfState(),
			wantCalls: []string{rebuild}, wantRebuilt: []int{7}, wantPromoted: []int{7}},
		{name: "a labelled one loses the label first", labels: []string{"ready-to-merge"}, state: rfState(),
			wantCalls: []string{remove, rebuild}, wantRebuilt: []int{7}, wantPromoted: []int{7}},
		{name: "a failed removal means no rebuild", labels: []string{"ready-to-merge"}, state: rfState(), labelErr: errors.New("down")},
		{name: "a mergeable PR is left alone", labels: []string{"ready-to-merge"}, state: rfState(func(st *providers.EntryPRState) { st.Mergeable = rfBool(true) })},
		{name: "while GitHub computes, it is left alone", labels: []string{"ready-to-merge"}, state: rfState(func(st *providers.EntryPRState) { st.Mergeable = nil })},
		{name: "another RunLore branch is left alone", state: rfState(func(st *providers.EntryPRState) { st.Branch = "runlore/revalidate-a" })},
		{name: "a human's branch is left alone", state: rfState(func(st *providers.EntryPRState) { st.Branch = "fix/a" })},
		{name: "an entry edit is never looked at", labels: []string{retireLabel}, state: rfState()},
		{name: "a failed read moves on", state: rfState(), stateErr: errors.New("down")},
		{name: "a human's commit is refused", state: rfState(humanCommit), wantRefused: conflict},
		{name: "a labelled one with a human's commit loses the label and is refused", labels: []string{"ready-to-merge"}, state: rfState(humanCommit),
			wantCalls: []string{remove}, wantRefused: conflict},
		{name: "a commit with no linked account is refused", state: rfState(func(st *providers.EntryPRState) { st.CommitAuthors = []string{BotLogin, ""} }), wantRefused: conflict},
		{name: "no commit to vouch for it is refused", state: rfState(func(st *providers.EntryPRState) { st.CommitAuthors = nil }), wantRefused: conflict},
		{name: "a fork is refused", state: rfState(func(st *providers.EntryPRState) { st.Fork = true }), wantRefused: conflict},
		{name: "a full page of commits is refused", state: rfState(func(st *providers.EntryPRState) { st.Truncated = true }), wantRefused: conflict},
		{name: "a path a rebuild would lose is refused", state: rfState(func(st *providers.EntryPRState) { st.Files = append(st.Files, "incidents/b.md") }), wantRefused: conflict},
		{name: "a path outside the bundle is refused", state: rfState(func(st *providers.EntryPRState) { st.Files = append(st.Files, "scripts/x.sh") }), wantRefused: conflict},
		{name: "no entry to re-put is refused", state: rfState(func(st *providers.EntryPRState) { st.Files = []string{"log.md"} }), wantRefused: conflict},
		{name: "the only other path is no entry", state: rfState(func(st *providers.EntryPRState) { st.Files = []string{"scripts/x.sh", "log.md"} }), wantRefused: conflict},
		{name: "the entry already on the base is refused", state: rfState(), rebuildErr: fmt.Errorf("x: %w", providers.ErrEntryOnBase),
			wantCalls: []string{rebuild}, wantRefused: conflict},
		{name: "a branch name the forge will not move is refused", state: rfState(func(st *providers.EntryPRState) { st.Branch = "runlore/kb-a#b" }),
			rebuildErr: fmt.Errorf("x: %w", providers.ErrNotRunLorePR), wantCalls: []string{"rebuild 7 abc"}, wantRefused: conflict},
		{name: "a moved head is retried next pass", state: rfState(), rebuildErr: fmt.Errorf("x: %w", providers.ErrHeadMoved), wantCalls: []string{rebuild}},
		{name: "any other failure is retried next pass", state: rfState(), rebuildErr: errors.New("down"), wantCalls: []string{rebuild}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRefresh{
				open:     []providers.CuratedIssue{rfPR(7, tc.labels...)},
				states:   map[int]providers.EntryPRState{7: tc.state},
				stateErr: tc.stateErr, labelErr: tc.labelErr, rebuildErr: tc.rebuildErr,
			}
			var promoted []int
			r := RefreshEntries{Forge: f, Log: discardLog(), Promote: func(_ context.Context, n int) error {
				promoted = append(promoted, n)
				return nil
			}}
			res, err := r.Once(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(f.calls, tc.wantCalls) {
				t.Fatalf("calls = %q, want %q", f.calls, tc.wantCalls)
			}
			if !slices.Equal(res.Rebuilt, tc.wantRebuilt) {
				t.Fatalf("Rebuilt = %v, want %v", res.Rebuilt, tc.wantRebuilt)
			}
			if len(res.Refused) != len(tc.wantRefused) || res.Refused[7] != tc.wantRefused[7] {
				t.Fatalf("Refused = %v, want %v", res.Refused, tc.wantRefused)
			}
			if !slices.Equal(promoted, tc.wantPromoted) {
				t.Fatalf("promoted = %v, want %v", promoted, tc.wantPromoted)
			}
		})
	}
}

func TestRefreshEntriesOnePRDoesNotStarveTheRest(t *testing.T) {
	f := &fakeRefresh{
		open: []providers.CuratedIssue{rfPR(7), rfPR(8)},
		states: map[int]providers.EntryPRState{
			7: rfState(func(st *providers.EntryPRState) { st.CommitAuthors = []string{"Smana"} }),
			8: rfState(),
		},
	}
	r := RefreshEntries{Forge: f, Log: discardLog(), Promote: func(context.Context, int) error { return errors.New("promote down") }}
	res, err := r.Once(context.Background())
	if err != nil || !slices.Equal(res.Rebuilt, []int{8}) || res.Refused[7] != "conflict" {
		t.Fatalf("%+v %v", res, err)
	}
}

func TestRefreshEntriesThroughGuard(t *testing.T) {
	ctx := context.Background()
	labelled := func() *fakeRefresh {
		return &fakeRefresh{open: []providers.CuratedIssue{rfPR(7, "ready-to-merge")}, states: map[int]providers.EntryPRState{7: rfState()}}
	}

	t.Run("dry-run logs and audits, and the forge writes nothing", func(t *testing.T) {
		inner, aud := labelled(), &recAudit{}
		var logged bytes.Buffer
		g := Guard{Inner: inner, DryRun: true, Audit: aud, Log: slog.New(slog.NewTextHandler(&logged, nil))}
		res, err := RefreshEntries{Forge: g, Log: discardLog()}.Once(ctx)
		if err != nil || !slices.Equal(res.Rebuilt, []int{7}) {
			t.Fatalf("%+v %v", res, err)
		}
		if len(inner.calls) != 0 {
			t.Fatalf("a dry-run reached the forge: %q", inner.calls)
		}
		if len(aud.recs) != 2 || aud.recs[0].Op != "kb.relabel" || aud.recs[0].Decision != audit.DecisionDryRun ||
			aud.recs[1].Op != "kb.refresh-entry" || aud.recs[1].Target != "pr/7" || aud.recs[1].Decision != audit.DecisionDryRun {
			t.Fatalf("audit: %+v", aud.recs)
		}
		for _, op := range []string{"op=kb.relabel", "op=kb.refresh-entry"} {
			if !strings.Contains(logged.String(), op) {
				t.Fatalf("%s not logged: %s", op, logged.String())
			}
		}
	})

	t.Run("not dry-run, the chain records the rebuild", func(t *testing.T) {
		inner, aud := labelled(), &recAudit{}
		g := Guard{Inner: inner, Audit: aud, Log: discardLog()}
		if _, err := (RefreshEntries{Forge: g, Log: discardLog()}).Once(ctx); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(inner.calls, []string{"label 7 ready-to-merge>", "rebuild 7 abc"}) {
			t.Fatalf("calls: %q", inner.calls)
		}
		last := aud.recs[len(aud.recs)-1]
		if last.Op != "kb.refresh-entry" || last.Target != "pr/7" || last.Decision != audit.DecisionExecuted {
			t.Fatalf("audit: %+v", last)
		}
	})

	t.Run("a head that moved or an entry already merged was no failure", func(t *testing.T) {
		for _, sentinel := range []error{providers.ErrHeadMoved, providers.ErrEntryOnBase} {
			inner, aud := labelled(), &recAudit{}
			inner.rebuildErr = sentinel
			g := Guard{Inner: inner, Audit: aud, Log: discardLog()}
			if err := g.RebuildEntryPR(ctx, 7, "abc"); !errors.Is(err, sentinel) {
				t.Fatal(err)
			}
			if aud.recs[0].Decision != audit.DecisionSkipped {
				t.Fatalf("%v: audit: %+v", sentinel, aud.recs[0])
			}
		}
	})
}

func TestQueueRunRefreshesFirst(t *testing.T) {
	marked := func(n int, fp string, labels ...string) providers.CuratedIssue {
		pr := rfPR(n, labels...)
		pr.Body = providers.FingerprintMarker(fp)
		return pr
	}
	f := &fakeRefresh{
		open: []providers.CuratedIssue{marked(7, "fp7", "ready-to-merge"), marked(8, "fp8", "ready-to-merge", "accepted", "solved"), marked(9, "fp9")},
		states: map[int]providers.EntryPRState{
			7: rfState(),
			8: rfState(func(st *providers.EntryPRState) { st.CommitAuthors = []string{"Smana"} }),
			9: rfState(func(st *providers.EntryPRState) { st.CommitAuthors = []string{"Smana"} }),
		},
	}
	var logged bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logged, nil))
	q := Queue{
		Forge:    f,
		Evidence: fakeEvidence{verified: map[string]bool{"fp7": true, "fp8": true, "fp9": true}},
		Refresh:  &RefreshEntries{Forge: f, Log: log},
		Log:      log,
	}
	if err := q.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"label 7 ready-to-merge>", "rebuild 7 abc",
		"label 8 ready-to-merge>",
		"label 7 >ready-to-merge",
	}
	if !slices.Equal(f.calls, want) {
		t.Fatalf("a rebuilt PR is labelled again, a refused one never: %q", f.calls)
	}
	if !strings.Contains(logged.String(), "reason=verified") {
		t.Fatalf("the reason: %s", logged.String())
	}
}

func TestQueueOnResolutionNeverLabelsARefusedPR(t *testing.T) {
	f := &fakeRefresh{
		open:   []providers.CuratedIssue{rfPR(8, "solved", "accepted")},
		states: map[int]providers.EntryPRState{8: rfState(func(st *providers.EntryPRState) { st.Fork = true })},
	}
	q := Queue{Forge: f, Refresh: &RefreshEntries{Forge: f, Log: discardLog()}, Log: discardLog()}
	if err := q.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.calls) != 0 {
		t.Fatalf("calls: %q", f.calls)
	}
}

func TestQueueIssueRefreshesFirst(t *testing.T) {
	marked := func(n int, fp string, labels ...string) providers.CuratedIssue {
		pr := rfPR(n, labels...)
		pr.Title = fmt.Sprintf("KB: pr%d", n)
		pr.Body = providers.FingerprintMarker(fp)
		return pr
	}
	setup := func() *fakeRefresh {
		human := func(st *providers.EntryPRState) { st.CommitAuthors = []string{"Smana"} }
		mergeable := func(st *providers.EntryPRState) { st.Mergeable = rfBool(true) }
		return &fakeRefresh{
			open: []providers.CuratedIssue{
				marked(1, "verified", "ready-to-merge"), // rebuilt, and promoted again
				marked(2, "unverified"),                 // nothing stands against it but the evidence
				marked(3, "verified", "security"),       // security outranks conflict
				marked(4, "verified"),                   // refused
				marked(5, "unverified"),                 // refused: conflict outranks unverified
			},
			states: map[int]providers.EntryPRState{
				1: rfState(), 2: rfState(mergeable), 3: rfState(human), 4: rfState(human), 5: rfState(human),
			},
		}
	}
	build := func(f *fakeRefresh) QueueIssue {
		ev := qiEvidence{verified: map[string]bool{"verified": true}}
		queue := Queue{Forge: f, Evidence: ev, Log: discardLog()}
		return QueueIssue{
			Forge: f, Evidence: ev, KBRepo: qiRepo, Log: discardLog(),
			Refresh: &RefreshEntries{Forge: f, Promote: queue.PromoteOne, Log: discardLog()},
		}
	}
	check := func(t *testing.T, f *fakeRefresh) {
		t.Helper()
		body := f.issueBody
		if strings.Contains(body, "pr1") {
			t.Fatalf("a rebuilt, verified PR is ready-to-merge and absent from Decide:\n%s", body)
		}
		if !inOrder(body, "| security | #3", "| conflict | #4", "| conflict | #5", "| unverified | #2") {
			t.Fatalf("the order is security, conflict, unverified:\n%s", body)
		}
		if !slices.Contains(f.open[0].Labels, "ready-to-merge") {
			t.Fatalf("the Queue's evidence labels it again: %+v", f.open[0])
		}
	}

	t.Run("RunNow", func(t *testing.T) {
		f := setup()
		if err := build(f).RunNow(context.Background()); err != nil {
			t.Fatal(err)
		}
		check(t, f)
	})
	t.Run("Run, once due", func(t *testing.T) {
		f := setup()
		if err := build(f).Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		check(t, f)
	})
	t.Run("Run with no schedule writes nothing and rebuilds nothing", func(t *testing.T) {
		f := setup()
		q := build(f)
		q.Unscheduled = true
		if err := q.Run(context.Background()); err != nil {
			t.Fatal(err)
		}
		if len(f.calls) != 0 || f.issueBody != "" {
			t.Fatalf("calls: %q", f.calls)
		}
	})
}
