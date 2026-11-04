// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/providers"
)

var (
	_ PullReader  = (*github.Client)(nil)
	_ DraftLister = (*github.Client)(nil)
)

var ctx = context.Background()

// countingInner is the curator: it counts its calls, answers ref and err, and remembers the
// fingerprint of each PR it opened. weak is what its bar refuses.
type countingInner struct {
	calls  int
	ref    providers.Ref
	err    error
	opened []string
	weak   func(providers.Investigation) bool
}

func (c *countingInner) Curate(_ context.Context, inv providers.Investigation) (providers.Ref, error) {
	c.calls++
	if c.ref.URL != "" {
		c.opened = append(c.opened, curator.DupFingerprint(inv))
	}
	return c.ref, c.err
}

func (c *countingInner) MeetsBar(inv providers.Investigation) bool {
	return c.weak == nil || !c.weak(inv)
}

func scriptOf(t *testing.T, script string) []int {
	t.Helper()
	var codes []int
	for s := range strings.SplitSeq(script, ",") {
		code, err := strconv.Atoi(s)
		must(t, err)
		codes = append(codes, code)
	}
	return codes
}

// newCuration is a Curation over a fake factory answering script (statuses joined by commas), a
// temp ledger and a counting Inner.
func newCuration(t *testing.T, script string) (*Curation, *countingInner, *Ledger) {
	t.Helper()
	n, _, l, _ := newTestNotifier(t, scriptOf(t, script))
	inner := &countingInner{}
	return &Curation{Inner: inner, Filer: n, Ledger: l, Now: func() time.Time { return time.Unix(1_800_000_000, 0) }}, inner, l
}

// clientAnswering is a factory client whose next task gets code, in room.
func clientAnswering(t *testing.T, code int, room string) *Client {
	t.Helper()
	clock := time.Unix(1_800_000_000, 0)
	return newTestClient(t, &fakeFactory{ttl: time.Hour, script: []int{code}, newRoom: room}, &clock)
}

func incident(key string) providers.Investigation {
	inv := testInvestigation()
	inv.TriggerKey, inv.Verified, inv.Confidence = key, true, 0.9
	return inv
}

func recall(key, entry string) providers.Investigation {
	inv := incident(key)
	inv.Recalled, inv.RecalledEntry = true, entry
	return inv
}

// snapshotTitle is the title of the investigation the hold keeps.
func snapshotTitle(t *testing.T, h *Hold) string {
	t.Helper()
	var inv providers.Investigation
	must(t, json.Unmarshal(h.Investigation, &inv))
	return inv.Title
}

func weakByTitle(inv providers.Investigation) bool { return inv.Title == "weak" }

func TestCurateHoldsOnAccept(t *testing.T) {
	for script, held := range map[string]bool{"201": true, "200": true, "202": false, "429": false, "503,503,503": false} {
		cu, inner, l := newCuration(t, script)     // fake factory answering script, temp ledger, counting Inner
		ref, err := cu.Curate(ctx, incident("k1")) // TriggerKey k1, verified, confidence 0.9
		if r, _ := l.Get("k1"); err != nil || (r.Curation != nil) != held || (inner.calls == 0) != held || held && ref.URL != "" {
			t.Errorf("%s: hold %v, inner %d, %v", script, r.Curation != nil, inner.calls, err)
		}
	}
}

func TestANewGenerationKeepsTheHold(t *testing.T) {
	cu, _, l := newCuration(t, "201")
	_, _ = cu.Curate(ctx, incident("k1"))
	first, _ := l.Get("k1")
	_ = l.Upsert("k1", func(r *Record) {
		r.FixRoom, r.PR, r.Verdict, r.Monitor, r.SlackRoot = "fix1", 2638, "persisting", "regression", "1700.1"
	})
	cu.Filer.Client = clientAnswering(t, 201, "room0002") // the incident re-fired after the task ended
	second := incident("k1")
	second.Title, second.Resource.Name = "a reworded title", "another-workload" // both model-written
	_, _ = cu.Curate(ctx, second)
	r, _ := l.Get("k1")
	if r.Room != "room0002" || r.FixRoom != "" || r.SlackRoot != "" || !r.Curation.AcceptedAt.Equal(first.Curation.AcceptedAt) ||
		len(r.Curation.Generations) != 1 || r.Curation.Generations[0].Outcome() != "failed" {
		t.Fatalf("%+v", r)
	}
	if r.Curation.Fingerprint != first.Curation.Fingerprint || r.Curation.Fingerprint == curator.DupFingerprint(second) ||
		!strings.Contains(string(r.Curation.Investigation), "a reworded title") {
		t.Fatalf("the fingerprint stays frozen while the snapshot follows the newest generation: %+v", r.Curation)
	}
}

