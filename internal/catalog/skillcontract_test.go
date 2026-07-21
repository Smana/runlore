// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The kb-steward Claude Code plugin is served from this repo (see
// docs/kb-steward.md). These tests keep the skill's documented OKF contract
// and the plugin manifests from drifting as the loader evolves.

const repoRoot = "../.."
const pluginRoot = repoRoot + "/plugins/kb-steward"

var parsedFieldsRE = regexp.MustCompile("`([a-z_]+)`")

// TestOKFFormatDocMatchesLoader pins the skill's documented frontmatter field
// list to what the loader actually parses. It reflects over entryMeta's yaml
// tags (load.go) — the struct parseEntry unmarshals into — rather than
// Entry's Go field names, because Entry is a derived in-memory shape: renaming
// a yaml tag on entryMeta without touching Entry would leave a name-based
// check on Entry green while the loader's real contract drifted from the doc.
func TestOKFFormatDocMatchesLoader(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pluginRoot, "skills/kb-steward/references/okf-format.md"))
	if err != nil {
		t.Fatalf("read okf-format.md: %v", err)
	}
	doc := string(raw)
	start := strings.Index(doc, "<!-- parsed-fields:start -->")
	end := strings.Index(doc, "<!-- parsed-fields:end -->")
	if start < 0 || end < 0 || end < start {
		t.Fatal("okf-format.md must contain a <!-- parsed-fields:start -->…<!-- parsed-fields:end --> block")
	}
	documented := map[string]bool{}
	for _, m := range parsedFieldsRE.FindAllStringSubmatch(doc[start:end], -1) {
		documented[m[1]] = true
	}

	parsed := map[string]bool{}
	mt := reflect.TypeOf(entryMeta{})
	for i := 0; i < mt.NumField(); i++ {
		tag := mt.Field(i).Tag.Get("yaml")
		if tag == "" || tag == "-" {
			continue
		}
		parsed[tag] = true
	}

	for f := range parsed {
		if !documented[f] {
			t.Errorf("loader parses frontmatter field %q but okf-format.md does not document it", f)
		}
	}
	for f := range documented {
		if !parsed[f] {
			t.Errorf("okf-format.md documents field %q but the loader does not parse it", f)
		}
	}
}

