package outcome

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

// replayOpenCounts is an independent oracle: it computes the per-entry recall
// aggregate by replaying every event in the file from scratch, mirroring the
// documented Episodes()→OpenCounts() semantics (LIFO pairing, order-independent
// resolve-before-open buffering). The cached OpenCounts() must equal this for
// any event sequence.
func replayOpenCounts(t *testing.T, path string) map[string]Aggregate {
	t.Helper()
	ref, err := New(path)
	if err != nil {
		t.Fatalf("oracle New: %v", err)
	}
	eps, err := ref.Episodes() // Episodes still replays the file, so it is a fresh source of truth
	if err != nil {
		t.Fatalf("oracle Episodes: %v", err)
	}
	counts := map[string]Aggregate{}
	for _, e := range eps {
		if e.Kind != "recall" || e.Entry == "" {
			continue
		}
		a := counts[e.Entry]
		a.Recalls++
		if e.Resolved {
			a.Resolved++
			if e.ResolvedAt.After(a.LastConfirmed) {
				a.LastConfirmed = e.ResolvedAt
			}
		}
		counts[e.Entry] = a
	}
	return counts
}

func assertCacheEqualsReplay(t *testing.T, l *Ledger, path string) {
	t.Helper()
	cached, err := l.OpenCounts()
	if err != nil {
		t.Fatalf("OpenCounts: %v", err)
	}
	want := replayOpenCounts(t, path)
	if !reflect.DeepEqual(cached, want) {
		t.Fatalf("cache != replay\n cached=%+v\n replay=%+v", cached, want)
	}
}

// TestOpenCountsCacheEqualsReplaySequence drives a varied open/resolve sequence
// (recurrence, multiple entries, resolve-before-open, fresh, interleaving) and
// asserts the live cached OpenCounts() equals a from-scratch replay of the file.
func TestOpenCountsCacheEqualsReplaySequence(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(10000, 0)
	at := func(d int) time.Time { return t0.Add(time.Duration(d) * time.Second) }

	// recurrence on one entry: 3 opens, 1 resolve (LIFO ⇒ exactly 1 resolved)
	_ = l.Open(Event{Fingerprint: "fp1", Kind: "recall", Entry: "x.md", At: at(0)})
	_ = l.Open(Event{Fingerprint: "fp1", Kind: "recall", Entry: "x.md", At: at(1)})
	_ = l.Open(Event{Fingerprint: "fp1", Kind: "recall", Entry: "x.md", At: at(2)})
	_, _, _ = l.Resolve("fp1", at(5))
	// a fresh open (must never appear in counts)
	_ = l.Open(Event{Fingerprint: "fr", Kind: "fresh", At: at(6)})
	_, _, _ = l.Resolve("fr", at(7))
	// second entry, resolved once
	_ = l.Open(Event{Fingerprint: "fp2", Kind: "recall", Entry: "y.md", At: at(8)})
	_, _, _ = l.Resolve("fp2", at(9))
	// resolve-before-open: the resolve lands first, then the open pairs with it
	_, _, _ = l.Resolve("fp3", at(10))
	_ = l.Open(Event{Fingerprint: "fp3", Kind: "recall", Entry: "z.md", At: at(11)})
	// unresolved recall
	_ = l.Open(Event{Fingerprint: "fp4", Kind: "recall", Entry: "w.md", At: at(12)})

	assertCacheEqualsReplay(t, l, p)

	// Spot-check the documented shape too, not only DeepEqual against the oracle.
	counts, _ := l.OpenCounts()
	if got := counts["x.md"]; got.Recalls != 3 || got.Resolved != 1 || !got.LastConfirmed.Equal(at(5)) {
		t.Fatalf("x.md = %+v", got)
	}
	if got := counts["z.md"]; got.Recalls != 1 || got.Resolved != 1 {
		t.Fatalf("resolve-before-open z.md = %+v", got)
	}
	if got := counts["w.md"]; got.Recalls != 1 || got.Resolved != 0 {
		t.Fatalf("unresolved w.md = %+v", got)
	}
	if _, ok := counts["fresh"]; ok {
		t.Fatalf("fresh must not be counted: %+v", counts)
	}
}