func TestCurateFirstHoldIsComplete(t *testing.T) {
	cu, _, l := newCuration(t, "201")
	inv := incident("k1")
	_, err := cu.Curate(ctx, inv)
	must(t, err)
	r, _ := l.Get("k1")
	h := r.Curation
	if h == nil || !h.AcceptedAt.Equal(cu.Now()) || h.Fingerprint != curator.DupFingerprint(inv) || h.Entry != "" || h.Drafted != "" || h.ObserveOnly {
		t.Fatalf("%+v", h)
	}
	if got := snapshotTitle(t, h); got != inv.Title {
		t.Fatalf("the snapshot reads back as the investigation: %q", got)
	}
	if r.Room != "abcdefgh" || r.Issue != 42 {
		t.Fatalf("the filing is recorded beside the hold: %+v", r)
	}
}

func TestCurateARecallIsFiledAndNeverDrafted(t *testing.T) {
	for _, tc := range []struct {
		script string
		held   bool
	}{{"201", true}, {"200", true}, {"202", false}, {"429", false}, {"503,503,503", false}} {
		t.Run(tc.script, func(t *testing.T) {
			cu, inner, l := newCuration(t, tc.script)
			inner.ref = providers.Ref{URL: "https://example.test/pull/1"}
			inv := recall("k1", "incidents/web.md")
			ref, err := cu.Curate(ctx, inv)
			if err != nil || ref.URL != "" || inner.calls != 0 {
				t.Fatalf("a recall has an empty Ref and no Inner: %+v, %v, inner %d", ref, err, inner.calls)
			}
			r, _ := l.Get("k1")
			if !tc.held {
				if r.Curation != nil {
					t.Fatalf("no hold on %s: %+v", tc.script, r.Curation)
				}
				return
			}
			want := Hold{ObserveOnly: true, Entry: "incidents/web.md", Drafted: "none", Fingerprint: curator.DupFingerprint(inv)}
			if !reflect.DeepEqual(*r.Curation, want) {
				t.Fatalf("got %+v, want %+v", *r.Curation, want)
			}
		})
	}
}

func TestCurateARecallLeavesAnExistingHoldAlone(t *testing.T) {
	cu, inner, l := newCuration(t, "201")
	_, err := cu.Curate(ctx, incident("k1"))
	must(t, err)
	before, _ := l.Get("k1")

	cu.Filer.Client = clientAnswering(t, 200, "")
	_, err = cu.Curate(ctx, recall("k1", "incidents/web.md"))
	must(t, err)
	r, _ := l.Get("k1")
	if !reflect.DeepEqual(r.Curation, before.Curation) {
		t.Fatalf("the same room leaves the hold as it was: %+v", r.Curation)
	}

	must(t, l.Upsert("k1", func(r *Record) { r.FixRoom, r.Verdict = "fix1", "persisting" }))
	cu.Filer.Client = clientAnswering(t, 201, "room0002")
	_, err = cu.Curate(ctx, recall("k1", "incidents/web.md"))
	must(t, err)
	r, _ = l.Get("k1")
	h := r.Curation
	if inner.calls != 0 || len(h.Generations) != 1 || h.Generations[0].FixRoom != "fix1" {
		t.Fatalf("a closed generation is archived: inner %d, %+v", inner.calls, h)
	}
	h.Generations = nil
	if !reflect.DeepEqual(h, before.Curation) {
		t.Fatalf("nothing else of the hold moves: %+v", h)
	}
}

