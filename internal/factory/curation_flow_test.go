// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// flowRig is the whole path: a fake factory behind Curation's filer, the signed webhook Receiver, the
// Verifier on one clock, over a real curator and a real outcome ledger.
type flowRig struct {
	t        *testing.T
	g        *onceRig
	factory  *fakeFactory
	vs       *verifyServer
	rc       *Receiver
	v        *Verifier
	forge    *entryForge
	outcomes *outcome.Ledger
	firing   bool
	sent     int
}

func newFlowRig(t *testing.T) *flowRig {
	t.Helper()
	g := newOnceRig(t)
	f := &flowRig{t: t, g: g, factory: &fakeFactory{ttl: time.Hour}, vs: &verifyServer{}, forge: &entryForge{}}
	var err error
	f.outcomes, err = outcome.New(filepath.Join(t.TempDir(), "outcome.jsonl"))
	must(t, err)
	g.cu.Filer = &Notifier{Client: newTestClient(t, f.factory, &g.now), Ledger: g.l, Repository: fixRepo, MaxCostUSD: 2}
	g.cu.Outcome, g.cu.Drafts = f.outcomes, f.forge
	g.cu.Inner = &curator.Curator{Forge: f.forge, MinConfidence: 0.7, Log: slog.New(slog.DiscardHandler)}
	key := bytes.Repeat([]byte{7}, 32)
	f.rc = &Receiver{Keys: func() [][]byte { return [][]byte{key} }, Ledger: g.l, Now: g.cu.Now, Log: slog.New(slog.DiscardHandler)}
	f.v = &Verifier{
		Ledger: g.l, Client: f.vs.client(t, &g.now), Soak: soak, Now: g.cu.Now,
		Firing:        func(_ context.Context, _, _ string) (bool, error) { return f.firing, nil },
		Reinvestigate: func(context.Context, Record) (Findings, error) { return Findings{Text: "t", Cause: "the cause"}, nil },
	}
	return f
}

// detect is one notification of the still-firing alert: code is what the factory would answer.
func (f *flowRig) detect(title string, code int, room string) providers.Ref {
	f.t.Helper()
	f.factory.script, f.factory.newRoom = append(f.factory.script, code), room
	inv := incident("k1")
	inv.Title = title
	inv.RootCauses = []providers.Hypothesis{{Summary: "s", Confidence: 0.9, Evidence: []string{"e"}, SuggestedAction: "a"}}
	ref, err := f.g.cu.Curate(ctx, inv)
	must(f.t, err)
	return ref
}

func (f *flowRig) event(body map[string]any) {
	f.t.Helper()
	raw, err := json.Marshal(body)
	must(f.t, err)
	f.sent++
	r := httptest.NewRequest(http.MethodPost, "/factory/webhook", bytes.NewReader(raw))
	for k, v := range sign(f.rc.Keys()[0], fmt.Sprintf("e%d", f.sent), f.g.now, raw) {
		r.Header[k] = v
	}
	w := httptest.NewRecorder()
	f.rc.ServeHTTP(w, r)
	if w.Code != http.StatusNoContent {
		f.t.Fatalf("%v: %d", body, w.Code)
	}
}

func (f *flowRig) record() Record {
	f.t.Helper()
	return f.g.record(f.t)
}

// publishAndMerge binds fix to origin and merges it, a repeat of the alert landing on the way.
func (f *flowRig) publishAndMerge(fix, origin string, pr int) {
	f.t.Helper()
	f.event(map[string]any{"type": "room.task_received", "room": fix, "origin": origin, "repo": fixRepo})
	f.event(map[string]any{"type": "room.pr_opened", "room": fix, "origin": origin, "pr": pr})
	f.quietRepeat("before the merge")
	f.event(map[string]any{"type": "room.merged", "room": fix, "pr": pr})
	f.v.OnMerged(ctx, "k1")
	if r := f.record(); r.SoakUntil.IsZero() {
		f.t.Fatalf("no soak: %+v", r)
	}
}

