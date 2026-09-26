// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

// backendField is one backend-selecting key and the values config.Validate accepts
// for it, read from the same exported set Validate ranges over.
type backendField struct {
	key    string
	values []string
}

// TestDecisionBackendsReadTheSameInEveryPlaceAnOperatorLooks pins the three places
// the decision model's backends are described to each other and to the parser: the
// configuration page, the chart's values.yaml comments, and the field docs in
// config.go that every editor shows on hover. After #581 they had drifted three ways,
// each in a different file, and the three t.Errorf messages below name them.
func TestDecisionBackendsReadTheSameInEveryPlaceAnOperatorLooks(t *testing.T) {
	root := repoRoot(t)
	sources := map[string]string{
		"configuration.md": readDoc(t, filepath.Join(root, "website", "content", "docs", "configuration", "configuration.md")),
		"values.yaml":      readDoc(t, filepath.Join(root, "deploy", "helm", "runlore", "values.yaml")),
		"config.go":        readDoc(t, filepath.Join(root, "internal", "config", "config.go")),
	}
	fields := []backendField{
		{"rerank_backend", config.RerankBackends},
		{"dedup_backend", config.DedupBackends},
	}
	for name, text := range sources {
		flat := flattenProse(text)
		for _, f := range fields {
			passage, ok := passageFor(name, flat, f.key)
			if !ok {
				t.Errorf("%s never documents %s", name, f.key)
				continue
			}
			for _, v := range f.values {
				if !strings.Contains(passage, v) {
					t.Errorf("%s documents %s without its valid value %q — config.Validate accepts it", name, f.key, v)
				}
			}
		}
		if suspectInBody.MatchString(flat) {
			t.Errorf("%s still says the annotate tier names the suspect in the body; it is rendered into the "+
				"PR/MR description only (KBEntry.SuspectedDuplicate)", name)
		}
		if w, _ := allWindows(flat, "rerank_backend", 300, 300); strings.Contains(w, "same incident pattern") {
			t.Errorf("%s says the reranker's backend answers \"same incident pattern?\" — that is the dedup "+
				"question; the reranker asks which candidate, if any, is the runbook", name)
		}
	}

	// The exported sets are the parser's own, so the one thing left to prove is that
	// Validate really ranges over them: every listed value clears it without the field
	// being named in an error, and a value outside it is refused by name. The
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
		for _, v := range append(append([]string{}, f.values...), "bogus") {
			var c config.Config
			config.ApplyDefaults(&c)
			set[f.key](&c, v)
			err := c.Validate()
			refused := err != nil && strings.Contains(err.Error(), f.key)
			switch {
			case v == "bogus" && !refused:
				t.Errorf("config.Validate does not refuse %s=%q by name: %v", f.key, v, err)
			case v != "bogus" && refused:
				t.Errorf("config.Validate refuses its own listed %s=%q: %v", f.key, v, err)
			}
		}
	}
}

// suspectInBody matches every spelling the stale claim shipped in: "named in the
// body" on the page and in values.yaml, "naming the suspect in the body" in config.go.
var suspectInBody = regexp.MustCompile(`nam(ed|ing)[^.]{0,60}in the body`)

// fieldDocs names the Go field whose doc comment documents each key in config.go.
var fieldDocs = map[string]string{"rerank_backend": "RerankBackend", "dedup_backend": "DedupBackend"}

// passageFor returns the text that documents key in the named source. In config.go it
// is the field's doc comment, delimited exactly (from "<Field> selects" to the `yaml:`
// tag) and NOT a window: config.Validate's error string in the same file lists every
// value, so a window around the tag was satisfied by the validator and never by the
// doc a reader hovers on — which is how the `shadow` omission slipped past the first
// version of this guard. On the page and in values.yaml it is the windows around every
// mention of the key: the list sits after the key on the page and before it in a
// values comment, and a page cross-references a key before it documents it. ok is
// false when the source does not document the key at all.
func passageFor(source, flat, key string) (string, bool) {
	if source != "config.go" {
		return allWindows(flat, key, 600, 900)
	}
	from := strings.Index(flat, fieldDocs[key]+" selects")
	to := strings.Index(flat, `yaml:"`+key+`"`)
	if from < 0 || to < from {
		return "", false
	}
	return flat[from:to], true
}

// allWindows concatenates the window around every occurrence of key, so a needle
// present near ANY mention satisfies a Contains on the result. ok is false when key
// never occurs.
func allWindows(text, key string, before, after int) (string, bool) {
	var b strings.Builder
	for from := 0; ; {
		i := strings.Index(text[from:], key)
		if i < 0 {
			return b.String(), b.Len() > 0
		}
		i += from
		b.WriteString(text[max(0, i-before):min(len(text), i+len(key)+after)])
		b.WriteString("\n")
		from = i + len(key)
	}
}
