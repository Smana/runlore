// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/kbresolve"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

const (
	draftURL   = "https://github.com/o/kb/pull/164"
	draftEntry = "incidents/web-abcd1234.md"
	fixRepo    = "Smana/cloud-native-ref"
	fix2638    = "https://github.com/Smana/cloud-native-ref/pull/2638"
	fix2639    = "https://github.com/Smana/cloud-native-ref/pull/2639"
	holdFP     = "frozenfp"
	whyPersist = "the alert still fired after the soak"
)

type resolveCall struct {
	target kbresolve.Target
	res    providers.AppliedResolution
}

// onceRig is a Curation over a temp ledger whose every collaborator is a recording fake. calls is
// the order things happened in.
type onceRig struct {
	cu    *Curation
	inner *recordingInner
	l     *Ledger
	now   time.Time
	logs  bytes.Buffer

	calls      []string
	verifies   []outcome.Verification
	links      []string
	resolves   []resolveCall
	promotes   []int
	pulled     []string
	open       []providers.CuratedIssue
	listErr    error
	entries    map[int]providers.PREntry
	entryErr   func(number int) error
	pullErr    func() error
	verifyErr  error
	linkErr    error
	resolveErr error
	promoteErr error
	catalog    map[string]string
	labelled   []string
	labelErr   error
}

// recordingInner is the curator, remembering what it was asked to draft.
type recordingInner struct {
	rig  *onceRig
	invs []providers.Investigation
	ref  providers.Ref
	err  error
}

func (i *recordingInner) Curate(_ context.Context, inv providers.Investigation) (providers.Ref, error) {
	i.invs = append(i.invs, inv)
	i.rig.calls = append(i.rig.calls, "inner")
	return i.ref, i.err
}

func (i *recordingInner) MeetsBar(providers.Investigation) bool { return true }

func (g *onceRig) Verify(v outcome.Verification) error {
	if g.verifyErr != nil {
		return g.verifyErr
	}
	g.verifies = append(g.verifies, v)
	g.calls = append(g.calls, "verify")
	return nil
}

func (g *onceRig) Link(key, url string, _ time.Time) error {
	if g.linkErr != nil {
		return g.linkErr
	}
	g.links = append(g.links, key+" "+url)
	g.calls = append(g.calls, "link")
	return nil
}

func (g *onceRig) PullSummary(_ context.Context, repo string, n int) (providers.PullSummary, error) {
	g.pulled = append(g.pulled, fmt.Sprintf("%s#%d", repo, n))
	if g.pullErr != nil {
		if err := g.pullErr(); err != nil {
			return providers.PullSummary{}, err
		}
	}
	return providers.PullSummary{MergeSHA: "3f5a133c", Files: []string{"observability/rules.yaml"}, Additions: 2, Deletions: 2}, nil
}

func (g *onceRig) ListPRsByLabel(_ context.Context, label string) ([]providers.CuratedIssue, error) {
	if label != "runlore" {
		return nil, fmt.Errorf("label %q", label)
	}
	return g.open, g.listErr
}

func (g *onceRig) PREntry(_ context.Context, n int) (providers.PREntry, error) {
	if g.entryErr != nil {
		if err := g.entryErr(n); err != nil {
			return providers.PREntry{}, err
		}
	}
	if e, ok := g.entries[n]; ok {
		return e, nil
	}
	return providers.PREntry{}, fmt.Errorf("no PR %d", n)
}

func (g *onceRig) ReplaceLabel(_ context.Context, n int, remove, add string) error {
	if g.labelErr != nil {
		return g.labelErr
	}
	g.labelled = append(g.labelled, fmt.Sprintf("%d -%s +%s", n, remove, add))
	return nil
}

func newOnceRig(t *testing.T) *onceRig {
	t.Helper()
	g := &onceRig{now: time.Unix(1_800_000_000, 0), catalog: map[string]string{}, entries: map[int]providers.PREntry{164: {Path: draftEntry, State: "open"}}}
	clock := func() time.Time { return g.now }
	l, err := OpenLedger(filepath.Join(t.TempDir(), "f.jsonl"), clock)
	must(t, err)
	g.l = l
	g.inner = &recordingInner{rig: g, ref: providers.Ref{URL: draftURL}}
	g.cu = &Curation{
		Inner: g.inner, Ledger: l, Outcome: g, Pulls: g, Drafts: g, Window: 168 * time.Hour, Repository: fixRepo, Now: clock,
		Log: slog.New(slog.NewTextHandler(&g.logs, nil)),
		Resolve: func(_ context.Context, target kbresolve.Target, r providers.AppliedResolution) (kbresolve.Result, error) {
			if g.resolveErr != nil {
				return kbresolve.Result{}, g.resolveErr
			}
			g.resolves = append(g.resolves, resolveCall{target, r})
			g.calls = append(g.calls, "resolve")
			return kbresolve.Result{Route: "appended"}, nil
		},
		FindEntry: func(fp string) (string, bool) {
			path, ok := g.catalog[fp]
			return path, ok
		},
		Promote: func(_ context.Context, n int) error {
			if g.promoteErr != nil {
				return g.promoteErr
			}
			g.promotes = append(g.promotes, n)
			g.calls = append(g.calls, "promote")
			return nil
		},
	}
	return g
}

// heldAt seeds k1's hold, accepted at the rig's clock, and whatever set adds to the record.
func (g *onceRig) held(t *testing.T, set func(r *Record)) {
	t.Helper()
	snapshot, err := json.Marshal(incident("k1"))
	must(t, err)
	must(t, g.l.Upsert("k1", func(r *Record) {
		r.Room = "room0001"
		r.Curation = &Hold{AcceptedAt: g.now, Investigation: snapshot, Fingerprint: holdFP}
		if set != nil {
			set(r)
		}
	}))
}

func (g *onceRig) record(t *testing.T) Record {
	t.Helper()
	r, ok := g.l.Get("k1")
	if !ok {
		t.Fatal("no record")
	}
	return r
}

func (g *onceRig) monitorAt() time.Time { return g.now.Add(-time.Hour) }

// verified makes r's current generation a verified fix, PR 2638 in room fix1.
func (g *onceRig) verified(r *Record) {
	r.FixRoom, r.PR, r.Repo = "fix1", 2638, fixRepo
	r.Verdict, r.Monitor, r.MonitorAt, r.Evidence = "resolved", "healthy", g.monitorAt(), "the alert cleared"
}

func (g *onceRig) failedGeneration() Generation {
	return Generation{FixRoom: "fix0", Repo: fixRepo, PR: 2639, Verdict: "persisting", Monitor: "regression", MonitorAt: g.monitorAt(), Evidence: "still firing"}
}

func (g *onceRig) resolvedRes() providers.AppliedResolution {
	return providers.AppliedResolution{
		FixPR: fix2638, MergeSHA: "3f5a133c", Verdict: "resolved", Diff: "1 file: observability/rules.yaml (+2 −2)",
		Evidence: "the alert cleared", VerifiedAt: g.monitorAt(),
	}
}