// TestPluginManifestsValid keeps the marketplace/plugin manifests installable:
// docs tell users to run `/plugin install kb-steward@runlore`.
func TestPluginManifestsValid(t *testing.T) {
	var marketplace struct {
		Name  string `json:"name"`
		Owner struct {
			Name string `json:"name"`
		} `json:"owner"`
		Plugins []struct {
			Name        string `json:"name"`
			Source      string `json:"source"`
			Description string `json:"description"`
		} `json:"plugins"`
	}
	raw, err := os.ReadFile(filepath.Join(repoRoot, ".claude-plugin/marketplace.json"))
	if err != nil {
		t.Fatalf("read marketplace.json: %v", err)
	}
	if err := json.Unmarshal(raw, &marketplace); err != nil {
		t.Fatalf("marketplace.json is not valid JSON: %v", err)
	}
	if marketplace.Name != "runlore" {
		t.Errorf("marketplace name = %q, want %q", marketplace.Name, "runlore")
	}
	if marketplace.Owner.Name == "" {
		t.Error("marketplace owner.name must be set")
	}
	if len(marketplace.Plugins) != 1 || marketplace.Plugins[0].Name != "kb-steward" {
		t.Fatalf("plugins = %+v, want exactly one entry named kb-steward", marketplace.Plugins)
	}
	if got, want := marketplace.Plugins[0].Source, "./plugins/kb-steward"; got != want {
		t.Errorf("plugin source = %q, want %q", got, want)
	}
	if marketplace.Plugins[0].Description == "" {
		t.Error("plugin description must be set (shown in /plugin listings)")
	}

	var plugin struct {
		Name string `json:"name"`
	}
	raw, err = os.ReadFile(filepath.Join(pluginRoot, ".claude-plugin/plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	if err := json.Unmarshal(raw, &plugin); err != nil {
		t.Fatalf("plugin.json is not valid JSON: %v", err)
	}
	if plugin.Name != "kb-steward" {
		t.Errorf("plugin name = %q, want kb-steward", plugin.Name)
	}

	for _, p := range []string{
		"skills/kb-steward/SKILL.md",
		"skills/kb-steward/references/okf-format.md",
		"skills/kb-steward/references/entry-quality-checklist.md",
		"skills/kb-steward/references/interview-guides.md",
	} {
		if _, err := os.Stat(filepath.Join(pluginRoot, p)); err != nil {
			t.Errorf("plugin file missing: %s: %v", p, err)
		}
	}
}

// TestPluginVersionTracksRelease pins the plugin manifest's version to the
// repo's released version. The two are kept in sync by release-please, which
// rewrites plugin.json through the extra-files entry asserted below; without
// that wiring the manifest silently freezes at its initial version while the
// project releases on, which is exactly how it read before this test existed.
func TestPluginVersionTracksRelease(t *testing.T) {
	var manifest map[string]string
	raw, err := os.ReadFile(filepath.Join(repoRoot, ".release-please-manifest.json"))
	if err != nil {
		t.Fatalf("read .release-please-manifest.json: %v", err)
	}
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatalf("release-please manifest is not valid JSON: %v", err)
	}
	want := manifest["."]
	if want == "" {
		t.Fatal(`release-please manifest has no version for the "." package`)
	}

	var plugin struct {
		Version string `json:"version"`
	}
	raw, err = os.ReadFile(filepath.Join(pluginRoot, ".claude-plugin/plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	if err := json.Unmarshal(raw, &plugin); err != nil {
		t.Fatalf("plugin.json is not valid JSON: %v", err)
	}
	if plugin.Version != want {
		t.Errorf("plugin.json version = %q, want %q (the released version); "+
			"release-please should be bumping it via extra-files", plugin.Version, want)
	}

	// The equality above only keeps holding while release-please is told to
	// rewrite the file, so pin that wiring too.
	var cfg struct {
		Packages map[string]struct {
			ExtraFiles []struct {
				Type     string `json:"type"`
				Path     string `json:"path"`
				JSONPath string `json:"jsonpath"`
			} `json:"extra-files"`
		} `json:"packages"`
	}
	raw, err = os.ReadFile(filepath.Join(repoRoot, "release-please-config.json"))
	if err != nil {
		t.Fatalf("read release-please-config.json: %v", err)
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("release-please-config.json is not valid JSON: %v", err)
	}
	const pluginManifest = "plugins/kb-steward/.claude-plugin/plugin.json"
	found := false
	for _, ef := range cfg.Packages["."].ExtraFiles {
		if ef.Path == pluginManifest && ef.Type == "json" && ef.JSONPath == "$.version" {
			found = true
		}
	}
	if !found {
		t.Errorf("release-please-config.json has no {type: json, path: %s, jsonpath: $.version} "+
			"extra-files entry — the plugin version would freeze at its current value", pluginManifest)
	}
}

// TestSkillContentIsHarnessNeutral keeps the skill body portable: the plugin
// manifests and SKILL.md's frontmatter are Claude Code packaging, but the
// instructions themselves must run under any agent that can read markdown
// (see docs/kb-steward.md, "Using it with another agent").
func TestSkillContentIsHarnessNeutral(t *testing.T) {
	// Vocabulary that would tie the instructions to one harness. Matching is
	// case-insensitive (below) so casing variants like "CLAUDE.md" don't slip
	// through; terms are listed here in the casing that should appear in any
	// failure message.
	banned := []string{
		"Claude", "/plugin", "slash command",
		"TodoWrite", "Task tool", "subagent", "Cursor", "Copilot", "Codex",
		"Bash tool", "Read tool", "Edit tool", "AskUserQuestion", "hooks",
	}
	files := []string{
		"skills/kb-steward/SKILL.md",
		"skills/kb-steward/references/okf-format.md",
		"skills/kb-steward/references/entry-quality-checklist.md",
		"skills/kb-steward/references/interview-guides.md",
	}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(pluginRoot, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		body := stripFrontmatter(string(raw)) // frontmatter is packaging metadata
		for _, word := range banned {
			if bannedTermRE(word).MatchString(body) {
				t.Errorf("%s: harness-specific term %q in skill body — the portable core must not name a specific agent or its tools", f, word)
			}
		}
	}
}

// bannedTermRE builds the case-insensitive matcher for one banned term.
//
// It anchors on a word boundary at the START of the term, because plain
// substring matching over-matches: "hooks" is a substring of "webhooks", and an
// SRE-facing skill has every reason to mention an Alertmanager webhook. Terms
// opening with punctuation ("/plugin") get no leading boundary — \b there would
// require a word character before the slash, so the term would stop matching at
// the start of a line.
//
// At the END it allows an optional plural "s" before the boundary, so tightening
// the front does not quietly stop catching "subagents" or "Task tools" the way a
// bare \b would.
func bannedTermRE(term string) *regexp.Regexp {
	pat := regexp.QuoteMeta(term)
	if isWordByte(term[0]) {
		pat = `\b` + pat
	}
	if isWordByte(term[len(term)-1]) {
		pat += `s?\b`
	}
	return regexp.MustCompile(`(?i)` + pat)
}

// isWordByte reports whether b is what RE2's \b treats as a word character.
// All banned terms are ASCII, so a byte test is exact here.
func isWordByte(b byte) bool {
	return b == '_' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// TestBannedTermREBoundaries is a mutation test for the guard above: it pins
// both directions of the boundary behaviour, so neither a regression to plain
// substring matching (which fails "webhooks") nor an over-strict anchor (which
// would silently stop catching "/plugin") can land unnoticed.
func TestBannedTermREBoundaries(t *testing.T) {
	cases := []struct {
		term, text string
		want       bool
	}{
		{"hooks", "configure hooks for this", true},
		{"hooks", "the Alertmanager webhooks route to Slack", false},
		{"hooks", "webhooks", false},
		{"Claude", "ask Claude to do it", true},
		{"Claude", "CLAUDE.md at the repo root", true}, // case-insensitive
		{"Claude", "clauded", false},
		{"/plugin", "/plugin install kb-steward", true}, // no leading \b needed
		{"/plugin", "run /plugin now", true},
		{"Task tool", "use the Task tool here", true},
		{"Task tool", "the Task tools listed", true}, // plural still caught
		{"subagent", "subagents are dispatched", true},
		{"subagent", "a subagent runs it", true},
	}
	for _, c := range cases {
		if got := bannedTermRE(c.term).MatchString(c.text); got != c.want {
			t.Errorf("bannedTermRE(%q).MatchString(%q) = %v, want %v", c.term, c.text, got, c.want)
		}
	}
}

// stripFrontmatter drops a leading YAML frontmatter block, which is harness
// packaging metadata rather than instruction content.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	if end := strings.Index(s[4:], "\n---"); end >= 0 {
		return s[4+end:]
	}
	return s
}
