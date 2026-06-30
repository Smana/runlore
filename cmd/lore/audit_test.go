package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/audit"
)

// writeAuditChain writes a fresh, intact audit chain of n records and returns its path.
func writeAuditChain(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "audit.jsonl")
	l, err := audit.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	for _, op := range []string{"suspend", "resume", "reconcile"} {
		if err := l.Log(audit.Record{Actor: "auto", Op: op, Target: "Kustomization/apps/web", Decision: audit.DecisionExecuted}); err != nil {
			t.Fatalf("log: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	return path
}

func TestAuditVerifyIntactOK(t *testing.T) {
	path := writeAuditChain(t, t.TempDir())
	var buf bytes.Buffer
	if err := auditVerify(&buf, path); err != nil {
		t.Fatalf("intact chain must verify, got: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "OK") {
		t.Fatalf("expected an OK line, got: %q", out)
	}
	if !strings.Contains(out, "3") {
		t.Fatalf("expected the record count (3) in output, got: %q", out)
	}
}

func TestAuditVerifyTamperedFails(t *testing.T) {
	path := writeAuditChain(t, t.TempDir())
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	lines[1] = strings.Replace(lines[1], "apps", "flux-system", 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := auditVerify(&buf, path); err == nil {
		t.Fatalf("tampered chain must fail, output: %q", buf.String())
	}
}

func TestAuditVerifyEmptyFileOK(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := auditVerify(&buf, path); err != nil {
		t.Fatalf("empty chain must verify (0 records), got: %v", err)
	}
	if !strings.Contains(buf.String(), "0") {
		t.Fatalf("expected 0-record OK line, got: %q", buf.String())
	}
}

func TestAuditVerifyMissingPathErrors(t *testing.T) {
	var buf bytes.Buffer
	if err := auditVerify(&buf, ""); err == nil {
		t.Fatal("an empty --path must error")
	}
}