// TestOpenCountsCacheReflectsAppendsAfterConstruction ensures the cache is
// updated INCREMENTALLY on appends made after New(), not only at load.
func TestOpenCountsCacheReflectsAppendsAfterConstruction(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p) // constructed over an empty/absent file
	if c, _ := l.OpenCounts(); len(c) != 0 {
		t.Fatalf("fresh ledger must start empty, got %+v", c)
	}
	t0 := time.Unix(11000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "a.md", At: t0})
	if c, _ := l.OpenCounts(); c["a.md"].Recalls != 1 || c["a.md"].Resolved != 0 {
		t.Fatalf("after Open, want recalls=1 resolved=0, got %+v", c["a.md"])
	}
	_, _, _ = l.Resolve("fp", t0.Add(time.Minute))
	if c, _ := l.OpenCounts(); c["a.md"].Resolved != 1 || !c["a.md"].LastConfirmed.Equal(t0.Add(time.Minute)) {
		t.Fatalf("after Resolve, want resolved=1 with LastConfirmed bumped, got %+v", c["a.md"])
	}
	assertCacheEqualsReplay(t, l, p)
}

// TestOpenCountsCacheBuiltFromExistingFile ensures opening a ledger over a file
// that already has events builds the cache once at construction (no append needed).
func TestOpenCountsCacheBuiltFromExistingFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	seed, _ := New(p)
	t0 := time.Unix(12000, 0)
	_ = seed.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "a.md", At: t0})
	_ = seed.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "a.md", At: t0.Add(time.Second)})
	_, _, _ = seed.Resolve("fp", t0.Add(5*time.Second))

	reopened, err := New(p)
	if err != nil {
		t.Fatalf("reopen New: %v", err)
	}
	c, _ := reopened.OpenCounts()
	if c["a.md"].Recalls != 2 || c["a.md"].Resolved != 1 || !c["a.md"].LastConfirmed.Equal(t0.Add(5*time.Second)) {
		t.Fatalf("reopened cache from file = %+v", c["a.md"])
	}
	assertCacheEqualsReplay(t, reopened, p)
}

// TestOpenCountsCacheSkipsCorruptLinesOnLoad ensures the initial cache build
// skips corrupt JSONL lines (matching readEvents' tolerance) rather than failing.
func TestOpenCountsCacheSkipsCorruptLinesOnLoad(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	t0 := time.Unix(13000, 0)
	good1, _ := json.Marshal(Event{Event: "open", Fingerprint: "fp", Kind: "recall", Entry: "a.md", At: t0})
	good2, _ := json.Marshal(Event{Event: "resolve", Fingerprint: "fp", At: t0.Add(time.Minute)})
	content := string(good1) + "\n" + "this is not json{{{\n" + string(good2) + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	l, err := New(p)
	if err != nil {
		t.Fatalf("New over file with a corrupt line must not fail: %v", err)
	}
	c, _ := l.OpenCounts()
	if c["a.md"].Recalls != 1 || c["a.md"].Resolved != 1 {
		t.Fatalf("corrupt line must be skipped, good events counted: %+v", c["a.md"])
	}
	assertCacheEqualsReplay(t, l, p)
}

