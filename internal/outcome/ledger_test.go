package outcome

import (
	"path/filepath"
	"testing"
	"time"
)

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
	if ep.Kind != "recall" || ep.Entry != "harbor.md" || ep.Duration != 90*time.Second {
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