func TestCurationOnceDraftsOnAVerifiedGeneration(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, g.verified)
	g.cu.Once(ctx)

	if len(g.inner.invs) != 1 {
		t.Fatalf("inner %d", len(g.inner.invs))
	}
	inv := g.inner.invs[0]
	if inv.PinnedFingerprint != holdFP || !reflect.DeepEqual(inv.Resolutions, []providers.AppliedResolution{g.resolvedRes()}) {
		t.Fatalf("%q %+v", inv.PinnedFingerprint, inv.Resolutions)
	}
	r := g.record(t)
	h := r.Curation
	if h.Drafted != draftURL || h.Entry != draftEntry || !h.DraftedAt.Equal(g.now) {
		t.Fatalf("%+v", h)
	}
	if r.Observed != "held" || r.Resolved != "held" {
		t.Fatalf("observed %q, resolved %q", r.Observed, r.Resolved)
	}
	want := outcome.Verification{Entry: draftEntry, DupFingerprint: holdFP, TriggerKey: "k1", Room: "fix1", Held: true, At: g.now}
	if !reflect.DeepEqual(g.verifies, []outcome.Verification{want}) {
		t.Fatalf("%+v", g.verifies)
	}
	if !slices.Equal(g.links, []string{"k1 " + draftURL}) || !slices.Equal(g.promotes, []int{164}) || len(g.resolves) != 0 {
		t.Fatalf("links %v, promotes %v, resolves %+v", g.links, g.promotes, g.resolves)
	}
	if !slices.Equal(g.calls, []string{"inner", "link", "verify", "promote"}) {
		t.Fatalf("the order: %v", g.calls)
	}
	if h.PromoteDue || h.LinkDue {
		t.Fatalf("%+v", h)
	}

	g.calls = nil
	g.pulled = nil
	g.cu.Once(ctx)
	if len(g.calls) != 0 || len(g.pulled) != 0 {
		t.Fatalf("a second tick calls nothing: %v, %v", g.calls, g.pulled)
	}
}

func TestCurationOnceAFailedThenAVerifiedGeneration(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, func(r *Record) {
		r.Curation.Generations = []Generation{g.failedGeneration()}
		g.verified(r)
	})
	g.cu.Once(ctx)
	if len(g.inner.invs) != 1 {
		t.Fatalf("inner %d", len(g.inner.invs))
	}
	failed := providers.AppliedResolution{
		FixPR: fix2639, MergeSHA: "3f5a133c", Verdict: "persisting", Why: whyPersist,
		Diff: "1 file: observability/rules.yaml (+2 −2)", Evidence: "still firing", VerifiedAt: g.monitorAt(),
	}
	if got := g.inner.invs[0].Resolutions; !reflect.DeepEqual(got, []providers.AppliedResolution{failed, g.resolvedRes()}) {
		t.Fatalf("%+v", got)
	}
	if len(g.verifies) != 2 || g.verifies[0].Room != "fix0" || g.verifies[0].Held || g.verifies[1].Room != "fix1" || !g.verifies[1].Held ||
		g.verifies[0].Entry != draftEntry || g.verifies[1].Entry != draftEntry {
		t.Fatalf("%+v", g.verifies)
	}
	r := g.record(t)
	if gen := r.Curation.Generations[0]; gen.Observed != "failed" || gen.Resolved != "failed" || r.Resolved != "held" || len(g.resolves) != 0 {
		t.Fatalf("%+v, %+v", gen, g.resolves)
	}
}

// entryForge is the knowledge base behind a real curator.
type entryForge struct {
	opened []providers.KBEntry
	open   []providers.CuratedIssue
}

func (f *entryForge) OpenPR(_ context.Context, e providers.KBEntry) (providers.Ref, error) {
	f.opened = append(f.opened, e)
	n := 163 + len(f.opened)
	url := "https://github.com/o/kb/pull/" + strconv.Itoa(n)
	f.open = append(f.open, providers.CuratedIssue{Number: n, URL: url, Body: providers.FingerprintMarker(e.Fingerprint)})
	return providers.Ref{URL: url}, nil
}

func (f *entryForge) ListPRsByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return f.open, nil
}

func (f *entryForge) CommentOnPR(context.Context, int, string) error { return nil }

func (f *entryForge) ReplaceLabel(context.Context, int, string, string) error { return nil }

func (f *entryForge) PREntry(_ context.Context, n int) (providers.PREntry, error) {
	return providers.PREntry{Path: fmt.Sprintf("incidents/pr-%d.md", n), State: "open"}, nil
}

// Review Focus 6: a new title and resource must not move the credit, the marker or the PR.
func TestCurationOnceIdentityStaysAcrossADriftedGeneration(t *testing.T) {
	g := newOnceRig(t)
	outcomes, err := outcome.New(filepath.Join(t.TempDir(), "outcome.jsonl"))
	must(t, err)
	forge := &entryForge{}
	n, _, l, _ := newTestNotifier(t, []int{201})
	strong := []providers.Hypothesis{{Summary: "s", Confidence: 0.9, Evidence: []string{"e"}, SuggestedAction: "a"}}
	cu := g.cu
	cu.Filer, cu.Ledger, cu.Outcome, cu.Drafts = n, l, outcomes, forge
	cu.Inner = &curator.Curator{Forge: forge, MinConfidence: 0.7, Log: slog.New(slog.DiscardHandler)}

	first := incident("k1")
	first.RootCauses = strong
	_, err = cu.Curate(ctx, first)
	must(t, err)
	r, _ := l.Get("k1")
	frozen := r.Curation.Fingerprint
	must(t, l.Upsert("k1", func(r *Record) {
		r.FixRoom, r.PR, r.Repo, r.Verdict, r.Monitor, r.MonitorAt = "fix1", 2639, fixRepo, "persisting", "regression", g.monitorAt()
	}))

	cu.Filer.Client = clientAnswering(t, 201, "room0002")
	second := incident("k1")
	second.RootCauses = strong
	second.Title, second.Resource.Name = "a reworded title", "another-workload"
	_, err = cu.Curate(ctx, second)
	must(t, err)
	if curator.DupFingerprint(second) == frozen {
		t.Fatal("the test needs a fingerprint that drifted")
	}
	must(t, l.Upsert("k1", func(r *Record) {
		r.FixRoom, r.PR, r.Repo, r.Verdict, r.Monitor, r.MonitorAt = "fix2", 2638, fixRepo, "resolved", "healthy", g.monitorAt()
	}))

	cu.Once(ctx)
	cu.Once(ctx)

	if len(forge.opened) != 1 || forge.opened[0].Fingerprint != frozen || forge.opened[0].Title != "a reworded title" {
		t.Fatalf("%+v", forge.opened)
	}
	if got := providers.ParseFingerprintMarker(forge.open[0].Body); got != frozen {
		t.Fatalf("the marker: %q", got)
	}
	// All within one tick and one clock reading: the failure must not win the tie.
	if !outcomes.Verifications()[frozen] {
		t.Fatalf("the verified generation is the latest: %+v", outcomes.Verifications())
	}
	counts, err := outcomes.OpenCounts()
	must(t, err)
	if got := counts["incidents/pr-164.md"]; got.Verified != 1 || got.Failed != 1 {
		t.Fatalf("both credit one entry: %+v", counts)
	}
	if forge.opened[0].Resolution == nil || forge.opened[0].Resolution.FixPR != fix2638 {
		t.Fatalf("%+v", forge.opened[0].Resolution)
	}
}

func TestCurationOncePendingGenerationsWait(t *testing.T) {
	for name, set := range map[string]func(r *Record){
		"healthy before the soak's verdict": func(r *Record) { r.FixRoom, r.PR, r.Monitor = "fix1", 2638, "healthy" },
		"resolved and inconclusive":         func(r *Record) { r.FixRoom, r.PR, r.Verdict, r.Monitor = "fix1", 2638, "resolved", "inconclusive" },
		"failed alone, within the window":   func(r *Record) { r.FixRoom, r.PR, r.Verdict = "fix1", 2638, "persisting" },
	} {
		t.Run(name, func(t *testing.T) {
			g := newOnceRig(t)
			g.held(t, set)
			g.cu.Once(ctx)
			if len(g.calls) != 0 || g.record(t).Curation.Drafted != "" {
				t.Fatalf("%v, %+v", g.calls, g.record(t).Curation)
			}
		})
	}
}