// TestOpenCountsCacheRandomizedEqualsReplay fuzzes many random open/resolve
// sequences (across fingerprints and entries, including resolve-before-open) and
// asserts the cached OpenCounts() always equals a from-scratch replay.
func TestOpenCountsCacheRandomizedEqualsReplay(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	fps := []string{"f0", "f1", "f2"}
	entries := []string{"a.md", "b.md", ""} // "" ⇒ fresh-style open
	t0 := time.Unix(14000, 0)
	for trial := 0; trial < 40; trial++ {
		p := filepath.Join(t.TempDir(), fmt.Sprintf("o%d.jsonl", trial))
		l, _ := New(p)
		n := 5 + rng.Intn(25)
		for i := 0; i < n; i++ {
			fp := fps[rng.Intn(len(fps))]
			at := t0.Add(time.Duration(i) * time.Second)
			if rng.Intn(2) == 0 {
				entry := entries[rng.Intn(len(entries))]
				kind := "recall"
				if entry == "" {
					kind = "fresh"
				}
				_ = l.Open(Event{Fingerprint: fp, Kind: kind, Entry: entry, At: at})
			} else {
				_, _, _ = l.Resolve(fp, at)
			}
		}
		assertCacheEqualsReplay(t, l, p)
	}
}

// TestOpenCountsCacheConcurrent exercises concurrent Open/Resolve and OpenCounts
// to catch data races (run under -race). It asserts the final cached aggregate
// equals a from-scratch replay of the file.
func TestOpenCountsCacheConcurrent(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(15000, 0)
	const writers = 8
	const perWriter = 50
	var writersWG, readersWG sync.WaitGroup

	// readers hammer OpenCounts concurrently with the writers, until stop is closed
	stop := make(chan struct{})
	for r := 0; r < 4; r++ {
		readersWG.Add(1)
		go func() {
			defer readersWG.Done()
			for {
				select {
				case <-stop:
					return
				default:
					if _, err := l.OpenCounts(); err != nil {
						t.Errorf("OpenCounts: %v", err)
						return
					}
				}
			}
		}()
	}

	for w := 0; w < writers; w++ {
		writersWG.Add(1)
		go func(w int) {
			defer writersWG.Done()
			fp := fmt.Sprintf("fp%d", w)
			entry := fmt.Sprintf("e%d.md", w)
			for i := 0; i < perWriter; i++ {
				at := t0.Add(time.Duration(w*perWriter+i) * time.Second)
				_ = l.Open(Event{Fingerprint: fp, Kind: "recall", Entry: entry, At: at})
				_, _, _ = l.Resolve(fp, at.Add(time.Millisecond))
			}
		}(w)
	}

	writersWG.Wait() // all appends done
	close(stop)      // let readers exit
	readersWG.Wait()

	assertCacheEqualsReplay(t, l, p)
	c, _ := l.OpenCounts()
	for w := 0; w < writers; w++ {
		entry := fmt.Sprintf("e%d.md", w)
		if c[entry].Recalls != perWriter || c[entry].Resolved != perWriter {
			t.Fatalf("%s: want recalls=%d resolved=%d, got %+v", entry, perWriter, perWriter, c[entry])
		}
	}
}

func TestStatusDisabled(t *testing.T) {
	l, _ := New("")
	s := l.Status()
	if s.Configured {
		t.Fatalf("empty path must be Configured=false: %+v", s)
	}
	if s.Present || s.Events != 0 {
		t.Fatalf("disabled ledger: want Present=false Events=0, got %+v", s)
	}
}

func TestStatusConfiguredButAbsent(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "missing.jsonl"))
	s := l.Status()
	if !s.Configured {
		t.Fatalf("a non-empty path must be Configured=true: %+v", s)
	}
	if s.Present {
		t.Fatalf("an absent file must be Present=false (this is the silent no-op the warning catches): %+v", s)
	}
	if s.Events != 0 {
		t.Fatalf("absent file: want Events=0, got %d", s.Events)
	}
}

func TestStatusPresentWithEvents(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(1000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0})
	_, _, _ = l.Resolve("fp", t0.Add(time.Minute))
	s := l.Status()
	if !s.Configured || !s.Present {
		t.Fatalf("a written ledger must be Configured && Present: %+v", s)
	}
	if s.Events != 2 {
		t.Fatalf("want Events=2 (1 open + 1 resolve), got %d", s.Events)
	}
}

