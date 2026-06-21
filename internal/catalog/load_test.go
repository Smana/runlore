package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func writeEntry(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "helmrelease-upgrade-failure.md", `---
type: Playbook
title: HelmRelease upgrade failure
description: Diagnose a Helm release stuck after an upgrade.
tags: [flux, helmrelease, upgrade]
---
# Symptom
Ready=False after a chart bump.
`)
	writeEntry(t, dir, "index.md", "---\ntype: Index\n---\n# ignored\n") // reserved, skipped
	writeEntry(t, dir, "notes.txt", "not markdown")                      // skipped

	entries, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry (index.md + .txt skipped), got %d", len(entries))
	}
	e := entries[0]
	if e.Type != "Playbook" || e.Title != "HelmRelease upgrade failure" || len(e.Tags) != 3 {
		t.Fatalf("frontmatter not parsed: %+v", e)
	}
	if !contains(e.Body, "Ready=False") {
		t.Fatalf("body not captured: %q", e.Body)
	}
}

func TestLoadSkipsMalformedEntry(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "good.md", "---\ntype: Playbook\ntitle: Good\ndescription: fine\n---\nbody\n")
	// Unquoted colon in a value → invalid YAML frontmatter (the real bug we hit).
	writeEntry(t, dir, "bad.md", "---\ntype: Playbook\ntitle: Bad\ndescription: a: b broken\n---\nbody\n")

	entries, skipped, err := Load(dir)
	if err != nil {
		t.Fatalf("Load should not fail fatally on a malformed entry: %v", err)
	}
	if len(entries) != 1 || entries[0].Title != "Good" {
		t.Fatalf("the good entry must still load; got %+v", entries)
	}
	if len(skipped) != 1 || !contains(skipped[0], "bad.md") {
		t.Fatalf("the malformed entry must be reported as skipped; got %v", skipped)
	}
}

func TestLoadSkipsHidden(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "real.md", "---\ntype: Playbook\ntitle: Real\n---\nbody\n")
	// Simulate a ConfigMap mount: a hidden ..data-style dir shadowing the entry,
	// plus a hidden dotfile. Neither should be indexed (else entries double-count).
	hidden := filepath.Join(dir, "..2026_06_20_data")
	if err := os.MkdirAll(hidden, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hidden, "real.md"), []byte("---\ntitle: Shadow\n---\nx"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeEntry(t, dir, ".hidden.md", "---\ntitle: Hidden\n---\nx")

	entries, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 || entries[0].Title != "Real" {
		t.Fatalf("want exactly 1 entry 'Real', got %d: %+v", len(entries), entries)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