func TestCurateAFreshInvestigationTurnsAnObserveOnlyHoldOrdinary(t *testing.T) {
	cu, inner, l := newCuration(t, "201")
	_, err := cu.Curate(ctx, recall("k1", "incidents/web.md"))
	must(t, err)
	must(t, l.Upsert("k1", func(r *Record) {
		r.FixRoom, r.Verdict, r.Monitor = "fix1", "resolved", "healthy"
	}))
	later := time.Unix(1_800_000_600, 0)
	cu.Now = func() time.Time { return later }
	cu.Filer.Client = clientAnswering(t, 201, "room0002")
	inv := incident("k1")
	inv.Title = "the recall did not fire"
	inv.Resource.Name = "another-workload"
	ref, err := cu.Curate(ctx, inv)
	must(t, err)
	r, _ := l.Get("k1")
	h := r.Curation
	if ref.URL != "" || inner.calls != 0 || h.ObserveOnly || h.Drafted != "" || h.Entry != "" ||
		h.Fingerprint != curator.DupFingerprint(inv) || !h.AcceptedAt.Equal(later) || snapshotTitle(t, h) != inv.Title {
		t.Fatalf("ref %+v, inner %d, %+v", ref, inner.calls, h)
	}
	if len(h.Generations) != 0 {
		t.Fatalf("a held generation of the recall is not this incident's: %+v", h.Generations)
	}
}

func TestCurateADraftedHoldIsFrozen(t *testing.T) {
	const pr = "https://github.com/o/kb/pull/9"
	for _, tc := range []struct {
		name, drafted, want string
		code                int
	}{
		{"url, 201", pr, pr, 201},
		{"url, 200", pr, pr, 200},
		{"none, 200", "none", "", 200},
		{"none, 201", "none", "", 201},
		{"url, 202", pr, pr, 202},
		{"none, 429", "none", "", 429},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cu, inner, l := newCuration(t, "201")
			_, err := cu.Curate(ctx, incident("k1"))
			must(t, err)
			must(t, l.Upsert("k1", func(r *Record) { r.Curation.Drafted, r.Curation.Entry = tc.drafted, "incidents/web.md" }))
			before, _ := l.Get("k1")

			cu.Filer.Client = clientAnswering(t, tc.code, "room0002")
			next := incident("k1")
			next.Title = "a reworded title"
			ref, err := cu.Curate(ctx, next)
			must(t, err)
			r, _ := l.Get("k1")
			if ref.URL != tc.want || inner.calls != 0 || !reflect.DeepEqual(r.Curation, before.Curation) {
				t.Fatalf("ref %+v, inner %d, %+v", ref, inner.calls, r.Curation)
			}
		})
	}
}

// A refused detection is no generation, and the hold still owns the entry: drafting here would open
// a PR under this investigation's fingerprint beside the one the window drafts.
func TestCurateARefusalLeavesAStandingHold(t *testing.T) {
	for _, script := range []string{"202", "429", "503,503,503"} {
		t.Run(script, func(t *testing.T) {
			cu, inner, l := newCuration(t, "201")
			inv := incident("k1")
			_, err := cu.Curate(ctx, inv)
			must(t, err)
			before, _ := l.Get("k1")

			clock := time.Unix(1_800_000_000, 0)
			cu.Filer.Client = newTestClient(t, &fakeFactory{ttl: time.Hour, script: scriptOf(t, script)}, &clock)
			next := incident("k1")
			next.Title = "a reworded title"
			ref, err := cu.Curate(ctx, next)
			must(t, err)
			r, _ := l.Get("k1")
			if ref.URL != "" || inner.calls != 0 || !reflect.DeepEqual(r.Curation, before.Curation) {
				t.Fatalf("ref %+v, inner %d, %+v", ref, inner.calls, r.Curation)
			}
		})
	}
}

func TestCurateARefusalOnAnObserveOnlyHoldTakesTodaysPath(t *testing.T) {
	cu, inner, l := newCuration(t, "201")
	_, err := cu.Curate(ctx, recall("k1", "incidents/web.md"))
	must(t, err)
	before, _ := l.Get("k1")
	cu.Filer.Client = clientAnswering(t, 202, "")
	inner.ref = providers.Ref{URL: "https://github.com/o/kb/pull/9"}
	ref, err := cu.Curate(ctx, incident("k1"))
	must(t, err)
	r, _ := l.Get("k1")
	if inner.calls != 1 || ref != inner.ref || !reflect.DeepEqual(r.Curation, before.Curation) {
		t.Fatalf("ref %+v, inner %d, %+v", ref, inner.calls, r.Curation)
	}
}