func TestStatusPresentButEmptyFile(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	if err := os.WriteFile(p, nil, 0o644); err != nil {
		t.Fatalf("seed empty file: %v", err)
	}
	l, _ := New(p)
	s := l.Status()
	if !s.Configured || !s.Present {
		t.Fatalf("an existing (empty) file is Present: %+v", s)
	}
	if s.Events != 0 {
		t.Fatalf("empty file: want Events=0, got %d", s.Events)
	}
}

func TestLedgerOpenResolveRoundTrip(t *testing.T) {
	p := filepath.Join(t.TempDir(), "outcomes.jsonl")
	l, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t0 := time.Unix(1000, 0)
	if err := l.Open(Event{Fingerprint: "fp1", Kind: "recall", Entry: "harbor.md", At: t0}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	ep, ok, err := l.Resolve("fp1", t0.Add(90*time.Second))
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if ep.Kind != "recall" || ep.Entry != "harbor.md" || ep.Duration != 90*time.Second || !ep.Resolved {
		t.Fatalf("episode: %+v", ep)
	}
}

func TestLedgerResolveWithoutOpen(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	if _, ok, err := l.Resolve("never-fired", time.Unix(1, 0)); ok || err != nil {
		t.Fatalf("resolve with no open should be ok=false, got ok=%v err=%v", ok, err)
	}
}

func TestLedgerReplayRebuildsOpenIndex(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(2000, 0)
	_ = l.Open(Event{Fingerprint: "fpA", Kind: "fresh", At: t0})
	// New ledger over the same file replays the open event.
	l2, err := New(p)
	if err != nil {
		t.Fatalf("replay New: %v", err)
	}
	if _, ok, _ := l2.Resolve("fpA", t0.Add(time.Minute)); !ok {
		t.Fatal("replay must rebuild the open-index so fpA resolves")
	}
}

func TestLedgerDisabledWhenPathEmpty(t *testing.T) {
	l, err := New("")
	if err != nil {
		t.Fatalf("New(\"\"): %v", err)
	}
	if err := l.Open(Event{Fingerprint: "x"}); err != nil {
		t.Fatalf("Open on disabled ledger must be a no-op, got %v", err)
	}
	if _, ok, _ := l.Resolve("x", time.Now()); ok {
		t.Fatal("disabled ledger Resolve must be ok=false")
	}
}

func TestReadEventsReturnsAllInOrder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t0 := time.Unix(3000, 0)
	_ = l.Open(Event{Fingerprint: "a", Kind: "fresh", At: t0})
	_ = l.Open(Event{Fingerprint: "b", Kind: "recall", Entry: "x.md", At: t0.Add(time.Second)})
	_, _, _ = l.Resolve("a", t0.Add(2*time.Second))
	events, err := l.readEvents()
	if err != nil {
		t.Fatalf("readEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d: %+v", len(events), events)
	}
	if events[0].Event != "open" || events[0].Fingerprint != "a" {
		t.Fatalf("event[0] = %+v", events[0])
	}
	if events[2].Event != "resolve" || events[2].Fingerprint != "a" {
		t.Fatalf("event[2] = %+v", events[2])
	}
}

func TestReadEventsDisabledOrAbsent(t *testing.T) {
	dis, _ := New("")
	if ev, err := dis.readEvents(); err != nil || ev != nil {
		t.Fatalf("disabled: want nil,nil; got %v,%v", ev, err)
	}
	absent, _ := New(filepath.Join(t.TempDir(), "missing.jsonl"))
	if ev, err := absent.readEvents(); err != nil || ev != nil {
		t.Fatalf("absent file: want nil,nil; got %v,%v", ev, err)
	}
}

func TestResolveMarksEpisodeResolved(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(4000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "e.md", At: t0})
	ep, ok, err := l.Resolve("fp", t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if !ep.Resolved {
		t.Fatalf("a matched resolve must set Resolved=true: %+v", ep)
	}
}