// quietRepeat is a re-detection that must reach neither the factory nor the record.
func (f *flowRig) quietRepeat(when string) {
	f.t.Helper()
	before := f.record()
	filed := len(f.factory.keys)
	f.detect("a reworded title, "+when, 201, "wrongroom")
	f.factory.script = nil
	if len(f.factory.keys) != filed {
		f.t.Fatalf("%s: a repeat filed a task", when)
	}
	after := f.record()
	if after.Room != before.Room || after.FixRoom != before.FixRoom || !after.SoakUntil.Equal(before.SoakUntil) || !after.MergedAt.Equal(before.MergedAt) {
		f.t.Fatalf("%s: a repeat moved the record: %+v", when, after)
	}
}

// soakEnds runs the verifier past the soak, with a repeat inside it.
func (f *flowRig) soakEnds() {
	f.t.Helper()
	f.g.now = f.g.now.Add(soak / 2)
	f.quietRepeat("inside the soak")
	f.v.Once(ctx)
	if r := f.record(); r.Verdict != "" {
		f.t.Fatalf("a verdict before the soak's end: %+v", r)
	}
	f.g.now = f.g.now.Add(soak)
	f.vs.script = append(f.vs.script, 201)
	f.v.Once(ctx)
}

// C1: alert, 201, publish, merge, soak with a repeat inside, verdict, monitor verdict, one draft.
func TestFlowAVerifiedFixDraftsOneCompleteEntry(t *testing.T) {
	f := newFlowRig(t)
	f.detect("web crash loops", 201, "room0001")
	f.g.cu.Once(ctx)
	f.quietRepeat("before the publish")

	f.publishAndMerge("fix00001", "room0001", 2638)
	f.soakEnds()
	if r := f.record(); r.Verdict != "resolved" || r.Evidence == "" {
		t.Fatalf("the soak's verdict: %+v", r)
	}
	f.g.cu.Once(ctx)
	if len(f.forge.opened) != 0 {
		t.Fatal("no draft before the monitor")
	}
	f.event(map[string]any{"type": "room.monitor_verdict", "room": "fix00001", "verdict": "healthy", "pr": 2638})
	f.g.cu.Once(ctx)
	f.g.cu.Once(ctx)

	if len(f.forge.opened) != 1 {
		t.Fatalf("%d PRs", len(f.forge.opened))
	}
	e := f.forge.opened[0]
	if e.Resolution == nil || e.Resolution.FixPR != fix2638 || e.Resolution.MergeSHA != "3f5a133c" ||
		!strings.Contains(e.Body, "**Applied:** [#2638]") || !strings.Contains(e.Body, "**Soak evidence:** firing at merge") {
		t.Fatalf("%+v\n%s", e.Resolution, e.Body)
	}
	if e.Title != "web crash loops" {
		t.Fatalf("a repeat that filed nothing is no generation: %q", e.Title)
	}
	if len(f.factory.keys) != 1 {
		t.Fatalf("%d tasks", len(f.factory.keys))
	}
	r := f.record()
	if !f.outcomes.Verifications()[r.Curation.Fingerprint] || r.Curation.Entry == "" || r.Curation.Investigation != nil {
		t.Fatalf("%+v", r.Curation)
	}
}