func TestCurationOnceTheSoakVerdictReleasesTheDraft(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, func(r *Record) { r.FixRoom, r.PR, r.Monitor, r.MonitorAt = "fix1", 2638, "healthy", g.monitorAt() })
	g.cu.Once(ctx)
	if len(g.inner.invs) != 0 {
		t.Fatal("nothing until the verdict")
	}
	must(t, g.l.Upsert("k1", func(r *Record) { r.Verdict = "resolved" }))
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	if len(g.inner.invs) != 1 || len(g.verifies) != 1 {
		t.Fatalf("inner %d, verifies %d", len(g.inner.invs), len(g.verifies))
	}
}

func TestCurationOnceTheWindowDraftsTodaysEntry(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, nil)
	g.now = g.now.Add(168*time.Hour - time.Second)
	g.cu.Once(ctx)
	if len(g.inner.invs) != 0 {
		t.Fatal("a second before the window")
	}
	g.now = g.now.Add(time.Second)
	g.cu.Once(ctx)
	if len(g.inner.invs) != 1 || len(g.inner.invs[0].Resolutions) != 0 || g.inner.invs[0].PinnedFingerprint != holdFP {
		t.Fatalf("%+v", g.inner.invs)
	}
	if !slices.Equal(g.promotes, []int{164}) || len(g.verifies) != 0 {
		t.Fatalf("promotes %v, verifies %v", g.promotes, g.verifies)
	}
}

func TestCurationOnceTheWindowCarriesOnlyTheFailedGenerations(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, func(r *Record) {
		r.Curation.Generations = []Generation{g.failedGeneration()}
		r.FixRoom, r.PR, r.Verdict, r.Monitor = "fix1", 2638, "resolved", "inconclusive"
	})
	g.now = g.now.Add(168 * time.Hour)
	g.cu.Once(ctx)
	got := g.inner.invs[0].Resolutions
	if len(got) != 1 || got[0].Verdict != "persisting" || got[0].FixPR != fix2639 {
		t.Fatalf("%+v", got)
	}
}

func TestCurationOnceAnEmptyRefIsTerminal(t *testing.T) {
	g := newOnceRig(t)
	g.inner.ref = providers.Ref{}
	g.held(t, g.verified)
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	h := g.record(t).Curation
	if h.Drafted != "none" || h.Entry != "" || len(g.inner.invs) != 1 || len(g.verifies) != 0 || len(g.resolves) != 0 || len(g.links) != 0 || len(g.promotes) != 0 {
		t.Fatalf("%+v, inner %d, %v", h, len(g.inner.invs), g.calls)
	}
}

func TestCurationOnceAnInnerErrorRetries(t *testing.T) {
	g := newOnceRig(t)
	g.inner.err = errors.New("forge down")
	g.inner.ref = providers.Ref{}
	g.held(t, g.verified)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; h.Drafted != "" || len(g.verifies) != 0 {
		t.Fatalf("%+v", h)
	}
	g.inner.err, g.inner.ref = nil, providers.Ref{URL: draftURL}
	g.cu.Once(ctx)
	if h := g.record(t).Curation; h.Drafted != draftURL || len(g.inner.invs) != 2 {
		t.Fatalf("%+v", h)
	}
}

func TestCurationOnceACatalogEntryIsNotDraftedAgain(t *testing.T) {
	g := newOnceRig(t)
	g.catalog[holdFP] = "incidents/a.md"
	g.held(t, g.verified)
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	h := g.record(t).Curation
	if len(g.inner.invs) != 0 || h.Drafted != "none" || h.Entry != "incidents/a.md" {
		t.Fatalf("inner %d, %+v", len(g.inner.invs), h)
	}
	if len(g.resolves) != 1 || g.resolves[0].target != (kbresolve.Target{Path: "incidents/a.md"}) || g.resolves[0].res.Verdict != "resolved" {
		t.Fatalf("%+v", g.resolves)
	}
	if len(g.verifies) != 1 || g.verifies[0].Entry != "incidents/a.md" || len(g.links) != 0 || len(g.promotes) != 0 {
		t.Fatalf("verifies %+v, links %v, promotes %v", g.verifies, g.links, g.promotes)
	}
}

// Review Focus 2: the draft landed, its Drafted write did not.
func TestCurationOnceAdoptsAnOpenPR(t *testing.T) {
	g := newOnceRig(t)
	g.open = []providers.CuratedIssue{
		{Number: 9, URL: "https://github.com/o/kb/pull/9", Body: "an entry with no marker"},
		{Number: 164, URL: draftURL, Body: providers.FingerprintMarker(holdFP)},
	}
	g.held(t, func(r *Record) {
		r.Curation.Generations = []Generation{g.failedGeneration()}
		g.verified(r)
		other := incident("k1")
		other.Resource.Name = "drifted"
		snapshot, err := json.Marshal(other)
		must(t, err)
		r.Curation.Investigation = snapshot
	})
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	h := g.record(t).Curation
	if len(g.inner.invs) != 0 || h.Drafted != draftURL || h.Entry != draftEntry {
		t.Fatalf("inner %d, %+v", len(g.inner.invs), h)
	}
	if len(g.resolves) != 2 || g.resolves[0].target != (kbresolve.Target{PR: 164}) || g.resolves[1].target != (kbresolve.Target{PR: 164}) ||
		g.resolves[0].res.Verdict != "persisting" || g.resolves[1].res.Verdict != "resolved" {
		t.Fatalf("%+v", g.resolves)
	}
	if !slices.Equal(g.links, []string{"k1 " + draftURL}) || !slices.Equal(g.promotes, []int{164}) || len(g.verifies) != 2 {
		t.Fatalf("links %v, promotes %v, verifies %+v", g.links, g.promotes, g.verifies)
	}
}

func TestCurationOnceAPRWithoutTheMarkerIsNotAdoptedByAnEmptyFingerprint(t *testing.T) {
	g := newOnceRig(t)
	g.open = []providers.CuratedIssue{{Number: 9, URL: "https://github.com/o/kb/pull/9", Body: "no marker"}}
	g.held(t, func(r *Record) {
		g.verified(r)
		r.Curation.Fingerprint = ""
	})
	g.cu.Once(ctx)
	if len(g.inner.invs) != 1 || g.record(t).Curation.Drafted != draftURL {
		t.Fatalf("inner %d, %+v", len(g.inner.invs), g.record(t).Curation)
	}
}

func TestCurationOnceAPRThatIsNotRunLoresIsNotAdopted(t *testing.T) {
	g := newOnceRig(t)
	g.open = []providers.CuratedIssue{{Number: 9, URL: "https://github.com/o/kb/pull/9", Body: providers.FingerprintMarker(holdFP)}}
	g.entryErr = func(n int) error {
		if n == 9 {
			return providers.ErrNotRunLorePR
		}
		return nil
	}
	g.held(t, g.verified)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; len(g.inner.invs) != 1 || h.Drafted != draftURL {
		t.Fatalf("inner %d, %+v", len(g.inner.invs), h)
	}
}

func TestCurationOnceAFailedListingDraftsNothing(t *testing.T) {
	g := newOnceRig(t)
	g.listErr = errors.New("forge down")
	g.held(t, g.verified)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; len(g.inner.invs) != 0 || h.Drafted != "" {
		t.Fatalf("a listing that failed cannot rule a PR out: inner %d, %+v", len(g.inner.invs), h)
	}
}