func TestCurateTodaysPathAnswersInnersResult(t *testing.T) {
	cu, inner, _ := newCuration(t, "202")
	inner.ref, inner.err = providers.Ref{URL: "https://github.com/o/kb/pull/9"}, errors.New("forge down")
	ref, err := cu.Curate(ctx, incident("k1"))
	if ref != inner.ref || !errors.Is(err, inner.err) {
		t.Fatalf("%+v, %v", ref, err)
	}
}

func TestCurateANilInnerDraftsNothing(t *testing.T) {
	cu, _, _ := newCuration(t, "202")
	cu.Inner = nil
	if ref, err := cu.Curate(ctx, incident("k1")); err != nil || ref.URL != "" {
		t.Fatalf("%+v, %v", ref, err)
	}
}

func TestCurateWithNothingFiledTakesTodaysPath(t *testing.T) {
	t.Run("no trigger key", func(t *testing.T) {
		cu, inner, _ := newCuration(t, "201")
		if _, err := cu.Curate(ctx, incident("")); err != nil || inner.calls != 1 {
			t.Fatalf("inner %d, %v", inner.calls, err)
		}
	})
	t.Run("a disabled client", func(t *testing.T) {
		cu, inner, l := newCuration(t, "201")
		cu.Filer.Client = nil
		_, err := cu.Curate(ctx, incident("k1"))
		if r, _ := l.Get("k1"); err != nil || inner.calls != 1 || r.Curation != nil {
			t.Fatalf("inner %d, %+v, %v", inner.calls, r.Curation, err)
		}
	})
}

func TestCurateA200WithAHoldTouchesNothing(t *testing.T) {
	for _, tc := range []struct {
		name  string
		first providers.Investigation
	}{
		{"an ordinary hold", incident("k1")},
		{"an observe-only hold", recall("k1", "incidents/web.md")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cu, inner, l := newCuration(t, "201")
			_, err := cu.Curate(ctx, tc.first)
			must(t, err)
			before, _ := l.Get("k1")
			cu.Filer.Client = clientAnswering(t, 200, "")
			next := incident("k1")
			next.Title, next.Resource.Name = "a reworded title", "another-workload"
			ref, err := cu.Curate(ctx, next)
			must(t, err)
			r, _ := l.Get("k1")
			if ref.URL != "" || inner.calls != 0 || !reflect.DeepEqual(r.Curation, before.Curation) {
				t.Fatalf("ref %+v, inner %d, %+v", ref, inner.calls, r.Curation)
			}
		})
	}
}

func TestCurateA200WithNoHoldHolds(t *testing.T) {
	cu, inner, l := newCuration(t, "200")
	_, err := cu.Curate(ctx, incident("k1"))
	must(t, err)
	if r, _ := l.Get("k1"); r.Curation == nil || inner.calls != 0 {
		t.Fatalf("the crash recovery of a filing with no hold: inner %d, %+v", inner.calls, r.Curation)
	}
}

func TestCurateA201ReplacesTheSnapshotOnlyWhenItMeetsTheBar(t *testing.T) {
	for _, tc := range []struct{ name, newest, want string }{
		{"it meets the bar", "stronger", "stronger"},
		{"it does not", "weak", "web crash loops"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cu, inner, l := newCuration(t, "201")
			inner.weak = weakByTitle
			_, err := cu.Curate(ctx, incident("k1"))
			must(t, err)
			before, _ := l.Get("k1")
			must(t, l.Upsert("k1", func(r *Record) { r.FixRoom, r.Verdict = "fix1", "persisting" }))
			cu.Filer.Client = clientAnswering(t, 201, "room0002")
			next := incident("k1")
			next.Title = tc.newest
			ref, err := cu.Curate(ctx, next)
			must(t, err)
			r, _ := l.Get("k1")
			h := r.Curation
			if ref.URL != "" || inner.calls != 0 || snapshotTitle(t, h) != tc.want {
				t.Fatalf("ref %+v, inner %d, snapshot %q", ref, inner.calls, snapshotTitle(t, h))
			}
			if r.Room != "room0002" || r.FixRoom != "" || len(h.Generations) != 1 || h.Generations[0].FixRoom != "fix1" {
				t.Fatalf("the generation is recorded either way: %+v", r)
			}
			if h.Fingerprint != before.Curation.Fingerprint || !h.AcceptedAt.Equal(before.Curation.AcceptedAt) {
				t.Fatalf("%+v", h)
			}
		})
	}
}

