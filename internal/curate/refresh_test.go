// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/outcome"
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

// rfPR is an open entry PR whose fingerprint is "fp"; rfHeld is the evidence that says it held.
func rfPR(n int, labels ...string) providers.CuratedIssue {
	return providers.CuratedIssue{Number: n, Title: "KB: a", Body: providers.FingerprintMarker("fp"), Labels: append([]string{"runlore"}, labels...)}
}

var rfHeld = qiEvidence{verified: map[string]bool{"fp": true}}

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
		{name: "another RunLore branch is not touched and is listed", state: rfState(func(st *providers.EntryPRState) { st.Branch = "runlore/revalidate-a" }), wantRefused: conflict},
		{name: "a human's branch is not touched and is listed", labels: []string{"ready-to-merge"}, state: rfState(func(st *providers.EntryPRState) { st.Branch = "fix/a" }), wantRefused: conflict},
		{name: "another branch that does not conflict is not listed", state: rfState(func(st *providers.EntryPRState) { st.Branch = "fix/a"; st.Mergeable = rfBool(true) })},
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
			r := RefreshEntries{Forge: f, Evidence: rfHeld, Log: discardLog(), Promote: func(_ context.Context, n int) error {
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
	r := RefreshEntries{Forge: f, Evidence: rfHeld, Log: discardLog(), Promote: func(context.Context, int) error { return errors.New("promote down") }}
	res, err := r.Once(context.Background())
	if err != nil || !slices.Equal(res.Rebuilt, []int{8}) || res.Refused[7] != "conflict" {
		t.Fatalf("%+v %v", res, err)
	}
}

// Final 2c: the first merge conflicts the whole backlog, so a pass rebuilds only what can use it.
func TestRefreshRebuildsOnlyWhatCanUseIt(t *testing.T) {
	labelled, held, neither := rfPR(1, "ready-to-merge"), rfPR(2), rfPR(3)
	labelled.Body, neither.Body = providers.FingerprintMarker("other"), providers.FingerprintMarker("other")
	failed := rfPR(4)
	failed.Body = providers.FingerprintMarker("failed")
	f := &fakeRefresh{open: []providers.CuratedIssue{neither, failed, held, labelled}, states: map[int]providers.EntryPRState{1: rfState(), 2: rfState(), 3: rfState(), 4: rfState()}}
	ev := qiEvidence{verified: map[string]bool{"fp": true, "failed": false}}
	res, err := RefreshEntries{Forge: f, Evidence: ev, Log: discardLog()}.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"label 1 ready-to-merge>", "rebuild 1 abc", "rebuild 2 abc"}
	if !slices.Equal(f.calls, want) || !slices.Equal(res.Rebuilt, []int{1, 2}) {
		t.Fatalf("calls %q rebuilt %v", f.calls, res.Rebuilt)
	}
	if res.Refused[3] != "conflict" || res.Refused[4] != "conflict" || len(res.Refused) != 2 {
		t.Fatalf("any other conflicting PR is listed, and nothing is written to it: %v", res.Refused)
	}

	t.Run("with no evidence, only the label counts", func(t *testing.T) {
		f := &fakeRefresh{open: []providers.CuratedIssue{rfPR(2), rfPR(1, "ready-to-merge")}, states: map[int]providers.EntryPRState{1: rfState(), 2: rfState()}}
		res, err := RefreshEntries{Forge: f, Log: discardLog()}.Once(context.Background())
		if err != nil || !slices.Equal(res.Rebuilt, []int{1}) || res.Refused[2] != "conflict" {
			t.Fatalf("%+v %v", res, err)
		}
	})
}

func TestRefreshStopsAtTheCap(t *testing.T) {
	f := &fakeRefresh{states: map[int]providers.EntryPRState{}}
	for n := 1; n <= maxRebuildsPerRun+2; n++ {
		f.open = append(f.open, rfPR(n))
		f.states[n] = rfState()
	}
	// The two with the label go first, wherever the forge lists them.
	last := len(f.open) - 1
	f.open[last].Labels = append(f.open[last].Labels, "ready-to-merge")
	f.open[last-1].Labels = append(f.open[last-1].Labels, "ready-to-merge")
	human := maxRebuildsPerRun + 3
	f.open = append(f.open, rfPR(human))
	f.states[human] = rfState(func(st *providers.EntryPRState) { st.CommitAuthors = []string{"Smana"} })

	res, err := RefreshEntries{Forge: f, Evidence: rfHeld, Log: discardLog()}.Once(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Rebuilt) != maxRebuildsPerRun || !slices.Contains(res.Rebuilt, last+1) || !slices.Contains(res.Rebuilt, last) {
		t.Fatalf("Rebuilt = %v", res.Rebuilt)
	}
	if len(res.Refused) != 3 || res.Refused[human] != "conflict" {
		t.Fatalf("what waits is listed for now, as is the refusal: %v", res.Refused)
	}
	rebuilds := slices.DeleteFunc(slices.Clone(f.calls), func(c string) bool { return !strings.HasPrefix(c, "rebuild") })
	if len(rebuilds) != maxRebuildsPerRun {
		t.Fatalf("calls: %q", f.calls)
	}
}

