// SPDX-License-Identifier: Apache-2.0

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
	writeEntry(t, dir, "README.md", "# repo docs, no frontmatter\n")     // reserved, skipped
	writeEntry(t, dir, "notes.txt", "not markdown")                      // skipped

	entries, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry (index.md + README.md + .txt skipped), got %d", len(entries))
	}
	e := entries[0]
	if e.Type != "Playbook" || e.Title != "HelmRelease upgrade failure" || len(e.Tags) != 3 {
		t.Fatalf("frontmatter not parsed: %+v", e)
	}
	if !contains(e.Body, "Ready=False") {
		t.Fatalf("body not captured: %q", e.Body)
	}
}

// TestLoadIgnoresSymlinkedEntries: a catalog is SYNCED FROM A GIT REPO — the shipped
// default even points at a third-party public commons repo — and a git tree can carry
// a mode-120000 entry, which clones into a real symlink. WalkDir does not follow
// symlinks, but a symlink to a FILE arrives with IsDir()==false, so it used to sail
// through the name check into os.ReadFile, which does follow it. A relative target
// escapes the checkout (go-billy rewrites only ABSOLUTE targets into the clone root),
// so one merged PR adding what reads as an ordinary new .md in a GitHub diff could
// pull the ServiceAccount token or /proc/self/environ — the model API key, the forge
// token — into Entry.Body, into the search corpus, and back out through kb_get.
//
// Guarded by file TYPE, not by resolving the target: a target check would still admit
// a link to another file inside the catalog while inviting a TOCTOU between the check
// and the read, and it would not cover a fifo or device node, which make os.ReadFile
// block or allocate without end. Nothing that is not a regular file is ever an entry.
func TestLoadIgnoresSymlinkedEntries(t *testing.T) {
	outside := t.TempDir()
	secret := filepath.Join(outside, "token")
	if err := os.WriteFile(secret, []byte("---\ntype: Playbook\ntitle: Stolen\ndescription: d\n---\nSUPER-SECRET-TOKEN\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	writeEntry(t, dir, "real-entry.md", "---\ntype: Playbook\ntitle: Real\ndescription: d\n---\nbody\n")
	// Both spellings a hostile repo could use: an absolute target, and the relative
	// one go-billy leaves untouched.
	if err := os.Symlink(secret, filepath.Join(dir, "exfil-abs.md")); err != nil {
		t.Skipf("symlinks unavailable on this platform: %v", err)
	}
	rel, err := filepath.Rel(dir, secret)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, filepath.Join(dir, "exfil-rel.md")); err != nil {
		t.Fatal(err)
	}

	entries, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want only the regular file indexed, got %d: %+v", len(entries), entries)
	}
	if contains(entries[0].Body, "SUPER-SECRET-TOKEN") {
		t.Fatalf("a symlink pulled a file from outside the catalog into the corpus: %+v", entries[0])
	}
}

// TestLoadSkipsRepoRootDocs: a catalog served from a Git repository carries the
// conventional repo documents, and none of them are knowledge. This is a real
// regression, not a hypothetical: the public commons repo has a CONTRIBUTING.md,
// and the loader used to index it as an entry — polluting kb_search, and tripping
// the validator because it carries no OKF frontmatter.
//
// Uppercase deliberately: these files are conventionally uppercase, so a
// case-sensitive check would skip none of them.
func TestLoadSkipsRepoRootDocs(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "real-entry.md", "---\ntype: Playbook\ntitle: Real\ndescription: d\n---\nbody\n")
	for _, name := range []string{
		"CONTRIBUTING.md", "CODE_OF_CONDUCT.md", "SECURITY.md",
		"CHANGELOG.md", "LICENSE.md", "GOVERNANCE.md", "MAINTAINERS.md", "SUPPORT.md",
	} {
		writeEntry(t, dir, name, "# repo doc, no OKF frontmatter\n")
	}

	entries, skipped, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(skipped) != 0 {
		t.Errorf("repo docs must be skipped silently, not reported as unparseable: %v", skipped)
	}
	if len(entries) != 1 || entries[0].Title != "Real" {
		got := make([]string, len(entries))
		for i, e := range entries {
			got[i] = e.Path
		}
		t.Fatalf("want exactly 1 entry (the real one), got %d: %v", len(entries), got)
	}
}