// I1: one outage fails the swallowed label and PREntry alike, and an unlabelled PR is in no listing.
// The URL is written at once, the entry is read later, and the PR is labelled again.
func TestCurationOnceALabelAndPREntryOutageOpensNoSecondPR(t *testing.T) {
	g := newOnceRig(t)
	g.open = nil // unlabelled: no listing finds it
	g.entryErr = func(int) error { return errors.New("github is down") }
	g.held(t, g.verified)
	g.cu.Once(ctx)
	h := g.record(t).Curation
	if h.Drafted != draftURL || h.Entry != "" || !h.EntryDue || len(g.verifies) != 0 || len(g.promotes) != 0 {
		t.Fatalf("%+v, verifies %+v", h, g.verifies)
	}
	if r := g.record(t); r.Observed != "" {
		t.Fatalf("nothing is observed before the entry is known: %+v", r)
	}

	// GitHub is back, and the guard refuses the PR until it carries the label.
	g.entryErr = func(int) error {
		if len(g.labelled) == 0 {
			return fmt.Errorf("PR: %w", providers.ErrNotRunLorePR)
		}
		return nil
	}
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	r := g.record(t)
	h = r.Curation
	if len(g.inner.invs) != 1 || h.Drafted != draftURL || h.Entry != draftEntry || h.EntryDue {
		t.Fatalf("inner %d, %+v", len(g.inner.invs), h)
	}
	if !slices.Equal(g.labelled, []string{"164 - +runlore"}) {
		t.Fatalf("%v", g.labelled)
	}
	if len(g.verifies) != 1 || g.verifies[0].Entry != draftEntry || r.Observed != "held" || !slices.Equal(g.promotes, []int{164}) {
		t.Fatalf("verifies %+v, promotes %v, %+v", g.verifies, g.promotes, r)
	}
	if !slices.Equal(g.links, []string{"k1 " + draftURL}) {
		t.Fatalf("%v", g.links)
	}
}

func TestCurationOnceAnEntryThatNeverReadsStopsAfterThirtyFailuresPastTheWindow(t *testing.T) {
	g := newOnceRig(t)
	calls := 0
	g.entryErr = func(int) error {
		calls++
		return errors.New("two entries in one PR")
	}
	g.held(t, g.verified)
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; !h.EntryDue || calls != 2 || len(h.PastWindowFailures) != 0 {
		t.Fatalf("calls %d, %+v", calls, h)
	}
	g.now = g.now.Add(168 * time.Hour)
	g.logs.Reset()
	tick(g, 29)
	if h := g.record(t).Curation; !h.EntryDue || strings.Contains(g.logs.String(), "window_passed") {
		t.Fatalf("%+v, %s", h, g.logs.String())
	}
	tick(g, 2)
	h := g.record(t).Curation
	if h.EntryDue || h.Entry != "" || h.Drafted != draftURL || calls != 2+30 || len(g.inner.invs) != 1 || len(g.verifies) != 0 {
		t.Fatalf("calls %d, inner %d, %+v", calls, len(g.inner.invs), h)
	}
	if n := strings.Count(g.logs.String(), "window_passed"); n != 1 {
		t.Fatalf("logged %d times: %s", n, g.logs.String())
	}
}

func draftedHold(r *Record) {
	r.Curation.Drafted, r.Curation.Entry = draftURL, draftEntry
}

func TestCurationOnceAfterTheDraftAVerifiedGeneration(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, func(r *Record) {
		draftedHold(r)
		g.verified(r)
	})
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	r := g.record(t)
	if len(g.inner.invs) != 0 || len(g.resolves) != 1 || g.resolves[0].target != (kbresolve.Target{PR: 164}) ||
		!reflect.DeepEqual(g.resolves[0].res, g.resolvedRes()) || r.Resolved != "held" || r.Observed != "held" {
		t.Fatalf("inner %d, %+v, %+v", len(g.inner.invs), g.resolves, r)
	}
	if len(g.verifies) != 1 || !g.verifies[0].Held || !slices.Equal(g.promotes, []int{164}) {
		t.Fatalf("%+v, %v", g.verifies, g.promotes)
	}
}

func TestCurationOnceAfterTheDraftAFailedGeneration(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, func(r *Record) {
		draftedHold(r)
		r.FixRoom, r.PR, r.Verdict, r.Repo = "fix1", 2638, "persisting", fixRepo
	})
	g.cu.Once(ctx)
	if len(g.resolves) != 1 || g.resolves[0].res.Verdict != "persisting" || g.resolves[0].res.Why != whyPersist {
		t.Fatalf("%+v", g.resolves)
	}
	if !g.resolves[0].res.VerifiedAt.Equal(g.now) {
		t.Fatalf("no monitor verdict, so verified at the observation: %v", g.resolves[0].res.VerifiedAt)
	}
	if len(g.verifies) != 1 || g.verifies[0].Held {
		t.Fatalf("%+v", g.verifies)
	}
}

func TestCurationOnceARevertFlipsAResolvedDraft(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, func(r *Record) {
		draftedHold(r)
		g.verified(r)
		r.Observed, r.Resolved = "held", "held"
	})
	g.cu.Once(ctx)
	if len(g.calls) != 0 {
		t.Fatalf("settled: %v", g.calls)
	}
	must(t, g.l.Upsert("k1", func(r *Record) { r.Reverted = true }))
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	if len(g.verifies) != 1 || g.verifies[0].Held || len(g.resolves) != 1 || g.resolves[0].res.Verdict != "persisting" ||
		g.resolves[0].res.Why != "the fix was reverted" || !slices.Equal(g.promotes, []int{164}) {
		t.Fatalf("%+v, %+v, %v", g.verifies, g.resolves, g.promotes)
	}
	if r := g.record(t); r.Observed != "failed" || r.Resolved != "failed" {
		t.Fatalf("%+v", r)
	}
}

func TestCurationOnceTheWhyOfAFailure(t *testing.T) {
	for want, gen := range map[string]Generation{
		"the alert still fired after the soak":             {Verdict: "persisting", Monitor: "regression"},
		"the factory's post-merge monitor read regression": {Verdict: "resolved", Monitor: "regression"},
		"the fix was reverted":                             {Verdict: "persisting", Monitor: "regression", Reverted: true},
	} {
		if got := gen.why(); got != want {
			t.Errorf("%+v: %q", gen, got)
		}
	}
	if got := (Generation{Verdict: "resolved", Monitor: "healthy"}).why(); got != "" {
		t.Errorf("a held fix has no why: %q", got)
	}
}

func TestCurationOnceAnObserveOnlyHoldIsOnlyObserved(t *testing.T) {
	g := newOnceRig(t)
	must(t, g.l.Upsert("k1", func(r *Record) {
		r.Curation = &Hold{ObserveOnly: true, Entry: "incidents/web.md", Drafted: "none", Fingerprint: holdFP}
		g.verified(r)
	}))
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	if len(g.verifies) != 1 || g.verifies[0].Entry != "incidents/web.md" || !g.verifies[0].Held || g.verifies[0].DupFingerprint != holdFP {
		t.Fatalf("%+v", g.verifies)
	}
	if len(g.inner.invs) != 0 || len(g.resolves) != 0 || len(g.promotes) != 0 || len(g.links) != 0 || len(g.pulled) != 0 {
		t.Fatalf("%v", g.calls)
	}
}

func TestCurationOnceATerminalResolveErrorIsNotRetried(t *testing.T) {
	for _, terminal := range []error{kbresolve.ErrClosedUnmerged, providers.ErrNotRunLorePR} {
		t.Run(terminal.Error(), func(t *testing.T) {
			g := newOnceRig(t)
			g.resolveErr = fmt.Errorf("PR #164: %w", terminal)
			g.held(t, func(r *Record) {
				draftedHold(r)
				g.verified(r)
			})
			calls := 0
			inner := g.cu.Resolve
			g.cu.Resolve = func(c context.Context, target kbresolve.Target, r providers.AppliedResolution) (kbresolve.Result, error) {
				calls++
				return inner(c, target, r)
			}
			g.cu.Once(ctx)
			g.cu.Once(ctx)
			if r := g.record(t); r.Resolved != "held" || calls != 1 {
				t.Fatalf("resolved %q, calls %d", r.Resolved, calls)
			}
			if n := strings.Count(g.logs.String(), "\n"); n != 1 {
				t.Fatalf("one log line: %s", g.logs.String())
			}
		})
	}
}