// A persisting generation with a repeat inside its soak, then a verified one: one entry with both.
func TestFlowAPersistingThenAVerifiedGenerationIsOneEntry(t *testing.T) {
	f := newFlowRig(t)
	f.detect("web crash loops", 201, "room0001")
	f.publishAndMerge("fix00001", "room0001", 2639)
	f.firing = true
	f.soakEnds()
	if r := f.record(); r.Verdict != "persisting" {
		t.Fatalf("persisting is recorded: %+v", r)
	}
	f.g.cu.Once(ctx)
	if len(f.forge.opened) != 0 {
		t.Fatal("a failed generation alone drafts nothing")
	}

	// The generation is decided, so the next re-detection files the next one.
	f.g.now = f.g.now.Add(time.Hour)
	f.detect("a reworded title", 201, "room0002")
	if len(f.factory.keys) != 2 {
		t.Fatalf("%d tasks", len(f.factory.keys))
	}
	f.firing = false
	f.publishAndMerge("fix00002", "room0002", 2638)
	f.soakEnds()
	f.event(map[string]any{"type": "room.monitor_verdict", "room": "fix00002", "verdict": "healthy", "pr": 2638})
	f.g.cu.Once(ctx)
	f.g.cu.Once(ctx)

	if len(f.forge.opened) != 1 {
		t.Fatalf("%d PRs", len(f.forge.opened))
	}
	e := f.forge.opened[0]
	for _, want := range []string{"**Applied:** [#2638]", "- The fix [#2639](" + fix2639 + ") (merge `3f5a133c`) did not hold: the alert still fired after the soak."} {
		if !strings.Contains(e.Body, want) {
			t.Fatalf("missing %q in\n%s", want, e.Body)
		}
	}
	r := f.record()
	h := r.Curation
	if e.Fingerprint != h.Fingerprint || !f.outcomes.Verifications()[h.Fingerprint] || len(h.Generations) != 1 || h.Generations[0].Verdict != "persisting" {
		t.Fatalf("%+v", h)
	}
	counts, err := f.outcomes.OpenCounts()
	must(t, err)
	if got := counts[h.Entry]; got.Verified != 1 || got.Failed != 1 {
		t.Fatalf("%+v", got)
	}
}

// C1's rule, one row per state. fakeFactory's keys say whether anything was filed.
func TestCurateARedetectionFilesNothingWhileAGenerationIsUndecided(t *testing.T) {
	bound := func(r *Record) { r.FixRoom, r.PR = "fix1", 2638 }
	for _, tc := range []struct {
		name  string
		set   func(r *Record)
		wait  time.Duration
		files bool
		ref   string
	}{
		{name: "a stub waiting for its human", set: func(*Record) {}},
		{name: "a stub at the window", set: func(*Record) {}, wait: 168 * time.Hour, files: true},
		{name: "a fix bound", set: bound},
		{name: "a fix bound, past the window", set: bound, wait: 168 * time.Hour, files: true},
		{name: "a fix merged and soaking", set: func(r *Record) { bound(r); r.MergedAt, r.SoakUntil = time.Unix(1, 0), time.Unix(2, 0) }},
		{name: "a fix merged and soaking, past the window", set: func(r *Record) { bound(r); r.MergedAt, r.SoakUntil = time.Unix(1, 0), time.Unix(2, 0) }, wait: 168 * time.Hour},
		{name: "a fix bound on a drafted hold", set: func(r *Record) { bound(r); r.Curation.Drafted = draftURL }, ref: draftURL},
		{name: "a fix bound on a settled hold", set: func(r *Record) { bound(r); r.Curation.Drafted = "none" }},
		{name: "a task done with its PR still open", set: func(r *Record) { bound(r); r.FixEnded = "done" }},
		{name: "a verdict, resolved", set: func(r *Record) { bound(r); r.Verdict = "resolved" }, files: true},
		{name: "a verdict, persisting", set: func(r *Record) { bound(r); r.Verdict = "persisting" }, files: true},
		{name: "a refused verdict", set: func(r *Record) { bound(r); r.Outcome = "window_closed" }, files: true},
		{name: "a revert", set: func(r *Record) { bound(r); r.Reverted = true }, files: true},
		{name: "a regression", set: func(r *Record) { bound(r); r.Monitor = "regression" }, files: true},
		{name: "a healthy monitor, no soak verdict", set: func(r *Record) { bound(r); r.Monitor = "healthy" }},
		{name: "the fix failed unmerged", set: func(r *Record) { bound(r); r.FixEnded = "failed" }, files: true},
		{name: "the fix stopped unmerged", set: func(r *Record) { bound(r); r.FixEnded = "stopped" }, files: true},
		{name: "the fix done with no PR", set: func(r *Record) { r.FixRoom, r.FixEnded = "fix1", "done" }, files: true},
		{name: "ended, then merged", set: func(r *Record) { bound(r); r.FixEnded, r.MergedAt = "done", time.Unix(1, 0) }},
		{name: "an observe-only hold", set: func(r *Record) { bound(r); r.Curation.ObserveOnly = true }, files: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlowRig(t)
			f.detect("first", 201, "room0001")
			must(t, f.g.l.Upsert("k1", func(r *Record) { tc.set(r) }))
			before := f.record()
			f.g.now = f.g.now.Add(tc.wait)
			ref := f.detect("again", 200, "")
			if filed := len(f.factory.keys) == 2; filed != tc.files {
				t.Fatalf("filed %v", filed)
			}
			if tc.files {
				return
			}
			if ref.URL != tc.ref {
				t.Fatalf("%+v", ref)
			}
			if after := f.record(); snapshotTitle(t, after.Curation) != snapshotTitle(t, before.Curation) {
				t.Fatalf("no generation, so no new snapshot: %+v", after)
			}
		})
	}
}