// A weak first investigation still holds: the factory took it, and only the bar at draft time decides.
func TestCurateAWeakFirstInvestigationHolds(t *testing.T) {
	cu, inner, l := newCuration(t, "201")
	inner.weak = weakByTitle
	inv := incident("k1")
	inv.Title = "weak"
	_, err := cu.Curate(ctx, inv)
	must(t, err)
	if r, _ := l.Get("k1"); r.Curation == nil || snapshotTitle(t, r.Curation) != "weak" || inner.calls != 0 {
		t.Fatalf("inner %d, %+v", inner.calls, r.Curation)
	}
}

func TestCurateTheCuratorsOwnBarDecides(t *testing.T) {
	cu, _, l := newCuration(t, "201")
	cu.Inner = &curator.Curator{MinConfidence: 0.7}
	good := incident("k1")
	good.RootCauses = []providers.Hypothesis{{Summary: "s", Evidence: []string{"e"}, SuggestedAction: "a"}}
	_, err := cu.Curate(ctx, good)
	must(t, err)
	cu.Filer.Client = clientAnswering(t, 201, "room0002")
	weak := good
	weak.Title, weak.Verified = "unverified", false
	_, err = cu.Curate(ctx, weak)
	must(t, err)
	if r, _ := l.Get("k1"); snapshotTitle(t, r.Curation) != good.Title {
		t.Fatalf("an unverified finding replaced a verified one: %q", snapshotTitle(t, r.Curation))
	}
}

// Whatever opened today's PR, the hold that follows carries that PR's fingerprint, so the draft
// adopts the PR: one PR per incident, however far the model-written resource moved.
func TestCurateAHoldAfterTodaysPRCarriesItsFingerprint(t *testing.T) {
	const pr = "https://github.com/o/kb/pull/9"
	drifted := func() providers.Investigation {
		inv := incident("k1")
		inv.Title, inv.Resource.Name = "a reworded title", "another-workload"
		return inv
	}
	huge := incident("k1")
	huge.Fingerprint = strings.Repeat("x", maxSnapshotBytes) // a field the filing does not render, so the test stays fast
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, cu *Curation)
		first providers.Investigation
		code  int
	}{
		{name: "no hold, a 429", first: incident("k1"), code: 429},
		{name: "no hold, a 202", first: incident("k1"), code: 202},
		{name: "no hold, a 503", first: incident("k1"), code: 503},
		{name: "a hold lost to its size", first: huge, code: 201},
		{name: "observe-only, a 429", first: incident("k1"), code: 429, setup: func(t *testing.T, cu *Curation) {
			t.Helper()
			cu.Filer.Client = clientAnswering(t, 201, "abcdefgh")
			_, err := cu.Curate(ctx, recall("k1", "incidents/web.md"))
			must(t, err)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cu, inner, l := newCuration(t, "201")
			if tc.setup != nil {
				tc.setup(t, cu)
			}
			inner.ref = providers.Ref{URL: pr}
			clock := time.Unix(1_800_000_000, 0)
			script := []int{tc.code}
			if tc.code == 503 {
				script = []int{503, 503, 503}
			}
			cu.Filer.Client = newTestClient(t, &fakeFactory{ttl: time.Hour, script: script, newRoom: "abcdefgh"}, &clock)
			ref, err := cu.Curate(ctx, tc.first)
			must(t, err)
			if ref.URL != pr || len(inner.opened) != 1 {
				t.Fatalf("today's path opened the PR: %+v, %v", ref, inner.opened)
			}
			must(t, l.Upsert("k1", func(r *Record) { r.FixRoom, r.Verdict = "fix1", "persisting" }))

			cu.Filer.Client = clientAnswering(t, 201, "room0002") // the reset keeps the fingerprint
			next := drifted()
			ref, err = cu.Curate(ctx, next)
			must(t, err)
			r, _ := l.Get("k1")
			h := r.Curation
			if ref.URL != "" || len(inner.opened) != 1 || h == nil || h.ObserveOnly {
				t.Fatalf("ref %+v, opened %v, %+v", ref, inner.opened, h)
			}
			if h.Fingerprint != inner.opened[0] || h.Fingerprint == curator.DupFingerprint(next) {
				t.Fatalf("the hold adopts the PR opened under %s, got %s", inner.opened[0], h.Fingerprint)
			}
			if snapshotTitle(t, h) != next.Title {
				t.Fatalf("the snapshot is still this investigation: %q", snapshotTitle(t, h))
			}
		})
	}
}