func TestEpisodesReconstructsRecurrence(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(5000, 0)
	for i := 0; i < 3; i++ {
		_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0.Add(time.Duration(i) * time.Second)})
	}
	_, _, _ = l.Resolve("fp", t0.Add(10*time.Second))
	eps, err := l.Episodes()
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 3 {
		t.Fatalf("want 3 episodes (recurrence preserved), got %d", len(eps))
	}
	resolved := 0
	for _, e := range eps {
		if e.Resolved {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("want exactly 1 resolved episode, got %d", resolved)
	}
}

func TestEpisodesResolvedPairingAndDuration(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(6000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "first.md", At: t0})
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "second.md", At: t0.Add(time.Second)})
	_, _, _ = l.Resolve("fp", t0.Add(5*time.Second))
	eps, _ := l.Episodes()
	var second Episode
	for _, e := range eps {
		if e.Entry == "first.md" && e.Resolved {
			t.Fatal("LIFO: the earlier open must remain unresolved")
		}
		if e.Entry == "second.md" {
			second = e
		}
	}
	if !second.Resolved || second.Duration != 4*time.Second {
		t.Fatalf("most-recent open should resolve with 4s duration: %+v", second)
	}
}

func TestEpisodesEmptyAndDisabled(t *testing.T) {
	dis, _ := New("")
	if eps, err := dis.Episodes(); err != nil || eps != nil {
		t.Fatalf("disabled: want nil,nil; got %v,%v", eps, err)
	}
	empty, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	if eps, err := empty.Episodes(); err != nil || len(eps) != 0 {
		t.Fatalf("empty ledger: want 0 episodes; got %v,%v", eps, err)
	}
}

// TestEpisodesResolveBeforeOpenPairs verifies the new order-independent pairing:
// a resolve written BEFORE its open (a transient incident that cleared
// mid-investigation, so the resolve webhook landed before the open was recorded)
// now PAIRS with the later open — the open becomes a resolved episode.
func TestEpisodesResolveBeforeOpenPairs(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(9000, 0)
	_, _, _ = l.Resolve("fp", t0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0.Add(time.Second)})
	eps, err := l.Episodes()
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 1 || !eps[0].Resolved {
		t.Fatalf("resolve-before-open must pair: want 1 resolved episode, got %+v", eps)
	}
	// Resolve predates the open here, so Duration is guarded to 0 (never negative).
	if eps[0].Duration != 0 {
		t.Fatalf("duration must be guarded non-negative when resolve predates open: %v", eps[0].Duration)
	}
}

// TestEpisodesCoalescedMultiFingerprintResolves checks the ledger-level shape of
// a coalesced attribution: opening the same Title/Kind for several constituent
// fingerprints, then a resolve for any one of them, marks that episode resolved.
func TestEpisodesCoalescedMultiFingerprintResolves(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(9500, 0)
	for _, fp := range []string{"a", "b", "c"} {
		if err := l.Open(Event{Fingerprint: fp, Kind: "recall", Entry: "x.md", Title: "BatchAlert", At: t0}); err != nil {
			t.Fatalf("Open(%s): %v", fp, err)
		}
	}
	ep, ok, err := l.Resolve("c", t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("Resolve(c): want ok=true, got ok=%v err=%v", ok, err)
	}
	if !ep.Resolved || ep.Title != "BatchAlert" {
		t.Fatalf("constituent resolve must mark its episode resolved: %+v", ep)
	}
}

func TestEpisodesTwoFingerprintsInterleaved(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(9100, 0)
	_ = l.Open(Event{Fingerprint: "A", Kind: "recall", Entry: "a.md", At: t0})
	_ = l.Open(Event{Fingerprint: "B", Kind: "recall", Entry: "b.md", At: t0.Add(time.Second)})
	_, _, _ = l.Resolve("A", t0.Add(2*time.Second))
	eps, _ := l.Episodes()
	if len(eps) != 2 {
		t.Fatalf("want 2 episodes, got %d", len(eps))
	}
	for _, e := range eps {
		if e.Entry == "a.md" && !e.Resolved {
			t.Fatalf("A should be resolved: %+v", e)
		}
		if e.Entry == "b.md" && e.Resolved {
			t.Fatalf("B should remain unresolved: %+v", e)
		}
	}
}