func TestCurationOnceAnotherResolveErrorRetries(t *testing.T) {
	g := newOnceRig(t)
	g.resolveErr = errors.New("a blip")
	g.held(t, func(r *Record) {
		draftedHold(r)
		g.verified(r)
	})
	g.cu.Once(ctx)
	if r := g.record(t); r.Resolved != "" || r.Observed != "held" {
		t.Fatalf("%+v", r)
	}
	g.resolveErr = nil
	g.cu.Once(ctx)
	if r := g.record(t); r.Resolved != "held" || len(g.resolves) != 1 {
		t.Fatalf("%+v", r)
	}
}

func TestCurationOnceADryRunMarksNothingResolved(t *testing.T) {
	g := newOnceRig(t)
	g.cu.DryRun = true
	g.held(t, func(r *Record) {
		draftedHold(r)
		g.verified(r)
	})
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	if r := g.record(t); r.Resolved != "" || len(g.resolves) != 0 {
		t.Fatalf("resolved %q, %+v", r.Resolved, g.resolves)
	}
}

// A dry run gates the writes that go through Resolve's guarded forge, not the curator: the draft is
// the curator's own write, as without defer.
func TestCurationOnceADryRunStillDraftsThroughTheCurator(t *testing.T) {
	g := newOnceRig(t)
	g.cu.DryRun = true
	g.held(t, g.verified)
	g.cu.Once(ctx)
	if len(g.inner.invs) != 1 {
		t.Fatalf("the curator drafts in a dry run: %d calls", len(g.inner.invs))
	}
	if h := g.record(t).Curation; h.Drafted != draftURL {
		t.Fatalf("%+v", h)
	}
	if len(g.resolves) != 0 {
		t.Fatalf("Resolve is not called in a dry run: %+v", g.resolves)
	}
}

func TestCurationOnceAFailedPromoteRetriesOnce(t *testing.T) {
	g := newOnceRig(t)
	g.promoteErr = errors.New("a blip")
	g.held(t, g.verified)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; !h.PromoteDue {
		t.Fatalf("%+v", h)
	}
	g.promoteErr = nil
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; h.PromoteDue || !slices.Equal(g.promotes, []int{164}) {
		t.Fatalf("%+v, %v", h, g.promotes)
	}
}

func TestCurationOnceANilPromoteChangesNothingElse(t *testing.T) {
	g := newOnceRig(t)
	g.cu.Promote = nil
	g.held(t, g.verified)
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	h := g.record(t).Curation
	if h.Drafted != draftURL || h.Entry != draftEntry || !h.PromoteDue || len(g.inner.invs) != 1 || len(g.verifies) != 1 {
		t.Fatalf("%+v", h)
	}
}

func TestCurationOnceAFailedLinkRetries(t *testing.T) {
	g := newOnceRig(t)
	g.linkErr = errors.New("disk full")
	g.held(t, g.verified)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; !h.LinkDue || h.Drafted != draftURL {
		t.Fatalf("%+v", h)
	}
	g.linkErr = nil
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; h.LinkDue || len(g.links) != 1 || len(g.inner.invs) != 1 {
		t.Fatalf("%+v, %v", h, g.links)
	}
}

func TestCurationOnceAFailedVerifyRetries(t *testing.T) {
	g := newOnceRig(t)
	g.verifyErr = errors.New("disk full")
	g.held(t, g.verified)
	g.cu.Once(ctx)
	if r := g.record(t); r.Observed != "" {
		t.Fatalf("%+v", r)
	}
	g.verifyErr = nil
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	if r := g.record(t); r.Observed != "held" || len(g.verifies) != 1 {
		t.Fatalf("%+v, %+v", r, g.verifies)
	}
}

func TestCurationOnceAPullBlipWaitsWithinTheWindow(t *testing.T) {
	g := newOnceRig(t)
	blip := errors.New("a blip")
	g.pullErr = func() error { return blip }
	g.held(t, g.verified)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; len(g.inner.invs) != 0 || h.Drafted != "" {
		t.Fatalf("inner %d, %+v", len(g.inner.invs), h)
	}
	g.pullErr = nil
	g.cu.Once(ctx)
	if len(g.inner.invs) != 1 || g.inner.invs[0].Resolutions[0].MergeSHA != "3f5a133c" {
		t.Fatalf("%+v", g.inner.invs)
	}
}

func TestCurationOnceDraftsWithoutTheSHA(t *testing.T) {
	for name, tc := range map[string]struct {
		err  error
		wait time.Duration
	}{
		"a gone PR":            {err: fmt.Errorf("x: %w", providers.ErrPullNotFound)},
		"a blip at the window": {err: errors.New("a blip"), wait: 168 * time.Hour},
	} {
		t.Run(name, func(t *testing.T) {
			g := newOnceRig(t)
			g.pullErr = func() error { return tc.err }
			g.held(t, g.verified)
			g.now = g.now.Add(tc.wait)
			g.cu.Once(ctx)
			if len(g.inner.invs) != 1 {
				t.Fatalf("inner %d", len(g.inner.invs))
			}
			got := g.inner.invs[0].Resolutions
			if len(got) != 1 || got[0].MergeSHA != "" || got[0].FixPR != fix2638 || got[0].Diff != "" {
				t.Fatalf("%+v", got)
			}
		})
	}
}

func TestCurationOnceAPullBlipWaitsOnTheResolvePathToo(t *testing.T) {
	g := newOnceRig(t)
	g.pullErr = func() error { return errors.New("a blip") }
	g.held(t, func(r *Record) {
		draftedHold(r)
		g.verified(r)
	})
	g.cu.Once(ctx)
	if r := g.record(t); r.Resolved != "" || len(g.resolves) != 0 || len(g.promotes) != 0 || !r.Curation.PromoteDue {
		t.Fatalf("no label on a body without the fix: %+v, promotes %v", r, g.promotes)
	}
	g.pullErr = nil
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	if len(g.resolves) != 1 || g.resolves[0].res.MergeSHA != "3f5a133c" || !slices.Equal(g.promotes, []int{164}) {
		t.Fatalf("%+v, %v", g.resolves, g.promotes)
	}
}

// I2: Promote waits for the fix to be in the body, and a label on a body without it is never laid.
func TestCurationOnceAResolveBlipHoldsThePromote(t *testing.T) {
	g := newOnceRig(t)
	g.resolveErr = errors.New("a blip")
	g.held(t, func(r *Record) {
		draftedHold(r)
		g.verified(r)
	})
	g.cu.Once(ctx)
	if h := g.record(t).Curation; len(g.promotes) != 0 || !h.PromoteDue {
		t.Fatalf("promotes %v, %+v", g.promotes, h)
	}
	g.resolveErr = nil
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; !slices.Equal(g.calls, []string{"verify", "resolve", "promote"}) || h.PromoteDue {
		t.Fatalf("%v, %+v", g.calls, h)
	}
}

// A failed generation does not hold the promote back: withdrawing the label should not wait.
func TestCurationOnceAResolveBlipOnAFailureStillWithdraws(t *testing.T) {
	g := newOnceRig(t)
	g.resolveErr = errors.New("a blip")
	g.held(t, func(r *Record) {
		draftedHold(r)
		r.FixRoom, r.PR, r.Repo, r.Verdict = "fix1", 2638, fixRepo, "persisting"
	})
	g.cu.Once(ctx)
	if !slices.Equal(g.promotes, []int{164}) {
		t.Fatalf("%v", g.promotes)
	}
}

