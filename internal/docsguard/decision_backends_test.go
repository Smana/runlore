// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

// TestDecisionBackendsReadTheSameInEveryPlaceAnOperatorLooks pins the three places
// the decision model's backends are described to each other: the configuration page,
// the chart's values.yaml comments, and the field docs in config.go that every editor
// shows on hover. They drifted in three ways after #581 shipped, each in a different
// file: config.go omitted `shadow` from rerank_backend's valid values while
// config.Validate accepted it; two of the three said the reranker's backend answers
// "same incident pattern?", which is the DEDUP question (the reranker asks which
// candidate, if any, is the runbook); and all three said the annotate tier files "with
// the suspect named in the body", after the fix had moved the suspect into the PR/MR
// description only (KBEntry.SuspectedDuplicate: never into Body, because Body is
// committed into the catalog).
//
// The values are listed here rather than read from config.Validate because its switch
// is not introspectable; the switch's own error message names the same set, and a
// value added there without updating this list fails loudly on the next docs edit.
func TestDecisionBackendsReadTheSameInEveryPlaceAnOperatorLooks(t *testing.T) {
	root := repoRoot(t)
	sources := map[string]string{
		"configuration.md": readDoc(t, filepath.Join(root, "website", "content", "docs", "configuration", "configuration.md")),
		"values.yaml":      readDoc(t, filepath.Join(root, "deploy", "helm", "runlore", "values.yaml")),
		"config.go":        readDoc(t, filepath.Join(root, "internal", "config", "config.go")),
	}
	fields := []struct {
		key    string   // the key an operator searches for
		values []string // what config.Validate accepts (config.go: the rerank_backend / dedup_backend switches)
	}{
		{"rerank_backend", []string{"llm", "jev", "shadow"}},
		{"dedup_backend", []string{"bm25", "jev"}},
	}
	for name, text := range sources {
		flat := flattenProse(text)
		for _, f := range fields {
			// The passage that documents the key is the window around SOME mention of it
			// that names every value: the list sits after the key on the page and before
			// it in a values.yaml comment or a Go field doc (the `yaml:` tag comes last).
			// A window rather than the whole file, so a value mentioned somewhere
			// unrelated does not satisfy the check; every mention rather than the first,
			// because a page cross-references a key before it documents it.
			if !strings.Contains(flat, f.key) {
				t.Errorf("%s never mentions %s", name, f.key)
				continue
			}
			for _, v := range f.values {
				if !anyWindowContains(flat, f.key, 600, 900, v) {
					t.Errorf("%s documents %s without its valid value %q — config.Validate accepts it", name, f.key, v)
				}
			}
		}
		if strings.Contains(flat, "named in the body") {
			t.Errorf("%s still says the annotate tier names the suspect in the body; it is rendered into the "+
				"PR/MR description only (KBEntry.SuspectedDuplicate)", name)
		}
		if anyWindowContains(flat, "rerank_backend", 300, 300, "same incident pattern") {
			t.Errorf("%s says the reranker's backend answers \"same incident pattern?\" — that is the dedup "+
				"question; the reranker asks which candidate, if any, is the runbook", name)
		}
	}

	assertBackendValuesValidate(t, fields)
}

// anyWindowContains reports whether needle appears within before bytes before or
// after bytes after ANY occurrence of key in text.
func anyWindowContains(text, key string, before, after int, needle string) bool {
	for from := 0; ; {
		i := strings.Index(text[from:], key)
		if i < 0 {
			return false
		}
		i += from
		if strings.Contains(text[max(0, i-before):min(len(text), i+len(key)+after)], needle) {
			return true
		}
		from = i + len(key)
	}
}

func assertBackendValuesValidate(t *testing.T, fields []struct {
	key    string
	values []string
}) {
	t.Helper()
	// The list above is checked against the parser, not trusted: every documented
	// value must clear config.Validate without the field being named in an error, and
	// a value outside the list must be refused by name. So a value added to or removed
	// from the switch shows up here, before it shows up in a support thread. The
	// companions a backend requires (a bar, band edges) are set so that only the
	// backend value itself is under test.
	set := map[string]func(c *config.Config, v string){
		"rerank_backend": func(c *config.Config, v string) {
			c.Catalog.InstantRecall.RerankBackend = v
			c.Catalog.InstantRecall.RerankThresholdJev = 0.7
		},
		"dedup_backend": func(c *config.Config, v string) {
			c.Forge.DedupBackend = v
			c.Forge.DedupSkipAbove, c.Forge.DedupAnnotateAbove = 0.9, 0.7
		},
	}
	for _, f := range fields {
		for _, v := range append(f.values, "bogus") {
			var c config.Config
			config.ApplyDefaults(&c)
			set[f.key](&c, v)
			err := c.Validate()
			refused := err != nil && strings.Contains(err.Error(), f.key)
			switch {
			case v == "bogus" && !refused:
				t.Errorf("config.Validate does not refuse %s=%q by name, so the value list this guard "+
					"checks the docs against cannot be trusted: %v", f.key, v, err)
			case v != "bogus" && refused:
				t.Errorf("config.Validate refuses the documented %s=%q: %v", f.key, v, err)
			}
		}
	}
}
