// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/kbvalidate"
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

func TestKBImportEndToEnd(t *testing.T) {
	kb := t.TempDir()
	var buf bytes.Buffer
	if err := runKBImport([]string{"testdata/kbimport", "--into", kb}, &buf); err != nil {
		t.Fatal(err)
	}

	// The bare runbook became a Playbook; the postmortem an Incident.
	playbook, err := os.ReadFile(filepath.Join(kb, "playbooks", "redis-failover.md"))
	if err != nil {
		t.Fatalf("playbook not written: %v", err)
	}
	for _, want := range []string{"type: Playbook", "title: Redis failover", "- redisdown", "## Steps"} {
		if !strings.Contains(string(playbook), want) {
			t.Errorf("playbook missing %q:\n%s", want, playbook)
		}
	}
	incident, err := os.ReadFile(filepath.Join(kb, "incidents", "payments-api-outage.md"))
	if err != nil {
		t.Fatalf("incident not written: %v", err)
	}
	for _, want := range []string{"type: Incident", "resource: payments/api", "2024-03-14", "- payments", "## Cause"} {
		if !strings.Contains(string(incident), want) {
			t.Errorf("incident missing %q:\n%s", want, incident)
		}
	}

	// The near-duplicate title was skipped, the README ignored.
	if !strings.Contains(buf.String(), "duplicate of") && !strings.Contains(buf.String(), "collides") {
		t.Fatalf("legacy redis copy must be skipped as a duplicate:\n%s", buf.String())
	}

	// Round-trip guarantee: everything written loads back and passes the gate.
	entries, skipped, err := catalog.Load(kb)
	if err != nil || len(skipped) > 0 {
		t.Fatalf("written entries must parse: err=%v skipped=%v", err, skipped)
	}
	if n := kbvalidate.WarnInvalid(entries, func(p string, errs []kbvalidate.Issue) {
		t.Errorf("invalid written entry %s: %v", p, errs)
	}); n != 0 {
		t.Fatalf("%d written entries fail the merge gate", n)
	}

	// Idempotence: a second run imports nothing.
	var buf2 bytes.Buffer
	if err := runKBImport([]string{"testdata/kbimport", "--into", kb}, &buf2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf2.String(), "imported 0") {
		t.Fatalf("re-run must be a no-op:\n%s", buf2.String())
	}
}