func TestCurateARecallFilesNothingWhileAGenerationIsUndecided(t *testing.T) {
	f := newFlowRig(t)
	f.detect("first", 201, "room0001")
	must(t, f.g.l.Upsert("k1", func(r *Record) { r.FixRoom, r.Curation.Drafted = "fix1", draftURL }))
	ref, err := f.g.cu.Curate(ctx, recall("k1", "incidents/web.md"))
	must(t, err)
	if ref.URL != "" || len(f.factory.keys) != 1 {
		t.Fatalf("a recall has an empty Ref: %+v, %d tasks", ref, len(f.factory.keys))
	}
}

func TestReceiverRecordsHowTheFixEnded(t *testing.T) {
	f := newFlowRig(t)
	f.detect("first", 201, "room0001")
	f.event(map[string]any{"type": "room.ended", "room": "room0001", "state": "done"})
	if r := f.record(); r.FixEnded != "" {
		t.Fatalf("the triager's end is not the fix's: %+v", r)
	}
	f.event(map[string]any{"type": "room.task_received", "room": "fix00001", "origin": "room0001"})
	f.event(map[string]any{"type": "room.ended", "room": "fix00001", "state": "failed"})
	if r := f.record(); r.FixEnded != "failed" {
		t.Fatalf("%+v", r)
	}
	// Released, the re-detection starts the next generation, with no end of its own.
	f.detect("again", 201, "room0002")
	if r := f.record(); r.FixEnded != "" || r.FixRoom != "" || r.Room != "room0002" {
		t.Fatalf("%+v", r)
	}
}

