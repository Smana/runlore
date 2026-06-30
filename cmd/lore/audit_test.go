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

// writeConfig writes a minimal RunLore config that sets actions.audit_log_path to
// logPath (mode is left unset, so config validation passes) and returns its path.
func writeConfig(t *testing.T, dir, logPath string) string {
	t.Helper()
	cfgPath := filepath.Join(dir, "runlore.yaml")
	body := "actions:\n  audit_log_path: " + logPath + "\n"
	if err := os.WriteFile(cfgPath, []byte(body), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	return cfgPath
}

// TestRunAuditVerifyConfigIntactOK exercises the --config code path: the audit-log
// path is resolved from actions.audit_log_path, and an intact chain verifies.
func TestRunAuditVerifyConfigIntactOK(t *testing.T) {
	dir := t.TempDir()
	logPath := writeAuditChain(t, dir)
	cfgPath := writeConfig(t, dir, logPath)

	if err := runAuditVerify([]string{"--config", cfgPath}); err != nil {
		t.Fatalf("intact chain via --config must verify, got: %v", err)
	}
}

// TestRunAuditVerifyConfigTamperedFails exercises --config against a broken chain:
// the resolved path's chain is tampered, so verification (and the command) fails.
func TestRunAuditVerifyConfigTamperedFails(t *testing.T) {
	dir := t.TempDir()
	logPath := writeAuditChain(t, dir)
	b, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	lines[1] = strings.Replace(lines[1], "apps", "flux-system", 1)
	if err := os.WriteFile(logPath, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfgPath := writeConfig(t, dir, logPath)

	if err := runAuditVerify([]string{"--config", cfgPath}); err == nil {
		t.Fatal("tampered chain via --config must fail")
	}
}

// TestRunAuditVerifyConfigNoAuditPathErrors checks that a --config without
// actions.audit_log_path is a usage error (nothing to verify).
func TestRunAuditVerifyConfigNoAuditPathErrors(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "runlore.yaml")
	if err := os.WriteFile(cfgPath, []byte("actions:\n  mode: off\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := runAuditVerify([]string{"--config", cfgPath}); err == nil {
		t.Fatal("a --config with no actions.audit_log_path must error")
	}
}
