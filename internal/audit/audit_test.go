package audit

import (
	"bytes"
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