// A labelled PR past the cap keeps its label: it comes back next pass, and an accepted one has no
// evidence that would put the label back.
func TestRefreshPastTheCapWritesNothing(t *testing.T) {
	f := &fakeRefresh{states: map[int]providers.EntryPRState{}}
	for n := 1; n <= maxRebuildsPerRun+1; n++ {
		f.open = append(f.open, rfPR(n, "ready-to-merge"))
		f.states[n] = rfState()
	}
	res, err := RefreshEntries{Forge: f, Log: discardLog()}.Once(context.Background())
	if err != nil || len(res.Rebuilt) != maxRebuildsPerRun || len(res.Refused) != 1 {
		t.Fatalf("%+v %v", res, err)
	}
	if len(f.calls) != 2*maxRebuildsPerRun {
		t.Fatalf("calls: %q", f.calls)
	}
}

func TestRefreshStopsOnALimit(t *testing.T) {
	limited := fmt.Errorf("github: %w", providers.ErrRateLimited)
	two := func() *fakeRefresh {
		return &fakeRefresh{open: []providers.CuratedIssue{rfPR(1, "ready-to-merge"), rfPR(2, "ready-to-merge")}, states: map[int]providers.EntryPRState{1: rfState(), 2: rfState()}}
	}
	cases := []struct {
		name    string
		arrange func(*fakeRefresh, *RefreshEntries)
		want    []string
	}{
		{"on the rebuild", func(f *fakeRefresh, _ *RefreshEntries) { f.rebuildErr = limited }, []string{"label 1 ready-to-merge>", "rebuild 1 abc"}},
		{"on the label", func(f *fakeRefresh, _ *RefreshEntries) { f.labelErr = limited }, nil},
		{"on the read", func(f *fakeRefresh, _ *RefreshEntries) { f.stateErr = limited }, nil},
		{"on the promotion", func(_ *fakeRefresh, r *RefreshEntries) {
			r.Promote = func(context.Context, int) error { return limited }
		}, []string{"label 1 ready-to-merge>", "rebuild 1 abc"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := two()
			r := RefreshEntries{Forge: f, Log: discardLog()}
			tc.arrange(f, &r)
			res, err := r.Once(context.Background())
			if err != nil || !res.Stopped {
				t.Fatalf("%+v %v", res, err)
			}
			if !slices.Equal(f.calls, tc.want) {
				t.Fatalf("calls %q, want %q", f.calls, tc.want)
			}
			if err := r.Run(context.Background()); !errors.Is(err, providers.ErrRateLimited) {
				t.Fatalf("the command says so: %v", err)
			}
		})
	}
	t.Run("any other failure does not stop the pass", func(t *testing.T) {
		f := two()
		f.rebuildErr = errors.New("down")
		res, err := RefreshEntries{Forge: f, Log: discardLog()}.Once(context.Background())
		if err != nil || res.Stopped || len(f.calls) != 4 {
			t.Fatalf("%+v %v %q", res, err, f.calls)
		}
	})
}

func TestRefreshOnePR(t *testing.T) {
	ctx := context.Background()
	human := func(st *providers.EntryPRState) { st.CommitAuthors = []string{"Smana"} }
	cases := []struct {
		name      string
		pr        providers.CuratedIssue
		state     providers.EntryPRState
		only      int
		rebuild   error
		wantCalls []string
		wantErr   string
		wantProm  bool
	}{
		{name: "neither label nor evidence: a human asked", pr: rfPR(7), state: rfState(), only: 7, wantCalls: []string{"rebuild 7 abc"}, wantProm: true},
		{name: "a labelled one loses the label first", pr: rfPR(7, "ready-to-merge"), state: rfState(), only: 7, wantCalls: []string{"label 7 ready-to-merge>", "rebuild 7 abc"}, wantProm: true},
		{name: "a human's commit is still refused", pr: rfPR(7), state: rfState(human), only: 7, wantErr: "refused"},
		{name: "a fork is still refused", pr: rfPR(7), state: rfState(func(st *providers.EntryPRState) { st.Fork = true }), only: 7, wantErr: "refused"},
		{name: "another branch is still refused", pr: rfPR(7), state: rfState(func(st *providers.EntryPRState) { st.Branch = "fix/a" }), only: 7, wantErr: "refused"},
		{name: "a moved head is an error", pr: rfPR(7), state: rfState(), only: 7, rebuild: providers.ErrHeadMoved, wantCalls: []string{"rebuild 7 abc"}, wantErr: "moved"},
		{name: "the entry on the base is refused", pr: rfPR(7), state: rfState(), only: 7, rebuild: providers.ErrEntryOnBase, wantCalls: []string{"rebuild 7 abc"}, wantErr: "refused"},
		{name: "not conflicting: nothing to do", pr: rfPR(7), state: rfState(func(st *providers.EntryPRState) { st.Mergeable = rfBool(true) }), only: 7},
		{name: "still computing says so", pr: rfPR(7), state: rfState(func(st *providers.EntryPRState) { st.Mergeable = nil }), only: 7, wantErr: "computing"},
		{name: "no such open PR", pr: rfPR(7), state: rfState(), only: 8, wantErr: "not an open"},
		{name: "an entry edit", pr: rfPR(7, retireLabel), state: rfState(), only: 7, wantErr: "entry edit"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			other := rfPR(9, "ready-to-merge")
			f := &fakeRefresh{open: []providers.CuratedIssue{other, tc.pr}, states: map[int]providers.EntryPRState{7: tc.state, 9: rfState()}, rebuildErr: tc.rebuild}
			promoted := 0
			r := RefreshEntries{Forge: f, Only: tc.only, Log: discardLog(), Promote: func(context.Context, int) error { promoted++; return nil }}
			err := r.Run(ctx)
			if tc.wantErr == "" && err != nil || tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)) {
				t.Fatalf("err = %v, want %q", err, tc.wantErr)
			}
			if !slices.Equal(f.calls, tc.wantCalls) {
				t.Fatalf("calls %q, want %q", f.calls, tc.wantCalls)
			}
			if (promoted == 1) != tc.wantProm {
				t.Fatalf("promoted %d", promoted)
			}
		})
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