// I1: a hold with no live artifact must not end drafting for its key.
func TestCurateAHoldWithNoLiveArtifactStartsAgain(t *testing.T) {
	for _, tc := range []struct {
		name     string
		drafted  string
		entry    string
		state    string
		entryErr error
		code     int
		fresh    bool
	}{
		{name: "none, no entry", drafted: "none", code: 201, fresh: true},
		{name: "none, no entry, a 200", drafted: "none", code: 200},
		{name: "none, a catalog entry", drafted: "none", entry: "incidents/a.md", code: 201},
		{name: "a PR closed unmerged", drafted: draftURL, entry: draftEntry, state: "closed", code: 201, fresh: true},
		{name: "a PR closed unmerged, a 200", drafted: draftURL, entry: draftEntry, state: "closed", code: 200},
		{name: "an open PR", drafted: draftURL, entry: draftEntry, state: "open", code: 201},
		{name: "a merged PR", drafted: draftURL, entry: draftEntry, state: "merged", code: 201},
		{name: "a PR that cannot be read", drafted: draftURL, entry: draftEntry, entryErr: fmt.Errorf("a blip"), code: 201},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlowRig(t)
			f.g.cu.Drafts = f.g
			f.g.entries[164] = providers.PREntry{Path: draftEntry, State: tc.state}
			f.g.entryErr = func(int) error { return tc.entryErr }
			f.detect("first", 201, "room0001")
			must(t, f.g.l.Upsert("k1", func(r *Record) {
				r.Curation.Drafted, r.Curation.Entry = tc.drafted, tc.entry
				r.Curation.Investigation = nil
				r.Curation.Generations = []Generation{
					{FixRoom: "fix0", Verdict: "persisting", Observed: "failed", Resolved: "failed"},
					{FixRoom: "fix1", Verdict: "resolved", Monitor: "healthy", Observed: "held", Resolved: "held"},
				}
			}))
			before := f.record()
			f.g.now = f.g.now.Add(time.Hour)
			ref := f.detect("second", tc.code, "room0002")
			r := f.record()
			h := r.Curation
			if !tc.fresh {
				if h.Drafted != tc.drafted || h.Entry != tc.entry || !h.AcceptedAt.Equal(before.Curation.AcceptedAt) || h.Investigation != nil {
					t.Fatalf("frozen: %+v", h)
				}
				return
			}
			if ref.URL != "" || h.Drafted != "" || h.Entry != "" || h.Fingerprint != before.Curation.Fingerprint || !h.AcceptedAt.Equal(f.g.now) || snapshotTitle(t, h) != "second" {
				t.Fatalf("ref %+v, %+v", ref, h)
			}
			if len(h.Generations) != 1 || h.Generations[0].FixRoom != "fix0" || h.Generations[0].Observed != "" || h.Generations[0].Resolved != "" {
				t.Fatalf("only the failed fix goes on, to be credited on the new entry: %+v", h.Generations)
			}
		})
	}
}

// A drafted hold with a live PR and no bound fix is one entry waiting for its human: a repeat files
// no triager and no stub, until someone acts on the PR: a merge files again, as a close does.
func TestCurateADraftedEntryWithALivePRAndNoFixFilesNothing(t *testing.T) {
	for _, tc := range []struct {
		name     string
		state    string
		entryErr error
		fixRoom  string
		filed    bool
	}{
		{name: "an open PR, no fix", state: "open"},
		{name: "a merged PR, no fix files as before", state: "merged", filed: true},
		{name: "a PR that cannot be read, no fix", entryErr: fmt.Errorf("a blip")},
		{name: "a PR closed unmerged restarts", state: "closed", filed: true},
		{name: "an open PR, a bound decided fix", state: "open", fixRoom: "fix1", filed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFlowRig(t)
			f.g.cu.Drafts = f.g
			f.g.entries[164] = providers.PREntry{Path: draftEntry, State: tc.state}
			f.g.entryErr = func(int) error { return tc.entryErr }
			f.detect("first", 201, "room0001")
			must(t, f.g.l.Upsert("k1", func(r *Record) {
				r.Curation.Drafted, r.Curation.Entry, r.Curation.Investigation = draftURL, draftEntry, nil
				if tc.fixRoom != "" {
					r.FixRoom, r.Verdict = tc.fixRoom, "resolved"
				}
			}))
			f.g.now = f.g.now.Add(30 * 24 * time.Hour) // past the window
			filed := len(f.factory.keys)
			ref := f.detect("second", 201, "room0002")
			if got := len(f.factory.keys) != filed; got != tc.filed {
				t.Fatalf("filed %v, want %v", got, tc.filed)
			}
			if !tc.filed && (ref.URL != draftURL || f.record().Room != "room0001") {
				t.Fatalf("ref %+v, %+v", ref, f.record())
			}
		})
	}
}

