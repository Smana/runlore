// SPDX-License-Identifier: Apache-2.0

package outcome

import (
	"path/filepath"
	"testing"
	"time"
)

func TestConfirmFoldsIntoAggregateAndTriggerIndex(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := l.Confirm("kb/worker-oom.md", "trig-1", "fp-abc", at); err != nil {
		t.Fatal(err)
	}
	if err := l.Confirm("kb/worker-oom.md", "trig-1", "fp-abc", at.Add(time.Minute)); err != nil {
		t.Fatal(err)
	}
	counts, err := l.OpenCounts()
	if err != nil {
		t.Fatal(err)
	}
	if got := counts["kb/worker-oom.md"].Confirms; got != 2 {
		t.Fatalf("Aggregate.Confirms = %d, want 2", got)
	}
}

func TestConfirmSurvivesReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := l.Confirm("kb/e.md", "trig-1", "fp", time.Now()); err != nil {
		t.Fatal(err)
	}
	// A fresh ledger over the same file replays the confirm line.
	l2, err := New(path)
	if err != nil {
		t.Fatal(err)
	}
	counts, _ := l2.OpenCounts()
	if got := counts["kb/e.md"].Confirms; got != 1 {
		t.Fatalf("replayed Confirms = %d, want 1", got)
	}
}

func TestConfirmSurvivesCompactionCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "ledger.jsonl")
	l, err := NewWithMaxEvents(path, 5) // tiny cap forces compaction on reload
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err := l.Confirm("kb/e.md", "trig-1", "fp", at); err != nil {
		t.Fatal(err)
	}
	// Push the confirm past the compaction horizon with filler opens.
	for i := 0; i < 10; i++ {
		if err := l.Open(Event{Fingerprint: DeriveFingerprint(GitOpsFingerprintPrefix, string(rune('a'+i))), Kind: "fresh", At: at.Add(time.Duration(i) * time.Second)}); err != nil {
			t.Fatal(err)
		}
	}
	// Reload triggers compaction (file > maxEvents): the confirm is folded into the
	// checkpoint. Its contribution must survive into a THIRD load that only ever
	// sees the checkpoint.
	l2, err := NewWithMaxEvents(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	counts, _ := l2.OpenCounts()
	if got := counts["kb/e.md"].Confirms; got != 1 {
		t.Fatalf("post-compaction Confirms = %d, want 1 (lost by the checkpoint)", got)
	}
	l3, err := NewWithMaxEvents(path, 5)
	if err != nil {
		t.Fatal(err)
	}
	counts3, _ := l3.OpenCounts()
	if got := counts3["kb/e.md"].Confirms; got != 1 {
		t.Fatalf("checkpoint-only Confirms = %d, want 1", got)
	}
}

func TestContestedTriggersCarryConfirmCount(t *testing.T) {
	l, err := New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	// An open with a KB link makes the trigger contest-eligible…
	if err := l.Open(Event{Fingerprint: "fp1", Kind: "fresh", TriggerKey: "trig-1", CuratedURL: "https://github.com/o/r/pull/7", At: at}); err != nil {
		t.Fatal(err)
	}
	// …a standing 👎 contests it, and a confirmation is recorded against it.
	if err := l.Feedback("trig-1", "down", "U1", at.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := l.Confirm("kb/e.md", "trig-1", "fp-dup", at.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	cts := l.ContestedTriggers()
	if len(cts) != 1 {
		t.Fatalf("ContestedTriggers = %v, want exactly one", cts)
	}
	if cts[0].Confirms != 1 {
		t.Fatalf("ContestedTrigger.Confirms = %d, want 1", cts[0].Confirms)
	}
}

func TestConfirmDisabledLedgerAndEmptyEntry(t *testing.T) {
	disabled, err := New("") // path "" ⇒ disabled ledger, mirroring Feedback's no-op contract
	if err != nil {
		t.Fatal(err)
	}
	if err := disabled.Confirm("kb/e.md", "t", "fp", time.Now()); err != nil {
		t.Fatalf("disabled ledger must no-op, got %v", err)
	}
	if err := disabled.Confirm("", "t", "fp", time.Now()); err == nil {
		t.Fatal("empty entry path must be an error (an unattributable confirm is a bug)")
	}
}
