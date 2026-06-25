package outcome

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

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
