// SPDX-License-Identifier: Apache-2.0

package outcome

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func mustNew(t *testing.T, path string) *Ledger {
	t.Helper()
	l, err := New(path)
	must(t, err)
	return l
}

func mustNewWithMaxEvents(t *testing.T, path string, n int) *Ledger {
	t.Helper()
	l, err := NewWithMaxEvents(path, n)
	must(t, err)
	return l
}

func TestVerifyIsAnObservation(t *testing.T) {
	p := filepath.Join(t.TempDir(), "outcomes.jsonl")
	l := mustNew(t, p)
	at := time.Unix(1_800_000_000, 0)
	obs := func(room string, held bool, d time.Duration) Verification {
		return Verification{Entry: "incidents/x.md", DupFingerprint: "fpA", TriggerKey: "k1", Room: room, Held: held, At: at.Add(d)}
	}
	factor := func(l *Ledger) float64 { c, _ := l.OpenCounts(); return c["incidents/x.md"].Factor(2) }
	for _, s := range []struct {
		v    Verification
		want float64
	}{
		{obs("fix1", true, 0), (Aggregate{Recalls: 1, Resolved: 1}).Factor(2)}, // weighted as a resolve: 2/3
		{obs("fix1", true, 0), 2.0 / 3},                                        // a replay counts once
		{obs("fix2", false, time.Hour), 0.5},                                   // persisting or regression lowers it
		{obs("fix1", false, 2*time.Hour), 0.25},                                // a revert moves fix1 to failed: latest wins
		{obs("fix1", true, 0), 0.25},                                           // an older replay after the flip stays failed
	} {
		if must(t, l.Verify(s.v)); factor(l) != s.want {
			t.Fatalf("%+v: %v", s.v, factor(l))
		}
	}
	for _, again := range []*Ledger{mustNew(t, p), mustNewWithMaxEvents(t, p, 2)} {
		if factor(again) != 0.25 || again.Verifications()["fpA"] {
			t.Fatal("reload and compaction keep the observations")
		}
	}
	// The compacted file is what a later start replays: read it back from the checkpoint.
	if fresh := mustNew(t, p); factor(fresh) != 0.25 || fresh.Verifications()["fpA"] {
		t.Fatal("a reopen after compaction keeps the observations")
	}
	must(t, l.Feedback("k1", "down", "U1", at))
	const url = "https://github.com/Smana/runlore-kb/pull/164"
	if must(t, l.Link("k1", url, at)); l.ContestedTriggers()[0].CuratedURL != url {
		t.Fatal("a deferred draft reaches Contested")
	}
	must(t, l.Open(Event{TriggerKey: "k1", Verdict: "action_required", At: at.Add(3 * time.Hour)})) // the next detection, CuratedURL empty
	if got := l.ContestedTriggers(); len(got) != 1 || got[0].CuratedURL != url {
		t.Fatalf("the next empty open keeps the link: %+v", got)
	}
	for _, again := range []*Ledger{mustNew(t, p), mustNewWithMaxEvents(t, p, 2)} {
		if got := again.ContestedTriggers(); len(got) != 1 || got[0].CuratedURL != url {
			t.Fatal("reload and compaction keep the link")
		}
	}
	fresh := mustNew(t, p)
	if got := fresh.ContestedTriggers(); len(got) != 1 || got[0].CuratedURL != url {
		t.Fatalf("a reopen after compaction keeps the link: %+v", got)
	}
	// The checkpoint's LinkedURL, not only its CuratedURL, must survive: one more empty open.
	must(t, fresh.Open(Event{TriggerKey: "k1", Verdict: "action_required", At: at.Add(4 * time.Hour)}))
	if got := fresh.ContestedTriggers(); len(got) != 1 || got[0].CuratedURL != url {
		t.Fatalf("an empty open after a reopen keeps the link: %+v", got)
	}
}

func TestVerificationsLatestPerFingerprint(t *testing.T) {
	l := mustNew(t, filepath.Join(t.TempDir(), "outcomes.jsonl"))
	at := time.Unix(1_800_000_000, 0)
	must(t, l.Verify(Verification{Entry: "a.md", DupFingerprint: "fp", Room: "r1", Held: true, At: at}))
	must(t, l.Verify(Verification{Entry: "a.md", DupFingerprint: "fp", Room: "r2", Held: false, At: at.Add(time.Minute)}))
	if l.Verifications()["fp"] {
		t.Fatal("the newest observation failed")
	}
	if err := l.Verify(Verification{Entry: "", Room: "r"}); err == nil {
		t.Fatal("an empty entry is refused")
	}
}