func TestCurateAnOpenedPRDoesNotMoveTheDraftFingerprint(t *testing.T) {
	cu, inner, l := newCuration(t, "429")
	inner.ref = providers.Ref{URL: "https://github.com/o/kb/pull/9"}
	_, err := cu.Curate(ctx, incident("k1"))
	must(t, err)
	first, _ := l.Get("k1")
	cu.Filer.Client = clientAnswering(t, 429, "")
	next := incident("k1")
	next.Resource.Name = "another-workload"
	_, err = cu.Curate(ctx, next)
	must(t, err)
	if r, _ := l.Get("k1"); first.DraftFingerprint == "" || r.DraftFingerprint != first.DraftFingerprint {
		t.Fatalf("first %q, now %q", first.DraftFingerprint, r.DraftFingerprint)
	}
}

func TestCurateNoPRRecordsNoDraftFingerprint(t *testing.T) {
	cu, _, l := newCuration(t, "429")
	_, err := cu.Curate(ctx, incident("k1"))
	must(t, err)
	if r, ok := l.Get("k1"); ok && r.DraftFingerprint != "" {
		t.Fatalf("no PR, no fingerprint: %+v", r)
	}
	cu, inner, l := newCuration(t, "429")
	inner.ref = providers.Ref{URL: "https://github.com/o/kb/pull/9"}
	cu.Filer.Client = nil
	_, err = cu.Curate(ctx, incident(""))
	must(t, err)
	if _, ok := l.Get(""); ok {
		t.Fatal("no key, no record")
	}
}

func TestCurateAnOversizedSnapshotIsNotHeld(t *testing.T) {
	huge := incident("k1")
	huge.Fingerprint = strings.Repeat("x", maxSnapshotBytes) // a field the filing does not render, so the test stays fast
	t.Run("no hold: drafts at once", func(t *testing.T) {
		cu, inner, l := newCuration(t, "201")
		ref, err := cu.Curate(ctx, huge)
		must(t, err)
		if r, _ := l.Get("k1"); r.Curation != nil || inner.calls != 1 || ref != inner.ref {
			t.Fatalf("inner %d, %+v", inner.calls, r.Curation)
		}
	})
	t.Run("a standing hold keeps its snapshot", func(t *testing.T) {
		cu, inner, l := newCuration(t, "201")
		_, err := cu.Curate(ctx, incident("k1"))
		must(t, err)
		before, _ := l.Get("k1")
		cu.Filer.Client = clientAnswering(t, 201, "room0002")
		_, err = cu.Curate(ctx, huge)
		must(t, err)
		if r, _ := l.Get("k1"); inner.calls != 0 || !reflect.DeepEqual(r.Curation, before.Curation) {
			t.Fatalf("inner %d, %+v", inner.calls, r.Curation)
		}
	})
}

// breakLedger makes every later append to l fail.
func breakLedger(t *testing.T, l *Ledger) {
	t.Helper()
	_ = os.Remove(l.path)
	must(t, os.Mkdir(l.path, 0o700))
}

