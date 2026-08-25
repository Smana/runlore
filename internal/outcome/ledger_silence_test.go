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