// KP20: a push strips ready-to-merge, so a push asks for the check again.
func TestCurationOnceAPushAsksForThePromoteAgain(t *testing.T) {
	for _, tc := range []struct {
		route string
		want  []int
	}{{"appended", []int{164}}, {"unchanged", nil}, {"edit-pr", nil}} {
		t.Run(tc.route, func(t *testing.T) {
			g := newOnceRig(t)
			g.held(t, func(r *Record) {
				draftedHold(r)
				g.verified(r)
				r.Observed = "held" // observed earlier, and promoted: PromoteDue is clear
			})
			g.cu.Resolve = func(context.Context, kbresolve.Target, providers.AppliedResolution) (kbresolve.Result, error) {
				return kbresolve.Result{Route: tc.route}, nil
			}
			g.cu.Once(ctx)
			if !slices.Equal(g.promotes, tc.want) {
				t.Fatalf("%v", g.promotes)
			}
		})
	}
}

// A fix that could not be written is not "every verified generation's Resolve succeeded".
func TestCurationOnceATerminalResolveOnAHeldFixDoesNotLabel(t *testing.T) {
	g := newOnceRig(t)
	g.resolveErr = kbresolve.ErrClosedUnmerged
	g.held(t, func(r *Record) {
		draftedHold(r)
		g.verified(r)
	})
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; len(g.promotes) != 0 || h.PromoteDue {
		t.Fatalf("promotes %v, %+v", g.promotes, h)
	}
}

func TestCurationOnceADryRunDoesNotLabel(t *testing.T) {
	g := newOnceRig(t)
	g.cu.DryRun = true
	g.held(t, func(r *Record) {
		draftedHold(r)
		g.verified(r)
	})
	g.cu.Once(ctx)
	if len(g.promotes) != 0 {
		t.Fatalf("%v", g.promotes)
	}
}

// I3: latest means the newest generation, not the last verdict to arrive.
func TestCurationOnceALateVerdictDoesNotOutrankTheNewestGeneration(t *testing.T) {
	archived := func(monitor string) Generation {
		return Generation{FixRoom: "fix0", Repo: fixRepo, PR: 2639, Verdict: "resolved", Monitor: monitor}
	}
	for _, tc := range []struct {
		name, oldVerdict, lateVerdict, verdict, monitor string
		want                                            bool
	}{
		{name: "the newest persisted, the oldest late healthy", lateVerdict: "healthy", verdict: "persisting", want: false},
		{name: "the newest held, the oldest late regression", lateVerdict: "regression", verdict: "resolved", monitor: "healthy", want: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newOnceRig(t)
			path := filepath.Join(t.TempDir(), "outcome.jsonl")
			outcomes, err := outcome.New(path)
			must(t, err)
			g.cu.Outcome = outcomes
			g.held(t, func(r *Record) {
				draftedHold(r)
				r.Curation.Generations = []Generation{archived("")}
				r.FixRoom, r.PR, r.Repo, r.Verdict, r.Monitor = "fix1", 2638, fixRepo, tc.verdict, tc.monitor
			})
			g.cu.Once(ctx)
			if got := outcomes.Verifications()[holdFP]; got != tc.want {
				t.Fatalf("before the late verdict: %v", got)
			}
			g.now = g.now.Add(time.Hour) // a later tick, on a later clock
			must(t, g.l.Upsert("k1", func(r *Record) { r.Curation.Generations[0].Monitor = tc.lateVerdict }))
			g.cu.Once(ctx)
			if got := outcomes.Verifications()[holdFP]; got != tc.want {
				t.Fatalf("the late verdict moved it: %v", got)
			}
		})
	}
}

func TestCurationOnceAVerifysAtIsTheGenerationsOwn(t *testing.T) {
	g := newOnceRig(t)
	accepted := g.now
	g.held(t, func(r *Record) {
		draftedHold(r)
		r.Curation.Generations = []Generation{g.failedGeneration()}
		g.verified(r)
	})
	g.now = g.now.Add(time.Hour)
	g.cu.Once(ctx)
	if len(g.verifies) != 2 || !g.verifies[0].At.Equal(accepted) || !g.verifies[1].At.Equal(accepted.Add(1)) {
		t.Fatalf("%+v", g.verifies)
	}
}

// A replay, in the same ledger or after a reload, counts once; a flip on the same instant still flips.
func TestCurationOnceAVerifyReplayIsIdempotent(t *testing.T) {
	g := newOnceRig(t)
	path := filepath.Join(t.TempDir(), "outcome.jsonl")
	outcomes, err := outcome.New(path)
	must(t, err)
	g.cu.Outcome = outcomes
	g.held(t, func(r *Record) {
		draftedHold(r)
		g.verified(r)
	})
	count := func(l *outcome.Ledger) outcome.Aggregate {
		t.Helper()
		counts, err := l.OpenCounts()
		must(t, err)
		return counts[draftEntry]
	}
	g.cu.Once(ctx)
	must(t, g.l.Upsert("k1", func(r *Record) { r.Observed = "" })) // the crash between Verify and Observed
	g.now = g.now.Add(time.Hour)
	g.cu.Once(ctx)
	if got := count(outcomes); got.Verified != 1 || got.Failed != 0 {
		t.Fatalf("%+v", got)
	}
	reopened, err := outcome.New(path)
	must(t, err)
	if got := count(reopened); got.Verified != 1 || got.Failed != 0 || !reopened.Verifications()[holdFP] {
		t.Fatalf("after a reload: %+v", got)
	}
	must(t, g.l.Upsert("k1", func(r *Record) { r.Reverted = true }))
	g.cu.Once(ctx)
	if got := count(outcomes); got.Verified != 0 || got.Failed != 1 || outcomes.Verifications()[holdFP] {
		t.Fatalf("a revert on the same instant flips the room: %+v", got)
	}
}

// A hold written with no acceptance time, as a recall's is, gets one once and keeps it.
func TestCurationOnceAHoldWithoutAnInstantGetsOneOnce(t *testing.T) {
	g := newOnceRig(t)
	must(t, g.l.Upsert("k1", func(r *Record) {
		r.Curation = &Hold{ObserveOnly: true, Entry: "incidents/web.md", Drafted: "none", Fingerprint: holdFP}
		g.verified(r)
	}))
	stamped := g.now
	g.cu.Once(ctx)
	if h := g.record(t).Curation; !h.AcceptedAt.Equal(stamped) || len(g.verifies) != 1 || !g.verifies[0].At.Equal(stamped) {
		t.Fatalf("%+v, %+v", h, g.verifies)
	}
	g.now = g.now.Add(time.Hour)
	must(t, g.l.Upsert("k1", func(r *Record) { r.Reverted = true }))
	g.cu.Once(ctx)
	if h := g.record(t).Curation; !h.AcceptedAt.Equal(stamped) || len(g.verifies) != 2 || !g.verifies[1].At.Equal(stamped) {
		t.Fatalf("%+v, %+v", h, g.verifies)
	}
}

// tick runs n ticks.
func tick(g *onceRig, n int) {
	for range n {
		g.cu.Once(ctx)
	}
}

