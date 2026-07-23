// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKBImportRequiresSrcAndDest(t *testing.T) {
	var buf bytes.Buffer
	if err := runKBImport([]string{}, &buf); err == nil {
		t.Fatal("missing source dir must error")
	}
	src := t.TempDir()
	writeFile(t, src, "a.md", "# A\n\nbody\n")
	if err := runKBImport([]string{src, "--config", filepath.Join(t.TempDir(), "nope.yaml")}, &buf); err == nil {
		t.Fatal("no --into and no config catalog.dir must error")
	}
}

func TestKBImportDryRunWritesNothing(t *testing.T) {
	src, kb := t.TempDir(), t.TempDir()
	writeFile(t, src, "redis-failover.md", "# Redis failover\n\nHow to fail over redis.\n")
	var buf bytes.Buffer
	if err := runKBImport([]string{src, "--into", kb, "--dry-run"}, &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(kb, "playbooks", "redis-failover.md")); !os.IsNotExist(err) {
		t.Fatal("--dry-run must not write files")
	}
	out := buf.String()
	for _, want := range []string{"import", "playbooks/redis-failover.md", "Redis failover", "dry-run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, out)
		}
	}
}

func TestKBImportSkipsReservedAndInvalid(t *testing.T) {
	src, kb := t.TempDir(), t.TempDir()
	writeFile(t, src, "README.md", "# not knowledge\n")
	writeFile(t, src, "notes.txt", "not markdown")
	// Declares Incident but has no resource/sections → fails the merge gate → skipped.
	writeFile(t, src, "broken.md", "---\ntype: Incident\ntitle: broken\n---\nno sections\n")
	writeFile(t, src, "ok.md", "# OK runbook\n\nA fine playbook.\n")
	var buf bytes.Buffer
	if err := runKBImport([]string{src, "--into", kb}, &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(kb, "playbooks", "ok-runbook.md")); err != nil {
		t.Fatalf("valid entry must be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kb, "incidents", "broken.md")); !os.IsNotExist(err) {
		t.Fatal("gate-failing entry must not be written")
	}
	out := buf.String()
	if !strings.Contains(out, "fails validation") {
		t.Fatalf("skip reason must be reported:\n%s", out)
	}
	if strings.Contains(out, "README.md") {
		t.Fatalf("reserved files must be silently ignored:\n%s", out)
	}
}
