// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

// minimalConfigBlock captures the YAML an integration page tells the reader to
// paste. Pages follow one template, so one expression covers all of them.
// The heading is not always followed immediately by the fence — several pages
// introduce the block with a line of prose first (slack.md offers the incoming-webhook
// form before showing it). Matching only the adjacent case silently skipped three
// pages, so this deliberately allows anything up to the FIRST yaml fence that follows.
var minimalConfigBlock = regexp.MustCompile("(?s)## Minimal config\\n(?:[^`]*?)```yaml\\n(.*?)```")

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
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read integrations dir: %v", err)
	}

	checked := 0
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".md" || e.Name() == "_index.md" {
			continue
		}
		page := filepath.Join(dir, e.Name())
		body, err := os.ReadFile(page) //nolint:gosec // test-owned path under the repo
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		m := minimalConfigBlock.FindSubmatch(body)
		if m == nil {
			// Not every integration is configured through runlore.yaml — the MCP and
			// cloud pages document flags or in-cluster identity instead. A page with
			// no such block is simply not this guard's business.
			continue
		}

		tmp := filepath.Join(t.TempDir(), e.Name()+".yaml")
		if err := os.WriteFile(tmp, m[1], 0o600); err != nil {
			t.Fatalf("write temp config for %s: %v", e.Name(), err)
		}
		if _, err := config.Load(tmp); err != nil {
			t.Errorf("%s: the documented Minimal config does not load — a reader "+
				"pasting it gets a hard startup failure: %v", e.Name(), err)
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