// Past the window, a retried failure ends only after 30 attempts: a hold's window often passes while
// RunLore is off, and one blip must not lose an entry.
func TestCurationOnceRetriesEndAfterThirtyFailuresPastTheWindow(t *testing.T) {
	boom := errors.New("permanent")
	past := func(g *onceRig) { g.now = g.now.Add(168 * time.Hour) }
	for _, tc := range []struct {
		name  string
		setup func(g *onceRig)
		seed  func(g *onceRig, r *Record)
		check func(t *testing.T, g *onceRig)
	}{
		{
			name:  "resolve",
			setup: func(g *onceRig) { g.resolveErr = boom },
			seed: func(g *onceRig, r *Record) {
				draftedHold(r)
				g.verified(r)
			},
			check: func(t *testing.T, g *onceRig) {
				t.Helper()
				if r := g.record(t); r.Resolved != "held" || len(g.promotes) != 0 || r.Curation.PromoteDue {
					t.Fatalf("%+v, promotes %v", r, g.promotes)
				}
			},
		},
		{
			name:  "inner",
			setup: func(g *onceRig) { g.inner.err, g.inner.ref = boom, providers.Ref{} },
			seed:  func(g *onceRig, r *Record) { g.verified(r) },
			check: func(t *testing.T, g *onceRig) {
				t.Helper()
				if h := g.record(t).Curation; h.Drafted != "none" || len(g.inner.invs) != 1+30 {
					t.Fatalf("one try in the window, thirty past it, none after: inner %d, %+v", len(g.inner.invs), h)
				}
			},
		},
		{
			name:  "promote",
			setup: func(g *onceRig) { g.promoteErr = boom },
			seed: func(g *onceRig, r *Record) {
				draftedHold(r)
				g.verified(r)
				r.Observed, r.Resolved, r.Curation.PromoteDue = "held", "held", true
			},
			check: func(t *testing.T, g *onceRig) {
				t.Helper()
				if h := g.record(t).Curation; h.PromoteDue {
					t.Fatalf("%+v", h)
				}
			},
		},
		{
			name:  "link",
			setup: func(g *onceRig) { g.linkErr = boom },
			seed: func(_ *onceRig, r *Record) {
				draftedHold(r)
				r.Curation.LinkDue = true
			},
			check: func(t *testing.T, g *onceRig) {
				t.Helper()
				if h := g.record(t).Curation; h.LinkDue {
					t.Fatalf("%+v", h)
				}
			},
		},
		{
			name:  "verify",
			setup: func(g *onceRig) { g.verifyErr = boom },
			seed: func(g *onceRig, r *Record) {
				draftedHold(r)
				g.verified(r)
				r.Resolved = "held"
			},
			check: func(t *testing.T, g *onceRig) {
				t.Helper()
				if r := g.record(t); r.Observed != "held" {
					t.Fatalf("%+v", r)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newOnceRig(t)
			tc.setup(g)
			g.held(t, func(r *Record) { tc.seed(g, r) })
			g.cu.Once(ctx)
			if strings.Contains(g.logs.String(), "window_passed") {
				t.Fatalf("within the window it retries: %s", g.logs.String())
			}
			past(g)
			g.logs.Reset()
			tick(g, 29)
			if strings.Contains(g.logs.String(), "window_passed") {
				t.Fatalf("29 failures are not terminal: %s", g.logs.String())
			}
			tick(g, 3)
			tc.check(t, g)
			if n := strings.Count(g.logs.String(), "window_passed"); n != 1 {
				t.Fatalf("logged %d times: %s", n, g.logs.String())
			}
			if h := g.record(t).Curation; len(h.PastWindowFailures) != 0 {
				t.Fatalf("a terminal step leaves no count: %+v", h.PastWindowFailures)
			}
		})
	}
}

// A blip at the window retries and succeeds, and the success clears its count.
func TestCurationOnceABlipAtTheWindowRetriesAndSucceeds(t *testing.T) {
	blip := errors.New("a blip")
	for _, tc := range []struct {
		name  string
		seed  func(g *onceRig, r *Record)
		fail  func(g *onceRig, err error)
		check func(t *testing.T, g *onceRig)
	}{
		{
			name: "inner",
			fail: func(g *onceRig, err error) {
				g.inner.err = err
				g.inner.ref = providers.Ref{}
				if err == nil {
					g.inner.ref = providers.Ref{URL: draftURL}
				}
			},
			check: func(t *testing.T, g *onceRig) {
				t.Helper()
				if h := g.record(t).Curation; h.Drafted != draftURL || h.Entry != draftEntry || len(g.inner.invs) != 2 {
					t.Fatalf("inner %d, %+v", len(g.inner.invs), h)
				}
			},
		},
		{
			name: "PREntry",
			fail: func(g *onceRig, err error) { g.entryErr = func(int) error { return err } },
			check: func(t *testing.T, g *onceRig) {
				t.Helper()
				if h := g.record(t).Curation; h.Drafted != draftURL || h.Entry != draftEntry || h.EntryDue || len(g.inner.invs) != 1 {
					t.Fatalf("inner %d, %+v", len(g.inner.invs), h)
				}
			},
		},
		{
			name: "Resolve",
			seed: func(g *onceRig, r *Record) {
				draftedHold(r)
				g.verified(r)
			},
			fail: func(g *onceRig, err error) { g.resolveErr = err },
			check: func(t *testing.T, g *onceRig) {
				t.Helper()
				if r := g.record(t); r.Resolved != "held" || len(g.resolves) != 1 || !slices.Equal(g.promotes, []int{164}) {
					t.Fatalf("%+v, %+v, %v", r, g.resolves, g.promotes)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := newOnceRig(t)
			g.held(t, func(r *Record) {
				if tc.seed != nil {
					tc.seed(g, r)
				}
			})
			tc.fail(g, blip)
			g.now = g.now.Add(168 * time.Hour)
			g.cu.Once(ctx)
			if h := g.record(t).Curation; len(h.PastWindowFailures) != 1 || strings.Contains(g.logs.String(), "window_passed") {
				t.Fatalf("one failure counted, not terminal: %+v, %s", h, g.logs.String())
			}
			tc.fail(g, nil)
			g.cu.Once(ctx)
			g.cu.Once(ctx)
			tc.check(t, g)
			if h := g.record(t).Curation; len(h.PastWindowFailures) != 0 {
				t.Fatalf("the success clears the count: %+v", h.PastWindowFailures)
			}
		})
	}
}

func TestCurationOnceARestartKeepsTheCount(t *testing.T) {
	g := newOnceRig(t)
	g.inner.err, g.inner.ref = errors.New("permanent"), providers.Ref{}
	g.held(t, nil)
	g.now = g.now.Add(168 * time.Hour)
	tick(g, 20)

	next, err := OpenLedger(g.l.path, g.cu.Now)
	must(t, err)
	g.l, g.cu.Ledger = next, next
	if h := g.record(t).Curation; h.PastWindowFailures["draft"] != 20 {
		t.Fatalf("%+v", h.PastWindowFailures)
	}
	tick(g, 9)
	if h := g.record(t).Curation; h.Drafted != "" {
		t.Fatalf("29 failures: %+v", h)
	}
	tick(g, 2)
	if h := g.record(t).Curation; h.Drafted != "none" || len(g.inner.invs) != 30 {
		t.Fatalf("the 30th across the restart ends it: inner %d, %+v", len(g.inner.invs), h)
	}
	if n := strings.Count(g.logs.String(), "window_passed"); n != 1 {
		t.Fatalf("logged %d times: %s", n, g.logs.String())
	}
}

func TestCurationOnceASuccessResetsTheCount(t *testing.T) {
	g := newOnceRig(t)
	g.resolveErr = errors.New("a blip")
	g.held(t, func(r *Record) {
		draftedHold(r)
		g.verified(r)
	})
	g.now = g.now.Add(168 * time.Hour)
	tick(g, 29)
	g.resolveErr = nil
	g.cu.Once(ctx)

	// The fix flips later, and its write has thirty attempts of its own.
	g.resolveErr = errors.New("a blip")
	must(t, g.l.Upsert("k1", func(r *Record) { r.Reverted = true }))
	tick(g, 29)
	r := g.record(t)
	if r.Resolved != "held" || r.Curation.PastWindowFailures["resolve:fix1"] != 29 {
		t.Fatalf("%+v", r)
	}
	tick(g, 1)
	if r := g.record(t); r.Resolved != "failed" {
		t.Fatalf("%+v", r)
	}
}

// Two fixes failing in one tick each keep their own count, so neither ends early.
func TestCurationOnceEachGenerationCountsItsOwnFailures(t *testing.T) {
	g := newOnceRig(t)
	g.resolveErr = errors.New("permanent")
	g.held(t, func(r *Record) {
		draftedHold(r)
		r.Curation.Generations = []Generation{g.failedGeneration()}
		g.verified(r)
	})
	g.now = g.now.Add(168 * time.Hour)
	tick(g, 29)
	r := g.record(t)
	if r.Resolved != "" || r.Curation.Generations[0].Resolved != "" || r.Curation.PastWindowFailures["resolve:fix0"] != 29 || r.Curation.PastWindowFailures["resolve:fix1"] != 29 {
		t.Fatalf("%+v", r)
	}
	tick(g, 1)
	r = g.record(t)
	if r.Resolved != "held" || r.Curation.Generations[0].Resolved != "failed" {
		t.Fatalf("%+v", r)
	}
}

// The count is the ledger's own: a failed append cannot leave it ahead of the file.
func TestLedgerClonesTheFailureCounts(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, func(r *Record) { r.Curation.PastWindowFailures = map[string]int{"draft": 1} })
	got := g.record(t)
	got.Curation.PastWindowFailures["draft"] = 9
	if g.record(t).Curation.PastWindowFailures["draft"] != 1 {
		t.Fatal("a clone's write reached the ledger")
	}
}

// An adoption that keeps failing never blocks the window's draft.
func TestCurationOnceAFailedAdoptionDoesNotBlockTheWindowsDraft(t *testing.T) {
	for name, setup := range map[string]func(g *onceRig){
		"the listing fails": func(g *onceRig) { g.listErr = errors.New("permanent") },
		"the candidate's entry cannot be read": func(g *onceRig) {
			g.open = []providers.CuratedIssue{{Number: 9, URL: "https://github.com/o/kb/pull/9", Body: providers.FingerprintMarker(holdFP)}}
			g.entryErr = func(n int) error {
				if n == 9 {
					return errors.New("two entries in one PR")
				}
				return nil
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			g := newOnceRig(t)
			setup(g)
			g.held(t, g.verified)
			g.cu.Once(ctx)
			if len(g.inner.invs) != 0 {
				t.Fatal("within the window it waits")
			}
			g.now = g.now.Add(168 * time.Hour)
			g.cu.Once(ctx)
			if h := g.record(t).Curation; len(g.inner.invs) != 1 || h.Drafted != draftURL || h.Entry != draftEntry || len(h.PastWindowFailures) != 0 {
				t.Fatalf("the first tick past the window drafts: inner %d, %+v", len(g.inner.invs), h)
			}
		})
	}
}

// A cancelled context says nothing about the failure: leadership moved, and the next leader retries.
func TestCurationOnceACancelledContextIsNotTerminal(t *testing.T) {
	g := newOnceRig(t)
	run, cancel := context.WithCancel(ctx)
	g.inner.ref, g.inner.err = providers.Ref{}, context.Canceled
	g.held(t, nil)
	g.cu.Inner = cancelling{g.inner, cancel}
	g.now = g.now.Add(168 * time.Hour)
	g.cu.Once(run)
	if h := g.record(t).Curation; h.Drafted != "" || len(h.PastWindowFailures) != 0 || strings.Contains(g.logs.String(), "window_passed") {
		t.Fatalf("%+v, %s", h, g.logs.String())
	}
}

type cancelling struct {
	*recordingInner
	cancel context.CancelFunc
}

func (c cancelling) Curate(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	c.cancel()
	return c.recordingInner.Curate(ctx, inv)
}

func TestCurationOnceAnEmptyRepoUsesTheConfiguredOne(t *testing.T) {
	g := newOnceRig(t)
	g.cu.Repository = "Smana/fallback"
	g.held(t, func(r *Record) {
		g.verified(r)
		r.Repo = ""
	})
	g.cu.Once(ctx)
	if !slices.Equal(g.pulled, []string{"Smana/fallback#2638"}) {
		t.Fatalf("%v", g.pulled)
	}
	if got := g.inner.invs[0].Resolutions[0].FixPR; got != "https://github.com/Smana/fallback/pull/2638" {
		t.Fatalf("%q", got)
	}
}

// The repo comes from a payload and builds an API path: a bad one is never asked about, and the
// fix cannot be written, once, with the verification still recorded.
func TestCurationOnceABadRepoCannotBeWritten(t *testing.T) {
	for _, bad := range []string{"../../admin", "owner", "a/b/c", "a/.."} {
		t.Run(bad, func(t *testing.T) {
			g := newOnceRig(t)
			g.held(t, func(r *Record) {
				g.verified(r)
				r.Repo = bad
			})
			g.cu.Once(ctx)
			g.cu.Once(ctx)
			r := g.record(t)
			if len(g.pulled) != 0 || len(g.inner.invs) != 1 || len(g.inner.invs[0].Resolutions) != 0 || len(g.resolves) != 0 {
				t.Fatalf("pulled %v, %+v, %+v", g.pulled, g.inner.invs, g.resolves)
			}
			if r.Resolved != "held" || r.Observed != "held" || len(g.verifies) != 1 {
				t.Fatalf("%+v", r)
			}
			if n := strings.Count(g.logs.String(), "cannot be written"); n != 2 {
				t.Fatalf("dropped from the draft, then settled: %s", g.logs.String())
			}
		})
	}
}

func TestCurationOnceALeaderChangeDraftsOnce(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, g.verified)
	g.cu.Once(ctx)

	next, err := OpenLedger(g.l.path, g.cu.Now)
	must(t, err)
	must(t, next.Reload())
	g.cu.Ledger = next
	g.cu.Once(ctx)
	if len(g.inner.invs) != 1 || len(g.verifies) != 1 || len(g.promotes) != 1 {
		t.Fatalf("inner %d, verifies %d, promotes %d", len(g.inner.invs), len(g.verifies), len(g.promotes))
	}
}

