// SPDX-License-Identifier: Apache-2.0

package outcome

import (
	"path/filepath"
	"testing"
	"time"
)

func newTestLedger(t *testing.T) *Ledger {
	t.Helper()
	l, err := New(filepath.Join(t.TempDir(), "outcome.jsonl"))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return l
}

// TestSilenceIsReadBackAsAnExpiry: the whole point of the event kind.
func TestSilenceIsReadBackAsAnExpiry(t *testing.T) {
	l := newTestLedger(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := l.Silence("ns/app:CrashLoop", 4*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	got := l.Recurrence("ns/app:CrashLoop").SilencedUntil
	if want := now.Add(4 * time.Hour); !got.Equal(want) {
		t.Errorf("SilencedUntil = %v, want %v", got, want)
	}
}

// TestSilenceLatestWinsPerTrigger pins the dedup rule that differs from votes:
// a silence is ONE standing decision about the trigger, not a per-user opinion,
// so a second click REPLACES the first — including shortening it.
func TestSilenceLatestWinsPerTrigger(t *testing.T) {
	l := newTestLedger(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	if err := l.Silence("k", 24*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	// A DIFFERENT user, a SHORTER window, one minute later.
	if err := l.Silence("k", 1*time.Hour, "U2", now.Add(time.Minute)); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	got := l.Recurrence("k").SilencedUntil
	if want := now.Add(time.Minute).Add(1 * time.Hour); !got.Equal(want) {
		t.Errorf("SilencedUntil = %v, want the SECOND silence %v", got, want)
	}
}

func TestSilenceRejectsBadWindows(t *testing.T) {
	l := newTestLedger(t)
	l.MaxSilenceWindow = 24 * time.Hour
	now := time.Now()
	for _, tc := range []struct {
		name   string
		window time.Duration
	}{
		{"zero", 0},
		{"negative", -time.Hour},
		{"above the cap", 25 * time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := l.Silence("k", tc.window, "U1", now); err == nil {
				t.Fatal("want an error, got nil")
			}
			if !l.Recurrence("k").SilencedUntil.IsZero() {
				t.Error("a rejected silence must not be folded")
			}
		})
	}
}

// TestSilenceUnattributableIsRejected: with no TriggerKey there is nothing to
// key the suppression on, so the write must fail loudly rather than append a
// line that can never be read back.
func TestSilenceUnattributableIsRejected(t *testing.T) {
	l := newTestLedger(t)
	if err := l.Silence("", time.Hour, "U1", time.Now()); err == nil {
		t.Fatal("want an error for an empty trigger key, got nil")
	}
}

// TestSilenceSurvivesReload proves the state is reconstructible from the file
// alone — the property every leadership failover depends on.
func TestSilenceSurvivesReload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "outcome.jsonl")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	first, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := first.Silence("k", 4*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}

	second, err := New(path) // a fresh process replaying the same file
	if err != nil {
		t.Fatalf("New (reload): %v", err)
	}
	if got, want := second.Recurrence("k").SilencedUntil, now.Add(4*time.Hour); !got.Equal(want) {
		t.Errorf("after reload SilencedUntil = %v, want %v", got, want)
	}
}

// TestSilenceSurvivesCompaction is the load-bearing one: compaction rewrites the
// file as [checkpoint][tail], so a silence older than the horizon exists ONLY in
// the checkpoint. Forgetting checkpointData.Silences makes RunLore start talking
// again mid-window, silently.
func TestSilenceSurvivesCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcome.jsonl")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	// maxEvents low enough that the silence is certain to fall before the horizon.
	l, err := NewWithMaxEvents(path, 8)
	if err != nil {
		t.Fatalf("NewWithMaxEvents: %v", err)
	}
	if err := l.Silence("k", 24*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	// Push the silence past the compaction horizon with unrelated opens.
	for i := 0; i < 20; i++ {
		if err := l.Open(Event{Fingerprint: "fp", Title: "noise", At: now}); err != nil {
			t.Fatalf("Open: %v", err)
		}
	}

	// THREE loads are required, and the reason is subtle enough to be worth
	// stating: compactLocked runs only inside loadLocked, and only AFTER the full
	// event list has already been folded into memory. So the load that COMPACTS
	// still holds correct state whether or not the checkpoint captured anything —
	// asserting on it would pass with a completely broken snapshotCheckpointLocked.
	// Only the load that replays [checkpoint][tail] actually reads the checkpoint.
	compacting, err := NewWithMaxEvents(path, 8) // folds all 21, THEN rewrites the file
	if err != nil {
		t.Fatalf("NewWithMaxEvents (compacting load): %v", err)
	}
	_ = compacting

	replayed, err := NewWithMaxEvents(path, 8) // reads [checkpoint][tail] — the real assertion
	if err != nil {
		t.Fatalf("NewWithMaxEvents (replaying load): %v", err)
	}
	if got, want := replayed.Recurrence("k").SilencedUntil, now.Add(24*time.Hour); !got.Equal(want) {
		t.Errorf("after compaction SilencedUntil = %v, want %v — the checkpoint dropped it", got, want)
	}
}

// TestResolveClearsTheSilence: the incident actually went away, so the silence
// has served its purpose; a later firing is arguably a new occurrence and
// deserves a fresh look.
func TestResolveClearsTheSilence(t *testing.T) {
	l := newTestLedger(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	if err := l.Open(Event{Fingerprint: "fp1", TriggerKey: "k", Title: "boom", At: now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Silence("k", 24*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	if l.Recurrence("k").SilencedUntil.IsZero() {
		t.Fatal("precondition: the silence should stand before the resolve")
	}

	if _, _, err := l.Resolve("fp1", now.Add(time.Minute)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got := l.Recurrence("k").SilencedUntil; !got.IsZero() {
		t.Errorf("SilencedUntil = %v after a resolve, want zero", got)
	}
}

// TestResolveOfAnUnrelatedFingerprintKeepsTheSilence guards the obvious
// over-reach: one incident ending must not un-silence a different one.
func TestResolveOfAnUnrelatedFingerprintKeepsTheSilence(t *testing.T) {
	l := newTestLedger(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	if err := l.Open(Event{Fingerprint: "fp1", TriggerKey: "k", At: now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Open(Event{Fingerprint: "fp2", TriggerKey: "other", At: now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Silence("k", 24*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	if _, _, err := l.Resolve("fp2", now.Add(time.Minute)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if l.Recurrence("k").SilencedUntil.IsZero() {
		t.Error("an unrelated resolve cleared the silence")
	}
}

// TestResolveClearingSurvivesReplay: the live path and the file replay must
// agree, or a leadership failover would resurrect a silence a resolve cleared.
func TestResolveClearingSurvivesReplay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcome.jsonl")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	l, err := New(path)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := l.Open(Event{Fingerprint: "fp1", TriggerKey: "k", At: now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Silence("k", 24*time.Hour, "U1", now); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	if _, _, err := l.Resolve("fp1", now.Add(time.Minute)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}

	reloaded, err := New(path)
	if err != nil {
		t.Fatalf("New (reload): %v", err)
	}
	if got := reloaded.Recurrence("k").SilencedUntil; !got.IsZero() {
		t.Errorf("after replay SilencedUntil = %v, want zero — replay disagrees with the live path", got)
	}
}

// TestSilenceNilReceiverIsSafe pins Finding 1(b) from the Task 5 review: Silence
// must degrade gracefully on a nil *Ledger, the same way Feedback already does
// via its nil-safe enabled() check. Before the fix, Silence read
// l.MaxSilenceWindow — a plain field dereference — before ever calling
// l.enabled(), so a nil receiver panicked instead of quietly no-opping.
func TestSilenceNilReceiverIsSafe(t *testing.T) {
	var l *Ledger
	if err := l.Silence("k", time.Hour, "U1", time.Now()); err != nil {
		t.Fatalf("Silence on a nil *Ledger returned %v, want nil (quiet no-op)", err)
	}
}

// TestNewestHumanWinsBetweenFeedbackAndSilence pins the ordering rule the
// suppression gate reads: a silence recorded AFTER the newest standing 👎
// outranks it, and a 👎 cast AFTER a silence outranks that. The snapshot has to
// carry both timestamps for the gate to be able to tell, which is exactly what
// was missing — Contested() compares no times at all, so a Monday 👎 outranked
// every later silence forever.
func TestNewestHumanWinsBetweenFeedbackAndSilence(t *testing.T) {
	t.Run("a silence recorded after the thumbs-down is the newer human", func(t *testing.T) {
		l := newTestLedger(t)
		mon := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
		if err := l.Feedback("k", "down", "U1", mon); err != nil {
			t.Fatalf("Feedback: %v", err)
		}
		if err := l.Silence("k", 24*time.Hour, "U2", mon.Add(24*time.Hour)); err != nil {
			t.Fatalf("Silence: %v", err)
		}
		r := l.Recurrence("k")
		if r.FeedbackDown != 1 {
			t.Fatalf("FeedbackDown = %d, want 1 (Contested must keep its meaning)", r.FeedbackDown)
		}
		if !r.FeedbackDownLatest.Equal(mon) {
			t.Errorf("FeedbackDownLatest = %v, want %v", r.FeedbackDownLatest, mon)
		}
		if !r.SilencedAt.Equal(mon.Add(24 * time.Hour)) {
			t.Errorf("SilencedAt = %v, want %v", r.SilencedAt, mon.Add(24*time.Hour))
		}
		if !r.SilenceOutranksFeedback() {
			t.Error("SilenceOutranksFeedback() = false, want true — the silence is the newer human")
		}
	})

	t.Run("a thumbs-down cast after the silence re-arms it", func(t *testing.T) {
		l := newTestLedger(t)
		now := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
		if err := l.Silence("k", 24*time.Hour, "U2", now); err != nil {
			t.Fatalf("Silence: %v", err)
		}
		if err := l.Feedback("k", "down", "U1", now.Add(time.Hour)); err != nil {
			t.Fatalf("Feedback: %v", err)
		}
		if r := l.Recurrence("k"); r.SilenceOutranksFeedback() {
			t.Error("SilenceOutranksFeedback() = true, want false — the 👎 is the newer human")
		}
	})

	t.Run("a repeat thumbs-down after a silence still re-arms it", func(t *testing.T) {
		// The vote's fold is idempotent on the AGGREGATE, but its TIME must still
		// move: re-clicking 👎 after a silence is a fresh human refusal, and freezing
		// the timestamp at the first click would leave the silence outranking it.
		l := newTestLedger(t)
		now := time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
		if err := l.Feedback("k", "down", "U1", now); err != nil {
			t.Fatalf("Feedback: %v", err)
		}
		if err := l.Silence("k", 24*time.Hour, "U2", now.Add(time.Hour)); err != nil {
			t.Fatalf("Silence: %v", err)
		}
		if err := l.Feedback("k", "down", "U1", now.Add(2*time.Hour)); err != nil {
			t.Fatalf("Feedback (repeat): %v", err)
		}
		r := l.Recurrence("k")
		if r.FeedbackDown != 1 {
			t.Errorf("FeedbackDown = %d, want 1 — a repeat vote must not stack", r.FeedbackDown)
		}
		if r.SilenceOutranksFeedback() {
			t.Error("SilenceOutranksFeedback() = true, want false — the repeat 👎 is the newest human")
		}
	})

	t.Run("an unknown ordering leaves the thumbs-down standing", func(t *testing.T) {
		// A checkpoint written before either timestamp existed replays a live 👎 with
		// a zero time. Unknown ordering must degrade to "the 👎 wins" — RunLore
		// investigates — never to a silence that cannot be argued for.
		var r TriggerRecurrence
		r.FeedbackDown = 1
		r.SilencedAt = time.Date(2026, 8, 24, 9, 0, 0, 0, time.UTC)
		if r.SilenceOutranksFeedback() {
			t.Error("SilenceOutranksFeedback() = true with an unknown 👎 time, want false")
		}
		r.FeedbackDownLatest = r.SilencedAt.Add(-time.Hour)
		r.SilencedAt = time.Time{}
		if r.SilenceOutranksFeedback() {
			t.Error("SilenceOutranksFeedback() = true with an unknown silence time, want false")
		}
	})

	t.Run("no thumbs-down at all: nothing to outrank", func(t *testing.T) {
		var r TriggerRecurrence
		if !r.SilenceOutranksFeedback() {
			t.Error("SilenceOutranksFeedback() = false with no 👎 standing, want true")
		}
	})
}

// TestVoteAndSilenceTimesSurviveCompaction is the load-bearing round-trip: both
// timestamps live only in the checkpoint once their events fall past the
// horizon, and losing either silently flips the ordering rule. THREE loads, for
// the reason TestSilenceSurvivesCompaction states.
func TestVoteAndSilenceTimesSurviveCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcome.jsonl")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	l, err := NewWithMaxEvents(path, 8)
	if err != nil {
		t.Fatalf("NewWithMaxEvents: %v", err)
	}
	if err := l.Feedback("k", "down", "U1", now); err != nil {
		t.Fatalf("Feedback: %v", err)
	}
	if err := l.Silence("k", 24*time.Hour, "U2", now.Add(time.Hour)); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	for i := 0; i < 20; i++ {
		if err := l.Open(Event{Fingerprint: "fp", Title: "noise", At: now}); err != nil {
			t.Fatalf("Open: %v", err)
		}
	}

	compacting, err := NewWithMaxEvents(path, 8) // folds all 22, THEN rewrites the file
	if err != nil {
		t.Fatalf("NewWithMaxEvents (compacting load): %v", err)
	}
	_ = compacting

	replayed, err := NewWithMaxEvents(path, 8) // reads [checkpoint][tail] — the real assertion
	if err != nil {
		t.Fatalf("NewWithMaxEvents (replaying load): %v", err)
	}
	r := replayed.Recurrence("k")
	if !r.FeedbackDownLatest.Equal(now) {
		t.Errorf("after compaction FeedbackDownLatest = %v, want %v — the checkpoint dropped the vote time", r.FeedbackDownLatest, now)
	}
	if !r.SilencedAt.Equal(now.Add(time.Hour)) {
		t.Errorf("after compaction SilencedAt = %v, want %v — the checkpoint dropped the silence time", r.SilencedAt, now.Add(time.Hour))
	}
	if !r.SilenceOutranksFeedback() {
		t.Error("after compaction the silence no longer outranks the older 👎")
	}
}

// TestResolveClearsTheSilenceWithNoLiveOpen walks the reachable path that killed
// the resolve escape outright. applyResolveLocked reached the TriggerKey through
// l.open[fp], which holds UNRESOLVED opens only — and the suppressed path
// deliberately records no open at all:
//
//	fire → investigate → resolve   (the open is paired and deleted)
//	🔕 clicked on the scrolled-back card
//	fire again → SUPPRESSED, no ledger open recorded
//	resolve again → l.open[fp] is empty → tk == "" → the silence survives
//
// so for that silence the resolve escape was permanently dead and only the
// expiry remained.
func TestResolveClearsTheSilenceWithNoLiveOpen(t *testing.T) {
	l := newTestLedger(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	if err := l.Open(Event{Fingerprint: "fp1", TriggerKey: "k", Title: "boom", At: now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, _, err := l.Resolve("fp1", now.Add(time.Minute)); err != nil {
		t.Fatalf("Resolve (first episode): %v", err)
	}
	// An hour later the on-call scrolls back and clicks 🔕 on that card.
	if err := l.Silence("k", 24*time.Hour, "U1", now.Add(time.Hour)); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	// The trigger fires again and is suppressed: NO open is recorded, by design.
	if _, _, err := l.Resolve("fp1", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("Resolve (second episode): %v", err)
	}
	if got := l.Recurrence("k").SilencedUntil; !got.IsZero() {
		t.Errorf("SilencedUntil = %v after the incident resolved, want zero — the resolve escape is dead", got)
	}
}

// TestResolveWithNoLiveOpenKeepsAnUnrelatedSilence guards the fallback lookup
// from over-reaching: it must clear the silence on the trigger whose own
// investigation carried this fingerprint, and no other.
func TestResolveWithNoLiveOpenKeepsAnUnrelatedSilence(t *testing.T) {
	l := newTestLedger(t)
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	if err := l.Open(Event{Fingerprint: "fp1", TriggerKey: "k", At: now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if err := l.Open(Event{Fingerprint: "fp2", TriggerKey: "other", At: now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, _, err := l.Resolve("fp1", now.Add(time.Minute)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if _, _, err := l.Resolve("fp2", now.Add(time.Minute)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := l.Silence("k", 24*time.Hour, "U1", now.Add(time.Hour)); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	if _, _, err := l.Resolve("fp2", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("Resolve (unrelated): %v", err)
	}
	if l.Recurrence("k").SilencedUntil.IsZero() {
		t.Error("an unrelated resolve cleared the silence")
	}
}

// TestResolveWithNoLiveOpenSurvivesCompaction: the fingerprint the fallback
// looks up lives on the per-trigger index, which is checkpointed. Drop it there
// and the escape works right up until the first compaction, then silently stops.
func TestResolveWithNoLiveOpenSurvivesCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcome.jsonl")
	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)

	l, err := NewWithMaxEvents(path, 8)
	if err != nil {
		t.Fatalf("NewWithMaxEvents: %v", err)
	}
	if err := l.Open(Event{Fingerprint: "fp1", TriggerKey: "k", At: now}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	if _, _, err := l.Resolve("fp1", now.Add(time.Minute)); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if err := l.Silence("k", 24*time.Hour, "U1", now.Add(time.Hour)); err != nil {
		t.Fatalf("Silence: %v", err)
	}
	for i := 0; i < 20; i++ {
		if err := l.Open(Event{Fingerprint: "noise", Title: "noise", At: now}); err != nil {
			t.Fatalf("Open: %v", err)
		}
	}
	compacting, err := NewWithMaxEvents(path, 8) // folds everything, THEN rewrites the file
	if err != nil {
		t.Fatalf("NewWithMaxEvents (compacting load): %v", err)
	}
	_ = compacting

	replayed, err := NewWithMaxEvents(path, 8) // reads [checkpoint][tail] — the real assertion
	if err != nil {
		t.Fatalf("NewWithMaxEvents (replaying load): %v", err)
	}
	if replayed.Recurrence("k").SilencedUntil.IsZero() {
		t.Fatal("precondition: the silence should still stand after the replay")
	}
	if _, _, err := replayed.Resolve("fp1", now.Add(2*time.Hour)); err != nil {
		t.Fatalf("Resolve (after compaction): %v", err)
	}
	if got := replayed.Recurrence("k").SilencedUntil; !got.IsZero() {
		t.Errorf("SilencedUntil = %v, want zero — the checkpoint dropped the trigger's fingerprint", got)
	}
}
