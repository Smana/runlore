// SPDX-License-Identifier: Apache-2.0

package audit

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChainVerifies(t *testing.T) {
	var buf bytes.Buffer
	l := NewWriter(&buf)
	for _, op := range []string{"suspend", "resume", "reconcile"} {
		if err := l.Log(Record{Actor: "auto", Op: op, Target: "Kustomization/apps/web", Decision: DecisionExecuted}); err != nil {
			t.Fatalf("log: %v", err)
		}
	}
	if err := Verify(bytes.NewReader(buf.Bytes())); err != nil {
		t.Fatalf("verify clean chain: %v", err)
	}
}

func TestChainDetectsTampering(t *testing.T) {
	var buf bytes.Buffer
	l := NewWriter(&buf)
	for _, op := range []string{"suspend", "resume", "reconcile"} {
		_ = l.Log(Record{Actor: "auto", Op: op, Target: "Kustomization/apps/web", Decision: DecisionExecuted})
	}
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 records, got %d", len(lines))
	}
	// Tamper with the target of the middle record without recomputing the hash.
	lines[1] = strings.Replace(lines[1], "apps", "flux-system", 1)
	if err := Verify(strings.NewReader(strings.Join(lines, "\n"))); err == nil {
		t.Fatal("verify should detect a tampered record")
	}
}

// TestOpenFileAppendsAndSyncs checks that a Logger opened on a real file appends
// a verifiable chain (the fsync-on-write path must not error and must persist
// every record to disk).
func TestOpenFileAppendsAndSyncs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
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

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if got := strings.Count(strings.TrimSpace(string(b)), "\n") + 1; got != 3 {
		t.Fatalf("want 3 records on disk, got %d", got)
	}
	if err := Verify(bytes.NewReader(b)); err != nil {
		t.Fatalf("verify file-backed chain: %v", err)
	}

	// Reopening seeds the chain from disk: a further append must chain cleanly.
	l2, err := Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if err := l2.Log(Record{Actor: "auto", Op: "suspend", Target: "Kustomization/apps/api", Decision: DecisionSkipped, Reason: "paused"}); err != nil {
		t.Fatalf("log after reopen: %v", err)
	}
	if err := l2.Close(); err != nil {
		t.Fatalf("close reopen: %v", err)
	}
	b, err = os.ReadFile(path)
	if err != nil {
		t.Fatalf("read back after reopen: %v", err)
	}
	if err := Verify(bytes.NewReader(b)); err != nil {
		t.Fatalf("verify chain across reopen: %v", err)
	}
}