func TestCurationOnceAnUnreadableSnapshotIsTerminal(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, func(r *Record) {
		g.verified(r)
		r.Curation.Investigation = nil
	})
	g.cu.Once(ctx)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; h.Drafted != "none" || len(g.inner.invs) != 0 {
		t.Fatalf("inner %d, %+v", len(g.inner.invs), h)
	}
}

func TestCurationOnceANilInnerSettlesTheHold(t *testing.T) {
	g := newOnceRig(t)
	g.cu.Inner = nil
	g.held(t, g.verified)
	g.cu.Once(ctx)
	if h := g.record(t).Curation; h.Drafted != "none" {
		t.Fatalf("%+v", h)
	}
}

func TestCurationOnceStopsOnACancelledContext(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, g.verified)
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	g.cu.Once(cancelled)
	if len(g.calls) != 0 {
		t.Fatalf("%v", g.calls)
	}
}

func TestCurationOnceRunTicksUntilCancelled(t *testing.T) {
	g := newOnceRig(t)
	g.held(t, g.verified)
	run, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	drafted := make(chan struct{})
	g.cu.Promote = func(context.Context, int) error {
		close(drafted)
		cancel()
		return nil
	}
	go func() {
		g.cu.Run(run, time.Millisecond)
		close(done)
	}()
	select {
	case <-drafted:
	case <-time.After(10 * time.Second):
		t.Fatal("no tick")
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not end")
	}
}

func TestDiffLine(t *testing.T) {
	for want, sum := range map[string]providers.PullSummary{
		"":                                {},
		"1 file: a (+2 −3)":               {Files: []string{"a"}, Additions: 2, Deletions: 3},
		"2 files: a, b (+0 −1)":           {Files: []string{"a", "b"}, Deletions: 1},
		"5+ files: a, b, c, d, e (+9 −9)": {Files: []string{"a", "b", "c", "d", "e"}, Additions: 9, Deletions: 9},
	} {
		if got := diffLine(sum); got != want {
			t.Errorf("%q, want %q", got, want)
		}
	}
}
