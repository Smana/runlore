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

	entries, err := Load(dir)
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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