func TestOpenCountsAggregatesRecallEntries(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(7000, 0)
	for i := 0; i < 3; i++ {
		_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0.Add(time.Duration(i) * time.Second)})
	}
	resolveAt := t0.Add(10 * time.Second)
	_, _, _ = l.Resolve("fp", resolveAt)
	counts, err := l.OpenCounts()
	if err != nil {
		t.Fatalf("OpenCounts: %v", err)
	}
	a := counts["x.md"]
	if a.Recalls != 3 || a.Resolved != 1 {
		t.Fatalf("want recalls=3 resolved=1, got %+v", a)
	}
	if !a.LastConfirmed.Equal(resolveAt) {
		t.Fatalf("LastConfirmed = %v, want %v", a.LastConfirmed, resolveAt)
	}
}

func TestOpenCountsSkipsFresh(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(8000, 0)
	_ = l.Open(Event{Fingerprint: "f1", Kind: "fresh", At: t0}) // fresh → no entry
	_, _, _ = l.Resolve("f1", t0.Add(time.Minute))
	counts, _ := l.OpenCounts()
	if len(counts) != 0 {
		t.Fatalf("fresh opens must not appear in OpenCounts, got %+v", counts)
	}
}

func TestOpenCountsEmptyMapWhenDisabled(t *testing.T) {
	dis, _ := New("")
	counts, err := dis.OpenCounts()
	if err != nil || counts == nil || len(counts) != 0 {
		t.Fatalf("disabled: want empty non-nil map; got %v,%v", counts, err)
	}
}

func TestOpenCountsUnresolvedRecallCounted(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(7100, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0}) // never resolved
	counts, err := l.OpenCounts()
	if err != nil {
		t.Fatalf("OpenCounts: %v", err)
	}
	a := counts["x.md"]
	if a.Recalls != 1 || a.Resolved != 0 {
		t.Fatalf("unresolved recall: want recalls=1 resolved=0, got %+v", a)
	}
	if !a.LastConfirmed.IsZero() {
		t.Fatalf("unresolved recall: LastConfirmed must be zero, got %v", a.LastConfirmed)
	}
}

func TestOpenCountsMultipleEntries(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(7200, 0)
	_ = l.Open(Event{Fingerprint: "A", Kind: "recall", Entry: "a.md", At: t0})
	_ = l.Open(Event{Fingerprint: "B", Kind: "recall", Entry: "b.md", At: t0.Add(time.Second)})
	_, _, _ = l.Resolve("A", t0.Add(2*time.Second))
	counts, _ := l.OpenCounts()
	if len(counts) != 2 {
		t.Fatalf("want 2 entries, got %d: %+v", len(counts), counts)
	}
	if counts["a.md"].Resolved != 1 || counts["a.md"].Recalls != 1 {
		t.Fatalf("a.md should be recalls=1 resolved=1: %+v", counts["a.md"])
	}
	if counts["b.md"].Recalls != 1 || counts["b.md"].Resolved != 0 {
		t.Fatalf("b.md should be recalls=1 resolved=0: %+v", counts["b.md"])
	}
}

func TestEpisodeCarriesDupFingerprint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "outcomes.jsonl")
	l, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t0 := time.Unix(2000, 0)
	if err := l.Open(Event{Fingerprint: "fp1", DupFingerprint: "dup-abc", Title: "T", At: t0}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// live Resolve carries it through
	ep, ok, err := l.Resolve("fp1", t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if ep.DupFingerprint != "dup-abc" {
		t.Fatalf("Resolve episode dup = %q, want dup-abc", ep.DupFingerprint)
	}
	// replayed Episodes() carries it too
	eps, err := l.Episodes()
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 1 || eps[0].DupFingerprint != "dup-abc" {
		t.Fatalf("Episodes dup = %+v, want one with dup-abc", eps)
	}
}
