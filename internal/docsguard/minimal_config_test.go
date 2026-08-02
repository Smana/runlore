// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"bytes"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

// yamlFence matches a fenced YAML block. Finding the block is deliberately NOT one
// big expression: two earlier attempts each silently skipped pages, and a guard that
// quietly checks a subset is worse than no guard because it reads as coverage.
//
//	attempt 1 required the fence to follow the heading immediately — skipped the three
//	          pages that introduce the block with a line of prose (slack.md et al)
//	attempt 2 allowed prose but excluded backticks — skipped custom-webhook.md, whose
//	          intro sentence contains inline code (`POST /webhook/custom/<instance>`)
//
// So: locate the heading by index, then take the first YAML fence after it, with no
// constraint on what sits between. extractMinimalConfig does that.
var yamlFence = regexp.MustCompile("(?s)```yaml\\n(.*?)```")

// extractMinimalConfig returns the YAML of a page's "## Minimal config" block, and
// whether the page has one at all.
func extractMinimalConfig(body []byte) ([]byte, bool) {
	i := bytes.Index(body, []byte("## Minimal config"))
	if i < 0 {
		return nil, false
	}
	m := yamlFence.FindSubmatch(body[i:])
	if m == nil {
		return nil, false
	}
	return m[1], true
}

// TestIntegrationMinimalConfigsParse loads every integration page's "Minimal
// config" block through the REAL loader, not a lookalike.
//
// Why this exists: config.Load decodes with KnownFields(true), so a single key
// that does not exist on config.Config is a hard startup failure — RunLore
// refuses to serve rather than silently ignoring what might be a safety-critical
// setting. That makes a wrong example on these pages worse than a missing one:
// the reader pastes it, the agent fails closed, and the page looks authoritative.
//
// It has already caught one: two pages wrapped their block in a top-level
// `config:` key, which is the Helm values.yaml convention (the chart unwraps
// .Values.config before writing runlore.yaml) rather than the raw config file
// these blocks claim to be. Both failed with "field config not found in type
// config.Config" — invisible to a human reader, obvious to the parser.
func TestIntegrationMinimalConfigsParse(t *testing.T) {
	dir := filepath.Join("..", "..", "website", "content", "docs", "integrations")
	// WalkDir, not ReadDir: the pages live in per-type subdirectories
	// (triggers/, llm/, data-sources/, notifications/, forge/). A non-recursive
	// read would find zero pages — the checked==0 floor below is what turns that
	// into a loud failure instead of a guard that passes while checking nothing.
	var pages []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(d.Name()) != ".md" || d.Name() == "_index.md" {
			return nil
		}
		pages = append(pages, path)
		return nil
	})
	if err != nil {
		t.Fatalf("walk integrations dir: %v", err)
	}

	checked := 0
	for _, page := range pages {
		name := filepath.Base(page)
		body, err := os.ReadFile(page) //nolint:gosec // test-owned path under the repo
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		block, ok := extractMinimalConfig(body)
		if !ok {
			// Not every integration is configured through runlore.yaml — the MCP and
			// cloud pages document flags or in-cluster identity instead. A page with
			// no such block is simply not this guard's business.
			continue
		}

		tmp := filepath.Join(t.TempDir(), name+".yaml")
		if err := os.WriteFile(tmp, block, 0o600); err != nil {
			t.Fatalf("write temp config for %s: %v", name, err)
		}
		if _, err := config.Load(tmp); err != nil {
			t.Errorf("%s: the documented Minimal config does not load — a reader "+
				"pasting it gets a hard startup failure: %v", name, err)
		}
		checked++
	}

	// Guard the guard: a refactor that renames the heading or the fence would make
	// every page silently unmatched, and this test would pass while checking nothing.
	if checked == 0 {
		t.Fatal("no Minimal config blocks matched — the page template or this " +
			"expression changed, and the guard is now inert")
	}
	t.Logf("validated %d minimal-config blocks against config.Load", checked)
}
