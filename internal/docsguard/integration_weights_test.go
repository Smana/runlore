// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"fmt"
	"io/fs"
	"path/filepath"
	"sort"
	"testing"
)

// weightBands maps a band's floor to the sidebar category it represents. The order
// matches the sections on docs/integrations/_index.md, so the sidebar and the
// landing page tell the same story.
var weightBands = []struct {
	floor int
	name  string
}{
	{100, "Triggers"},
	{200, "LLM providers"},
	{300, "Data sources"},
	{400, "Notifications"},
	{500, "Forge"},
}

// TestIntegrationWeightsMatchTheirSection pins the sidebar order, and pins it
// against the ONE thing that can silently disagree with it: the directory a page
// lives in.
//
// History matters here. The pages used to sit in one flat directory numbered from
// 10 per category, which Hugo sorted as a single list — five pages tied at 10,
// five at 20, ties broken by title. The sidebar read
//
//	alertmanager (trigger) → github (forge) → openai-compatible (LLM) →
//	prometheus (data source) → slack (notifier)
//
// interleaving every category while each page's front matter looked deliberate.
// Two fixes landed for that: banded weights (triggers 100s, LLM 200s, …) and a
// move into per-type subdirectories. Either alone fixes the ordering; together
// they encode the section TWICE, and two encodings can drift apart.
//
// So this guard no longer checks bands in isolation — it checks that a page's
// weight band and its directory name agree. A page filed under forge/ carrying a
// 300-series weight is the failure it now catches: it renders in the Forge group
// but sorts as a data source, and nothing else in the build would object.
func TestIntegrationWeightsMatchTheirSection(t *testing.T) {
	dir := filepath.Join(repoRoot(t), "website", "content", "docs", "integrations")

	// section directory name -> band floor. Order matches docs/integrations/_index.md.
	sectionBand := map[string]int{
		"triggers": 100, "llm": 200, "data-sources": 300,
		"notifications": 400, "forge": 500,
	}

	seen := map[int]string{}
	checked := 0
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || filepath.Ext(d.Name()) != ".md" || d.Name() == "_index.md" {
			return nil
		}
		rel, rerr := filepath.Rel(dir, path)
		if rerr != nil {
			return rerr
		}
		section := filepath.ToSlash(filepath.Dir(rel))
		floor, known := sectionBand[section]
		if !known {
			t.Errorf("%s: %q is not one of the five integration sections (%s)", rel, section, bandList())
			return nil
		}
		w, ok := frontMatterWeight(t, path)
		if !ok {
			t.Errorf("%s: no `weight` in front matter — it sorts to 0 and leads its section", rel)
			return nil
		}
		if prev, dup := seen[w]; dup {
			t.Errorf("%s and %s both use weight %d — keep them unique so the order is never "+
				"decided by a title tiebreak", prev, rel, w)
			return nil
		}
		seen[w] = rel
		if w < floor || w >= floor+100 {
			t.Errorf("%s lives in %s/ (band %d-%d) but carries weight %d — it renders in one group "+
				"and sorts with another", rel, section, floor, floor+99, w)
		}
		checked++
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	// Guard the guard: a directory move or a front-matter rename would leave this
	// iterating over nothing and passing in silence.
	if checked < 10 {
		t.Fatalf("only %d integration pages had a weight — the page layout or front-matter key "+
			"changed and this guard is now inert", checked)
	}
	t.Logf("%d integration pages: weight band agrees with section directory", checked)
}

func bandList() string {
	parts := make([]string, 0, len(weightBands))
	for _, b := range weightBands {
		parts = append(parts, fmt.Sprintf("%d-%d %s", b.floor, b.floor+99, b.name))
	}
	sort.Strings(parts)
	return fmt.Sprint(parts)
}

// frontMatterWeight reads `weight` via the package's shared front-matter parser.
// An earlier version of this file re-implemented the ---/--- scan and matched
// nothing, passing its own inert check — reusing the real parser is the fix.
func frontMatterWeight(t *testing.T, path string) (int, bool) {
	t.Helper()
	fm := parseFrontMatter(t, path)
	if fm.Weight == nil {
		return 0, false
	}
	return *fm.Weight, true
}