// A merged entry is acted on: a recurrence months later reaches the factory, and its verified fix edits
// the merged entry through an edit PR instead of opening a second PR for the key.
func TestCurateARecurrenceAfterAMergedEntryFilesAndItsFixEditsTheEntry(t *testing.T) {
	f := newFlowRig(t)
	f.g.cu.Drafts = f.g
	f.g.entries[164] = providers.PREntry{Path: draftEntry, State: "merged"}
	f.detect("first", 201, "room0001")
	must(t, f.g.l.Upsert("k1", func(r *Record) {
		r.Curation.Drafted, r.Curation.Entry, r.Curation.Investigation = draftURL, draftEntry, nil
	}))
	f.g.catalog[f.record().Curation.Fingerprint] = draftEntry
	f.g.now = f.g.now.Add(30 * 24 * time.Hour)
	f.detect("months later", 201, "room0002")
	if r := f.record(); len(f.factory.keys) != 2 || r.Room != "room0002" {
		t.Fatalf("the recurrence reaches the factory: %d tasks, %+v", len(f.factory.keys), r)
	}
	f.event(map[string]any{"type": "room.task_received", "room": "fix00002", "origin": "room0002", "repo": fixRepo})
	f.event(map[string]any{"type": "room.pr_opened", "room": "fix00002", "origin": "room0002", "pr": 2638})
	f.event(map[string]any{"type": "room.merged", "room": "fix00002", "pr": 2638})
	f.v.OnMerged(ctx, "k1")
	f.soakEnds()
	f.event(map[string]any{"type": "room.monitor_verdict", "room": "fix00002", "verdict": "healthy", "pr": 2638})
	f.g.cu.Once(ctx)
	f.g.cu.Once(ctx)
	if len(f.forge.opened) != 0 || len(f.g.inner.invs) != 0 {
		t.Fatalf("no second PR for the key: %d opened, %d drafted", len(f.forge.opened), len(f.g.inner.invs))
	}
	if len(f.g.resolves) != 1 || f.g.resolves[0].res.FixPR != fix2638 {
		t.Fatalf("the verified fix goes through an edit PR: %+v", f.g.resolves)
	}
}

// I2: a recall's held fix must not release a later investigation's draft naming it as Applied.
func TestCurateThenOnceARecallsHeldGenerationReleasesNoDraft(t *testing.T) {
	f := newFlowRig(t)
	f.factory.script, f.factory.newRoom = []int{201}, "room0001"
	_, err := f.g.cu.Curate(ctx, recall("k1", "incidents/web.md"))
	must(t, err)
	must(t, f.g.l.Upsert("k1", func(r *Record) {
		r.Curation.Generations = []Generation{{FixRoom: "fix0", PR: 2639, Repo: fixRepo, Verdict: "persisting", Observed: "failed"}}
		r.FixRoom, r.PR, r.Repo, r.Verdict, r.Monitor, r.MonitorAt = "fix1", 2638, fixRepo, "resolved", "healthy", f.g.now
	}))
	f.g.cu.Once(ctx) // observed under the recall
	f.detect("the recall did not fire", 201, "room0002")
	f.g.cu.Once(ctx)
	f.g.cu.Once(ctx)
	r := f.record()
	if len(f.forge.opened) != 0 || r.Curation.Drafted != "" {
		t.Fatalf("%+v, %+v", f.forge.opened, r.Curation)
	}
	if gens := r.Curation.Generations; len(gens) != 1 || gens[0].FixRoom != "fix0" {
		t.Fatalf("the failed one stays: %+v", gens)
	}
}

func TestOnceClearsTheSnapshotOnceDrafted(t *testing.T) {
	for name, set := range map[string]func(g *onceRig){
		"a draft":       func(*onceRig) {},
		"an empty ref":  func(g *onceRig) { g.inner.ref = providers.Ref{} },
		"a catalog hit": func(g *onceRig) { g.catalog[holdFP] = "incidents/a.md" },
		"an adoption": func(g *onceRig) {
			g.open = []providers.CuratedIssue{{Number: 164, URL: draftURL, Body: providers.FingerprintMarker(holdFP)}}
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newOnceRig(t)
			set(g)
			g.held(t, g.verified)
			g.cu.Once(ctx)
			if h := g.record(t).Curation; h.Drafted == "" || h.Investigation != nil {
				t.Fatalf("%+v", h)
			}
		})
	}
}
