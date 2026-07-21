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
	"unicode"
)

// The kb-steward Claude Code plugin is served from this repo (see
// docs/kb-steward.md). These tests keep the skill's documented OKF contract
// and the plugin manifests from drifting as the loader evolves.

const pluginRoot = "../../plugins/kb-steward"

var parsedFieldsRE = regexp.MustCompile("`([a-z_]+)`")

// TestOKFFormatDocMatchesLoader pins the skill's documented frontmatter field
// list to what the loader actually parses (the frontmatter fields of Entry).
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
	et := reflect.TypeOf(Entry{})
	for i := 0; i < et.NumField(); i++ {
		name := et.Field(i).Name
		if name == "Body" || name == "Path" { // not frontmatter
			continue
		}
		parsed[snakeCase(name)] = true
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
	raw, err := os.ReadFile("../../.claude-plugin/marketplace.json")
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

// TestSkillContentIsHarnessNeutral keeps the skill body portable: the plugin
// manifests and SKILL.md's frontmatter are Claude Code packaging, but the
// instructions themselves must run under any agent that can read markdown
// (see docs/kb-steward.md, "Using it with another agent").
func TestSkillContentIsHarnessNeutral(t *testing.T) {
	// Vocabulary that would tie the instructions to one harness.
	banned := []string{
		"Claude", "claude", "/plugin", "slash command",
		"TodoWrite", "Task tool", "subagent", "Cursor", "Copilot", "Codex",
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
			if strings.Contains(body, word) {
				t.Errorf("%s: harness-specific term %q in skill body — the portable core must not name a specific agent or its tools", f, word)
			}
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

func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