// Final 2c, I3 and Task 12's M6: a 👎 since the last sweep, through the real ledger. queue-issue runs
// the Queue pass first, so the label comes off and the PR reaches Decide, ranked contested.
func TestQueueIssueReconcilesARealContest(t *testing.T) {
	ctx := context.Background()
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Date(2026, 11, 4, 8, 0, 0, 0, time.UTC)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, fp := range []string{"fp7", "fp8"} {
		must(ledger.Verify(outcome.Verification{Entry: "incidents/" + fp + ".md", DupFingerprint: fp, TriggerKey: fp, Room: "room-" + fp, Held: true, At: at}))
	}
	must(ledger.Open(outcome.Event{TriggerKey: "k7", Verdict: "action_required", At: at}))
	must(ledger.Link("k7", prURL(7), at))
	must(ledger.Feedback("k7", "down", "U1", at.Add(time.Minute)))

	pr := func(n int, fp string) providers.CuratedIssue {
		p := rfPR(n, "ready-to-merge")
		p.Title, p.Body = fmt.Sprintf("KB: pr%d", n), providers.FingerprintMarker(fp)
		return p
	}
	mergeable := func(st *providers.EntryPRState) { st.Mergeable = rfBool(true) }
	build := func(f *fakeRefresh) QueueIssue {
		return QueueIssue{Forge: f, Evidence: ledger, KBRepo: qiRepo, Log: discardLog(),
			Reconcile: Queue{Forge: f, Evidence: ledger, KBRepo: qiRepo, Log: discardLog()},
			Refresh:   &RefreshEntries{Forge: f, Evidence: ledger, Log: discardLog()}}
	}

	f := &fakeRefresh{open: []providers.CuratedIssue{pr(7, "fp7"), pr(8, "fp8")}, states: map[int]providers.EntryPRState{7: rfState(mergeable), 8: rfState(mergeable)}}
	if err := build(f).RunNow(ctx); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f.calls, []string{"label 7 ready-to-merge>"}) {
		t.Fatalf("the contested label comes off, the other stays: %q", f.calls)
	}
	if !strings.Contains(f.issueBody, "| contested | #7") {
		t.Fatalf("Decide lists it:\n%s", f.issueBody)
	}
	if strings.Contains(f.issueBody, "pr8") {
		t.Fatalf("the uncontested one stays out:\n%s", f.issueBody)
	}

	t.Run("the Queue runs before the refresh", func(t *testing.T) {
		// Held, unlabelled, and a conflict RunLore must refuse. The Queue labels it on the evidence, and
		// only a refresh that runs after it can take the label off and leave it in Decide. In the other
		// order it would end up ready-to-merge and absent from Decide.
		human := func(st *providers.EntryPRState) { st.CommitAuthors = []string{"Smana"} }
		p := pr(8, "fp8")
		p.Labels = []string{"runlore"}
		f := &fakeRefresh{open: []providers.CuratedIssue{p}, states: map[int]providers.EntryPRState{8: rfState(human)}}
		if err := build(f).RunNow(ctx); err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(f.calls, []string{"label 8 >ready-to-merge", "label 8 ready-to-merge>"}) {
			t.Fatalf("calls: %q", f.calls)
		}
		if !strings.Contains(f.issueBody, "| conflict | #8") {
			t.Fatalf("Decide lists it:\n%s", f.issueBody)
		}
	})
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
