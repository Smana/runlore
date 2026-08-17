// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
)

const (
	releaseConfigPath   = "../../release-please-config.json"
	releaseManifestPath = "../../.release-please-manifest.json"
	contributingPath    = "../../CONTRIBUTING.md"
)

// TestBreakingChangesDoNotSilentlyCutAOneDotZero pins the release configuration
// against the version it is actually operating on.
//
// release-please's default versioning strategy bumps a package whose major version is
// 0 straight to 1.0.0 on the FIRST breaking change, unless bump-minor-pre-major is
// set. This repo has never marked a breaking change — no `!` in any subject, no
// `BREAKING CHANGE:` footer anywhere in 1000+ commits — so that path has never been
// exercised, and nothing lints commit subjects or PR titles either. The day someone
// correctly marks a config migration as breaking, the release automation would
// silently cut a 1.0.0 that nobody decided to make, with the stability promise that
// carries.
//
// The guard is scoped to the pre-1.0 window it protects: once the project genuinely
// releases 1.x the setting stops mattering and this test stops asserting it, rather
// than becoming a stale rule someone has to delete.
func TestBreakingChangesDoNotSilentlyCutAOneDotZero(t *testing.T) {
	if major := releasedMajor(t); major > 0 {
		t.Skipf("released version is %d.x — bump-minor-pre-major no longer changes any bump", major)
	}
	pkg := releasePackageConfig(t)
	bump, ok := pkg["bump-minor-pre-major"].(bool)
	if !ok || !bump {
		t.Errorf("release-please-config.json must set \"bump-minor-pre-major\": true while %s is pre-1.0.\n"+
			"Without it, the first commit marked breaking (a `!` subject or a BREAKING CHANGE: footer) "+
			"bumps 0.x straight to 1.0.0 — so marking a migration honestly and cutting a 1.0 release "+
			"become the same action, and the only pressure that resolves it is to under-mark the migration.",
			releaseManifestPath)
	}
}

// TestChangelogSectionsCoverTheTypesWeWrite pins that the two commit types this repo
// actually ships behaviour under both reach CHANGELOG.md. A type missing from
// changelog-sections is not an error at release time — release-please just drops it —
// so an operator reading the changelog would silently not see that class of change.
func TestChangelogSectionsCoverTheTypesWeWrite(t *testing.T) {
	pkg := releasePackageConfig(t)
	sections, ok := pkg["changelog-sections"].([]any)
	if !ok {
		t.Fatalf("release-please-config.json has no changelog-sections array")
	}
	visible := map[string]bool{}
	for _, s := range sections {
		m, ok := s.(map[string]any)
		if !ok {
			continue
		}
		typ, _ := m["type"].(string)
		hidden, _ := m["hidden"].(bool)
		visible[typ] = !hidden
	}
	for _, typ := range []string{"feat", "fix"} {
		if !visible[typ] {
			t.Errorf("changelog-sections must render %q — a behaviour change landed under a hidden "+
				"type never reaches an upgrading operator", typ)
		}
	}
}

// TestContributingExplainsHowToMarkABreakingChange pins the one piece of guidance the
// release pipeline cannot enforce for itself.
//
// Nothing in CI lints a commit subject or a PR title, PRs are squash-merged (so the PR
// TITLE becomes the commit release-please parses, not the branch commits), and the
// only mechanism that carries prose — not just a subject line — into CHANGELOG.md is a
// BREAKING CHANGE: footer. All three facts have to be written down together or the
// migration note for a behaviour change lands nowhere an operator reads.
func TestContributingExplainsHowToMarkABreakingChange(t *testing.T) {
	doc := flattenProse(readDoc(t, contributingPath))
	for _, want := range []string{
		"BREAKING CHANGE:",
		"the PR **title** is what release-please parses",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("CONTRIBUTING.md must explain how to mark a breaking change so the migration note "+
				"reaches CHANGELOG.md; missing %q", want)
		}
	}
}

// releasePackageConfig returns the "." package block of release-please-config.json.
func releasePackageConfig(t *testing.T) map[string]any {
	t.Helper()
	var cfg struct {
		Packages map[string]map[string]any `json:"packages"`
	}
	raw, err := os.ReadFile(releaseConfigPath)
	if err != nil {
		t.Fatalf("read %s: %v", releaseConfigPath, err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("parse %s: %v", releaseConfigPath, err)
	}
	pkg, ok := cfg.Packages["."]
	if !ok {
		t.Fatalf("%s has no \".\" package", releaseConfigPath)
	}
	return pkg
}

// releasedMajor returns the major version release-please last released, read from the
// manifest it maintains — the same file the tooling reads, not a number restated here.
func releasedMajor(t *testing.T) int {
	t.Helper()
	raw, err := os.ReadFile(releaseManifestPath)
	if err != nil {
		t.Fatalf("read %s: %v", releaseManifestPath, err)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("parse %s: %v", releaseManifestPath, err)
	}
	v, ok := m["."]
	if !ok {
		t.Fatalf("%s has no \".\" entry", releaseManifestPath)
	}
	major, err := strconv.Atoi(v[:strings.Index(v+".", ".")])
	if err != nil {
		t.Fatalf("unparseable version %q in %s: %v", v, releaseManifestPath, err)
	}
	return major
}