func TestCurateALostHoldDraftsAtOnce(t *testing.T) {
	t.Run("the hold's write fails", func(t *testing.T) {
		cu, inner, _ := newCuration(t, "201")
		bad, err := OpenLedger(filepath.Join(t.TempDir(), "bad.jsonl"), cu.Now)
		must(t, err)
		breakLedger(t, bad)
		cu.Ledger = bad // the filing's ledger stays healthy
		inner.ref = providers.Ref{URL: "https://github.com/o/kb/pull/9"}
		ref, err := cu.Curate(ctx, incident("k1"))
		if err != nil || ref != inner.ref || inner.calls != 1 {
			t.Fatalf("ref %+v, inner %d, %v", ref, inner.calls, err)
		}
	})
	t.Run("the filing's write fails", func(t *testing.T) {
		cu, inner, l := newCuration(t, "201")
		breakLedger(t, l)
		if _, err := cu.Curate(ctx, incident("k1")); err != nil || inner.calls != 1 {
			t.Fatalf("inner %d, %v", inner.calls, err)
		}
	})
	t.Run("a ledger with no path holds nothing", func(t *testing.T) {
		cu, inner, _ := newCuration(t, "201")
		noPath, err := OpenLedger("", cu.Now)
		must(t, err)
		cu.Ledger, cu.Filer.Ledger = noPath, noPath
		if _, err := cu.Curate(ctx, incident("k1")); err != nil || inner.calls != 1 {
			t.Fatalf("inner %d, %v", inner.calls, err)
		}
	})
	t.Run("a standing hold is not lost", func(t *testing.T) {
		cu, inner, l := newCuration(t, "201")
		_, err := cu.Curate(ctx, incident("k1"))
		must(t, err)
		breakLedger(t, l)
		cu.Filer.Client = clientAnswering(t, 200, "")
		if _, err := cu.Curate(ctx, incident("k1")); err != nil || inner.calls != 0 {
			t.Fatalf("the hold still stands, so nothing drafts: inner %d, %v", inner.calls, err)
		}
	})
	t.Run("a standing hold's update fails", func(t *testing.T) {
		cu, inner, l := newCuration(t, "201")
		_, err := cu.Curate(ctx, incident("k1"))
		must(t, err)
		before, _ := l.Get("k1")
		breakLedger(t, l)
		healthy, err := OpenLedger(filepath.Join(t.TempDir(), "ok.jsonl"), cu.Now)
		must(t, err)
		cu.Filer.Ledger = healthy
		cu.Filer.Client = clientAnswering(t, 201, "room0002")
		_, err = cu.Curate(ctx, incident("k1"))
		r, _ := l.Get("k1")
		if err != nil || inner.calls != 0 || !reflect.DeepEqual(r.Curation, before.Curation) {
			t.Fatalf("inner %d, %+v, %v", inner.calls, r.Curation, err)
		}
	})
}

func TestCurateLogsWhatTheFactoryDidNotTake(t *testing.T) {
	for _, tc := range []struct{ script, want string }{
		{"429", "level=INFO"},
		{"400", "level=WARN"},
		{"503,503,503", "level=WARN"},
	} {
		cu, _, _ := newCuration(t, tc.script)
		var logs bytes.Buffer
		cu.Filer.Log, cu.Log = slog.New(slog.NewTextHandler(&logs, nil)), slog.New(slog.NewTextHandler(&logs, nil))
		_, err := cu.Curate(ctx, incident("k1"))
		must(t, err)
		if !strings.Contains(logs.String(), tc.want) {
			t.Errorf("%s: %s", tc.script, logs.String())
		}
	}
}

func TestFileClearsTheSlackRootAtAReset(t *testing.T) {
	n, f, l, _ := newTestNotifier(t, []int{201, 201})
	got, err := n.File(ctx, testInvestigation())
	must(t, err)
	if got.Status != 201 || got.Room != "abcdefgh" {
		t.Fatalf("%+v", got)
	}
	l.Register("slack", "1729.0001", "C01", providers.Investigation{TriggerKey: "k1"})
	f.newRoom = "bcdefghi"
	_, err = n.File(ctx, testInvestigation())
	must(t, err)
	if r, _ := l.Get("k1"); r.Room != "bcdefghi" || r.SlackRoot != "" || r.SlackChannel != "" {
		t.Fatalf("%+v", r)
	}
}