// TestLoadParsesTimestampAndFingerprint: curated entries carry a timestamp
// (OKF-recommended) and a deterministic dedup fingerprint in frontmatter — both
// written by the forge serializer and consumed back here (recency-aware ranking,
// exact-identity catalog dedup).
func TestLoadParsesTimestampAndFingerprint(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "curated.md", `---
type: Incident
title: Harbor down
description: valkey down
resource: tooling/harbor-core
timestamp: "2026-06-20T00:00:00Z"
fingerprint: deadbeefcafebabe
---
body
`)
	entries, _, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("want 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Timestamp != "2026-06-20T00:00:00Z" {
		t.Fatalf("Timestamp = %q, want 2026-06-20T00:00:00Z", e.Timestamp)
	}
	if e.Fingerprint != "deadbeefcafebabe" {
		t.Fatalf("Fingerprint = %q, want deadbeefcafebabe", e.Fingerprint)
	}

	// Seed entries write the timestamp unquoted (a YAML !!timestamp scalar); it
	// must still land in the string field rather than failing the parse.
	writeEntry(t, dir, "seed.md", `---
type: Playbook
title: Seed
description: seed entry
timestamp: 2026-06-20T00:00:00Z
---
body
`)
	entries, skipped, err := Load(dir)
	if err != nil || len(skipped) != 0 {
		t.Fatalf("Load with unquoted timestamp: err=%v skipped=%v", err, skipped)
	}
	for _, e := range entries {
		if e.Title == "Seed" && e.Timestamp != "2026-06-20T00:00:00Z" {
			t.Fatalf("unquoted Timestamp = %q, want 2026-06-20T00:00:00Z", e.Timestamp)
		}
	}
}

// TestLoadParsesStatusAndLastValidated: the lifecycle fields (status, last_validated)
// are parsed into the Entry and an unknown frontmatter key (okf_version) never errors —
// yaml.Unmarshal without KnownFields ignores it, pinned here so it stays true.
func TestLoadParsesStatusAndLastValidated(t *testing.T) {
	dir := t.TempDir()
	entry := `---
type: Incident
title: retired one
status: retired
last_validated: 2026-01-10
okf_version: "0.1"
---
Body.
`
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, skipped, err := Load(dir)
	if err != nil || len(skipped) != 0 || len(entries) != 1 {
		t.Fatalf("entries=%d skipped=%v err=%v", len(entries), skipped, err)
	}
	e := entries[0]
	if e.Status != "retired" || e.LastValidated != "2026-01-10" {
		t.Errorf("Status=%q LastValidated=%q, want retired / 2026-01-10", e.Status, e.LastValidated)
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

// TestParseEntryDate pins the one date grammar shared by kbvalidate and recall:
// RFC3339 and bare date parse; empty and garbage report ok=false (distinct from a
// zero-but-valid time).
func TestParseEntryDate(t *testing.T) {
	tests := []struct {
		in   string
		want bool
	}{
		{"2026-01-10T00:00:00Z", true},
		{"2026-01-10", true},
		{"", false},
		{"not-a-date", false},
	}
	for _, tc := range tests {
		if _, ok := ParseEntryDate(tc.in); ok != tc.want {
			t.Errorf("ParseEntryDate(%q) ok=%v, want %v", tc.in, ok, tc.want)
		}
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

func TestSplitFrontmatterExported(t *testing.T) {
	fm, body := SplitFrontmatter([]byte("---\ntitle: t\n---\nbody\n"))
	if string(fm) != "title: t" || string(body) != "body\n" {
		t.Fatalf("got fm=%q body=%q", fm, body)
	}
	fm, body = SplitFrontmatter([]byte("no frontmatter"))
	if fm != nil || string(body) != "no frontmatter" {
		t.Fatalf("frontmatterless input must pass through, got fm=%q body=%q", fm, body)
	}
}
