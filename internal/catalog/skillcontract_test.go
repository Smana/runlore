// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// The kb-steward plugin is served from this repo (see docs/kb-steward.md).
// These tests keep the skill's documented OKF contract and the plugin manifests
// from drifting as the loader evolves.

const repoRoot = "../.."
const pluginRoot = repoRoot + "/plugins/kb-steward"
const skillRoot = pluginRoot + "/skills/kb-steward"

// Field names in the parsed-fields block. The class admits digits and uppercase
// so a future yaml tag like `sha256` fails the comparison below rather than
// being silently skipped by the scanner.
var parsedFieldsRE = regexp.MustCompile("`([a-zA-Z0-9_]+)`")

// skillMarkdown lists every markdown file shipped in the skill, discovered by
// walking rather than hardcoded: a hardcoded list silently under-covers the
// moment someone adds a reference file, which is exactly when the neutrality
// scan below matters most.
func skillMarkdown(t *testing.T) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(skillRoot, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && strings.HasSuffix(d.Name(), ".md") {
			out = append(out, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", skillRoot, err)
	}
	if len(out) == 0 {
		t.Fatalf("no markdown found under %s — the skill is the thing under test", skillRoot)
	}
	return out
}

// readJSON reads and unmarshals a repo-relative JSON file into T.
func readJSON[T any](t *testing.T, rel string) T {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(repoRoot, rel))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	var v T
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("%s is not valid JSON: %v", rel, err)
	}
	return v
}

// TestOKFFormatDocMatchesLoader pins the skill's documented frontmatter field
// list to what the loader actually parses. It reflects over entryMeta's yaml
// tags (load.go) — the struct parseEntry unmarshals into — rather than Entry's
// Go field names, because Entry is a derived in-memory shape: renaming a yaml
// tag on entryMeta without touching Entry would leave a name-based check on
// Entry green while the loader's real contract drifted from the doc.
//
// It checks the per-field table as well as the marker block. The doc states the
// field list twice, so pinning only the block would let the table — the part a
// reader actually consults — keep describing a field the loader dropped.
func TestOKFFormatDocMatchesLoader(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(skillRoot, "references/okf-format.md"))
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
			t.Errorf("loader parses frontmatter field %q but okf-format.md's parsed-fields block does not list it", f)
		}
		if !strings.Contains(doc, "| `"+f+"` |") {
			t.Errorf("loader parses frontmatter field %q but okf-format.md's field table has no row for it", f)
		}
	}
	for f := range documented {
		if !parsed[f] {
			t.Errorf("okf-format.md documents field %q but the loader does not parse it", f)
		}
	}
}

// TestPluginManifestsValid keeps the marketplace/plugin manifests installable:
// docs tell users to type `kb-steward@runlore`.
func TestPluginManifestsValid(t *testing.T) {
	marketplace := readJSON[struct {
		Name  string `json:"name"`
		Owner struct {
			Name string `json:"name"`
		} `json:"owner"`
		Plugins []struct {
			Name        string `json:"name"`
			Source      string `json:"source"`
			Description string `json:"description"`
		} `json:"plugins"`
	}](t, ".claude-plugin/marketplace.json")

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
		t.Error("plugin description must be set (shown in plugin listings)")
	}

	plugin := readJSON[struct {
		Name string `json:"name"`
	}](t, "plugins/kb-steward/.claude-plugin/plugin.json")
	if plugin.Name != "kb-steward" {
		t.Errorf("plugin name = %q, want kb-steward", plugin.Name)
	}

	// SKILL.md is the entrypoint; the references are what it loads.
	for _, p := range []string{
		"SKILL.md",
		"references/okf-format.md",
		"references/entry-quality-checklist.md",
		"references/interview-guides.md",
	} {
		if _, err := os.Stat(filepath.Join(skillRoot, p)); err != nil {
			t.Errorf("skill file missing: %s: %v", p, err)
		}
	}
}

// TestPluginVersionTracksRelease pins the plugin manifest's version to the
// repo's released version. The two are kept in sync by release-please, which
// rewrites plugin.json through the extra-files entry asserted below; without
// that wiring the manifest silently freezes at its initial version while the
// project releases on, which is exactly how it read before this test existed.
func TestPluginVersionTracksRelease(t *testing.T) {
	manifest := readJSON[map[string]string](t, ".release-please-manifest.json")
	want := manifest["."]
	if want == "" {
		t.Fatal(`release-please manifest has no version for the "." package`)
	}

	plugin := readJSON[struct {
		Version string `json:"version"`
	}](t, "plugins/kb-steward/.claude-plugin/plugin.json")
	if plugin.Version != want {
		t.Errorf("plugin.json version = %q, want %q (the released version); "+
			"release-please should be bumping it via extra-files", plugin.Version, want)
	}

	// The equality above only keeps holding while release-please is told to
	// rewrite the file, so pin that wiring too.
	cfg := readJSON[struct {
		Packages map[string]struct {
			ExtraFiles []struct {
				Type     string `json:"type"`
				Path     string `json:"path"`
				JSONPath string `json:"jsonpath"`
			} `json:"extra-files"`
		} `json:"packages"`
	}](t, "release-please-config.json")

	const pluginManifest = "plugins/kb-steward/.claude-plugin/plugin.json"
	found := 0
	for _, ef := range cfg.Packages["."].ExtraFiles {
		if ef.Path == pluginManifest {
			if ef.Type != "json" || ef.JSONPath != "$.version" {
				t.Errorf("extra-files entry for %s has type=%q jsonpath=%q, want json / $.version",
					pluginManifest, ef.Type, ef.JSONPath)
			}
			found++
		}
	}
	if found != 1 {
		t.Errorf("release-please-config.json has %d extra-files entries for %s, want exactly 1 — "+
			"none means the plugin version freezes; duplicates mean conflicting rewrites", found, pluginManifest)
	}
}

