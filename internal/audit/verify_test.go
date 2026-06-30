package audit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeChain writes a fresh, intact 3-record chain to path and returns it closed.
func writeChain(t *testing.T, path string) {
	t.Helper()
	l, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, op := range []string{"suspend", "resume", "reconcile"} {
		if err := l.Log(Record{Actor: "auto", Op: op, Target: "Kustomization/apps/web", Decision: DecisionExecuted}); err != nil {
			t.Fatalf("log: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

func TestOpenVerifiedIntact(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	writeChain(t, path)

	l, err := OpenVerified(path)
	if err != nil {
		t.Fatalf("OpenVerified on an intact chain must succeed, got: %v", err)
	}
	// The returned Logger must keep appending cleanly (chain seeded from disk).
	if err := l.Log(Record{Actor: "auto", Op: "suspend", Target: "Kustomization/apps/api", Decision: DecisionSkipped, Reason: "paused"}); err != nil {
		t.Fatalf("append after OpenVerified: %v", err)
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if err := Verify(strings.NewReader(string(b))); err != nil {
		t.Fatalf("chain must stay verifiable after OpenVerified append: %v", err)
	}
}

func TestOpenVerifiedAbsentFile(t *testing.T) {
	// An absent file is an empty (valid) chain — OpenVerified must create + succeed.
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	l, err := OpenVerified(path)
	if err != nil {
		t.Fatalf("OpenVerified on an absent file must succeed (empty chain is valid), got: %v", err)
	}
	_ = l.Close()
}

func TestOpenVerifiedEmptyFile(t *testing.T) {
	// An existing but empty file is a valid (zero-record) chain.
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	l, err := OpenVerified(path)
	if err != nil {
		t.Fatalf("OpenVerified on an empty file must succeed, got: %v", err)
	}
	_ = l.Close()
}

func TestOpenVerifiedTampered(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	writeChain(t, path)

	// Tamper with the middle record's target without recomputing the hash.
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 records, got %d", len(lines))
	}
	lines[1] = strings.Replace(lines[1], "apps", "flux-system", 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenVerified(path); err == nil {
		t.Fatal("OpenVerified must return an error on a tampered chain")
	}
}

func TestOpenVerifiedMidChainDeletion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	writeChain(t, path)

	// Drop the middle record: the surviving record 2's prev_hash no longer matches
	// record 1's hash (insertion/mid-chain deletion is caught; tail-truncation is not).
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 records, got %d", len(lines))
	}
	kept := []string{lines[0], lines[2]}
	if err := os.WriteFile(path, []byte(strings.Join(kept, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := OpenVerified(path); err == nil {
		t.Fatal("OpenVerified must return an error on a mid-chain deletion")
	}
}