func TestLinkSetsConclusiveURLWhenEmptyAndKeepsCount(t *testing.T) {
	l := mustNew(t, filepath.Join(t.TempDir(), "outcomes.jsonl"))
	at := time.Unix(1_800_000_000, 0)
	must(t, l.Open(Event{Fingerprint: "f", TriggerKey: "k1", Verdict: "action_required", At: at}))
	const url = "https://github.com/Smana/runlore-kb/pull/9"
	must(t, l.Link("k1", url, at.Add(time.Minute)))
	n, _, got := l.Occurrences("k1")
	if n != 1 || got != url {
		t.Fatalf("count %d url %q: want 1 and the PR", n, got)
	}
	if c := l.Recurrence("k1").Conclusive.CuratedURL; c != url {
		t.Fatalf("conclusive URL %q: want the PR", c)
	}
	// A conclusive prior that already has a URL keeps it.
	must(t, l.Open(Event{Fingerprint: "g", TriggerKey: "k2", Verdict: "action_required", CuratedURL: "https://x/1", At: at}))
	must(t, l.Link("k2", url, at))
	if c := l.Recurrence("k2").Conclusive.CuratedURL; c != "https://x/1" {
		t.Fatalf("conclusive URL %q: want it unchanged", c)
	}
}

func TestMeasures(t *testing.T) {
	l := mustNew(t, filepath.Join(t.TempDir(), "outcomes.jsonl"))
	at := time.Unix(1_800_000_000, 0)
	must(t, l.Open(Event{Fingerprint: "old", TriggerKey: "k0", Kind: "fresh", Tokens: 999, At: at.Add(-time.Hour)}))
	must(t, l.Open(Event{Fingerprint: "fa", TriggerKey: "k1", Kind: "fresh", Tokens: 40000, At: at}))
	must(t, l.Open(Event{Fingerprint: "fb", TriggerKey: "k2", Kind: "recall", Entry: "e.md", Tokens: 6000, At: at.Add(time.Minute)}))
	must(t, l.Open(Event{Fingerprint: "fc", Kind: "fresh", Tokens: 5, At: at})) // no TriggerKey: not an incident
	if _, _, err := l.Resolve("fb", at.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	got, err := l.Measures(at)
	must(t, err)
	if want := (Measures{Incidents: 2, InstantRecalls: 1, RecallsResolved: 1, TokensPerIncident: 23000}); got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

// A compaction folds the oldest opens into a checkpoint, so a window reaching back past the
// retained tail undercounts: Measures says so rather than reporting a clean number.
func TestMeasuresFlagsAWindowThePartialTailCannotCover(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcomes.jsonl")
	l, err := NewWithMaxEvents(path, 4)
	must(t, err)
	at := time.Unix(1_800_000_000, 0)
	for i := 0; i < 6; i++ {
		must(t, l.Open(Event{Fingerprint: fmt.Sprintf("f%d", i), TriggerKey: fmt.Sprintf("k%d", i), Kind: "fresh", At: at.Add(time.Duration(i) * time.Hour)}))
	}
	l, err = NewWithMaxEvents(path, 4) // the reload compacts
	must(t, err)
	if m, err := l.Measures(time.Time{}); err != nil || !m.Partial || m.Incidents >= 6 {
		t.Fatalf("the whole history asked of a compacted ledger: %+v %v", m, err)
	}
	if m, err := l.Measures(at.Add(5 * time.Hour)); err != nil || m.Partial || m.Incidents != 1 {
		t.Fatalf("a window inside the retained tail is exact: %+v %v", m, err)
	}
	fresh := mustNew(t, filepath.Join(t.TempDir(), "o.jsonl"))
	must(t, fresh.Open(Event{Fingerprint: "a", TriggerKey: "k", Kind: "fresh", At: at}))
	if m, _ := fresh.Measures(time.Time{}); m.Partial {
		t.Fatal("a ledger never compacted is complete")
	}
}