// bannedVocabulary is harness-coupled wording that must not appear in the
// portable skill. Terms are matched leniently (see bannedTermRE), so list the
// stem: "subagent" also catches "subagents".
//
// Deliberately NOT listed, because they are ordinary vocabulary an SRE-facing
// skill has every reason to use — banning them would fail the build on correct
// prose, which is worse than missing a term:
//
//	"hook"   — lifecycle hooks, PreStop hooks, Flux post-build hooks, git hooks.
//	           The harness-specific sense is "<harness> hooks", already caught
//	           by the harness name itself.
//	"cursor" — a pagination or database cursor.
var bannedVocabulary = []string{
	"Claude", "/plugin", "slash command",
	"TodoWrite", "Task tool", "subagent", "Copilot", "Codex",
	"Bash tool", "Read tool", "Edit tool", "AskUserQuestion",
}

// TestSkillContentIsHarnessNeutral keeps the skill body portable: the plugin
// manifests and SKILL.md's frontmatter are packaging, but the instructions
// themselves must run under any agent that can read markdown (see
// docs/kb-steward.md, "Using it with another agent").
func TestSkillContentIsHarnessNeutral(t *testing.T) {
	res := make([]*regexp.Regexp, len(bannedVocabulary))
	for i, term := range bannedVocabulary {
		res[i] = bannedTermRE(term)
	}
	for _, path := range skillMarkdown(t) {
		raw, err := os.ReadFile(path) //nolint:gosec // G304: path comes from walking the in-repo skill dir
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		_, body := SplitFrontmatter(raw) // frontmatter is packaging metadata
		for i, term := range bannedVocabulary {
			if res[i].Match(body) {
				t.Errorf("%s: harness-specific term %q in skill body — the portable core must not name a specific agent or its tools",
					filepath.Base(path), term)
			}
		}
	}
}

// bannedTermRE builds the case-insensitive matcher for one banned term.
//
// It anchors the START on a non-word character (or start-of-text) rather than
// using \b, so terms opening with punctuation ("/plugin") still anchor. That
// front anchor is what keeps "hook" from firing on "webhook" — a word an
// SRE-facing skill has every reason to use.
//
// There is deliberately NO end anchor: harness vocabulary shows up glued to
// other word characters ("CLAUDE_CODE", "ClaudeCode", "subagents"), and a
// trailing \b would miss every one of those.
//
// Whitespace inside a term matches any run of non-word characters, because the
// skill files are hard-wrapped: "Task tool" has to match across a line break
// and around markdown punctuation ("`Task` tool") too.
func bannedTermRE(term string) *regexp.Regexp {
	parts := strings.Fields(term)
	for i, p := range parts {
		parts[i] = regexp.QuoteMeta(p)
	}
	return regexp.MustCompile(`(?i)(^|\W)` + strings.Join(parts, `[\W_]+`))
}

// TestBannedTermREBoundaries is a mutation test for the matcher above. It pins
// both directions: the substring false positives the front anchor exists to
// prevent, and the realistic evasions a trailing \b would let through.
//
// The "hook"/"webhook" pair tests the matcher, not the list — "hook" is
// deliberately absent from bannedVocabulary (see there). It stays here because
// it is the clearest case of the property the front anchor provides.
func TestBannedTermREBoundaries(t *testing.T) {
	cases := []struct {
		name, term, text string
		want             bool
	}{
		{"plain match", "hook", "configure hooks for this", true},
		{"singular too", "hook", "register a hook here", true},
		{"webhook is not a hook", "hook", "Alertmanager webhooks route to Slack", false},
		{"webhook singular", "hook", "the webhook fires", false},
		{"case-insensitive", "Claude", "CLAUDE.md at the repo root", true},
		{"underscore join", "Claude", "CLAUDE_CODE_SETTINGS", true},
		{"camel join", "Claude", "ClaudeCode is the harness", true},
		{"punctuation-leading term", "/plugin", "/plugin install kb-steward", true},
		{"punctuation-leading mid-line", "/plugin", "run /plugin now", true},
		{"multiword", "Task tool", "use the Task tool here", true},
		{"multiword across a line wrap", "Task tool", "use the Task\ntool to fan out", true},
		{"multiword around markdown", "Task tool", "use the `Task` tool", true},
		{"multiword hyphenated", "slash command", "a slash-command named /kb", true},
		{"plural", "subagent", "subagents are dispatched", true},
		{"unrelated prose stays clean", "Claude", "the cloud provider region", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := bannedTermRE(c.term).MatchString(c.text); got != c.want {
				t.Errorf("bannedTermRE(%q).MatchString(%q) = %v, want %v", c.term, c.text, got, c.want)
			}
		})
	}
}