func TestFileKeepsTheSlackRootOfTheSameRoom(t *testing.T) {
	n, _, l, _ := newTestNotifier(t, []int{201, 200})
	_, err := n.File(ctx, testInvestigation())
	must(t, err)
	l.Register("slack", "1729.0001", "C01", providers.Investigation{TriggerKey: "k1"})
	_, err = n.File(ctx, testInvestigation())
	must(t, err)
	if r, _ := l.Get("k1"); r.SlackRoot != "1729.0001" || r.SlackChannel != "C01" {
		t.Fatalf("%+v", r)
	}
}

func TestFileWithoutATriggerKeyPostsNothing(t *testing.T) {
	n, f, _, _ := newTestNotifier(t, []int{201})
	inv := testInvestigation()
	inv.TriggerKey = ""
	got, err := n.File(ctx, inv)
	if err != nil || got.Status != 0 || len(f.keys) != 0 {
		t.Fatalf("%+v, %v, %v", got, err, f.keys)
	}
}

func TestDeliverKeepsTheSlackRootAndTheHoldAcrossAReset(t *testing.T) {
	n, f, l, _ := newTestNotifier(t, []int{201, 201})
	must(t, n.Deliver(ctx, testInvestigation()))
	l.Register("slack", "1729.0001", "C01", providers.Investigation{TriggerKey: "k1"})
	hold := Hold{Fingerprint: "fp", Investigation: json.RawMessage(`{}`)}
	must(t, l.Upsert("k1", func(r *Record) {
		r.Curation = &hold
		r.FixRoom, r.Verdict = "fix1", "persisting"
	}))
	f.newRoom = "bcdefghi"
	must(t, n.Deliver(ctx, testInvestigation()))
	r, _ := l.Get("k1")
	if r.SlackRoot != "1729.0001" || r.SlackChannel != "C01" || r.FixRoom != "" || r.Curation == nil || r.Curation.Fingerprint != "fp" {
		t.Fatalf("%+v", r)
	}
	if len(r.Curation.Generations) != 1 || r.Curation.Generations[0].FixRoom != "fix1" {
		t.Fatalf("the closed generation is archived: %+v", r.Curation)
	}
}

func TestAResetWithNoFixArchivesNothing(t *testing.T) {
	n, f, l, _ := newTestNotifier(t, []int{201, 201})
	must(t, n.Deliver(ctx, testInvestigation()))
	must(t, l.Upsert("k1", func(r *Record) { r.Curation = &Hold{Fingerprint: "fp"} }))
	f.newRoom = "bcdefghi"
	must(t, n.Deliver(ctx, testInvestigation()))
	if r, _ := l.Get("k1"); r.Curation == nil || len(r.Curation.Generations) != 0 {
		t.Fatalf("%+v", r.Curation)
	}
}

// Run under -race: Held hands out clones, so a caller may write to what it got.
func TestHeldHandsOutClones(t *testing.T) {
	l, err := OpenLedger(filepath.Join(t.TempDir(), "f.jsonl"), time.Now)
	must(t, err)
	seed := func(r *Record) {
		r.Curation = &Hold{Investigation: json.RawMessage(`{}`), Generations: []Generation{{FixRoom: "fix0"}}}
	}
	must(t, l.Upsert("k0", seed))
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := range 50 {
			if err := l.Upsert("k0", func(r *Record) {
				r.Curation.Generations = append(r.Curation.Generations, Generation{FixRoom: fmt.Sprintf("fix%d", i)})
			}); err != nil {
				t.Error(err)
			}
		}
	})
	wg.Go(func() {
		for range 50 {
			for _, r := range l.Held() {
				r.Curation.Generations[0].FixRoom = "mine"
				r.Curation.Investigation[0] = 'x'
			}
		}
	})
	wg.Wait()
	r, _ := l.Get("k0")
	if r.Curation.Generations[0].FixRoom != "fix0" || string(r.Curation.Investigation) != `{}` {
		t.Fatalf("a clone's write reached the ledger: %+v", r.Curation)
	}
}
