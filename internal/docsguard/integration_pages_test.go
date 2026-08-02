// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/logs"
	"github.com/Smana/runlore/internal/notify"
	_ "github.com/Smana/runlore/internal/notify/templated" // self-registers the "templated" notifier
	_ "github.com/Smana/runlore/internal/notify/webhook"   // self-registers the "webhook" notifier
	"github.com/Smana/runlore/internal/source"
	_ "github.com/Smana/runlore/internal/source/alertmanager" // self-registers the "alertmanager" source
	_ "github.com/Smana/runlore/internal/source/custom"       // self-registers the "custom" source
	_ "github.com/Smana/runlore/internal/source/gitops"       // self-registers the "gitops" source
	_ "github.com/Smana/runlore/internal/source/pagerduty"    // self-registers the "pagerduty" source
	// notify's own package (imported above) self-registers "slack" and "matrix"
	// directly via their own init() funcs — no blank import needed for those two.
)

// integrationFrontMatter is the subset of a docs/integrations page's YAML
// front matter this guard cares about. Integration is a pointer so a page
// with no `integration:` key (e.g. _index.md) decodes to a nil claim rather
// than a zero-valued one indistinguishable from `integration: {kind: "", id: ""}`.
type integrationFrontMatter struct {
	Integration *struct {
		Kind string `yaml:"kind"`
		ID   string `yaml:"id"`
	} `yaml:"integration"`
}

// integrationPage is one docs page's claim: "I document the id under kind".
type integrationPage struct {
	Path string
	Kind string
	ID   string
}

// reflectedRegistry is one of the three registries/constant-sets this guard
// can check pages against, because — unlike an LLM provider, cloud, network,
// MCP, or forge integration, which RunLore wires directly with no
// self-registering plugin — each of these has a runtime source of truth to
// reflect over instead of trusting a hand-maintained list.
type reflectedRegistry struct {
	source string          // named in failure messages, e.g. "source.Registered()"
	ids    map[string]bool // registered ids for this kind
}

// TestIntegrationPagesMatchRegistries walks website/content/docs/integrations
// and checks, in both directions, that the published integration list never
// drifts from what is actually wired:
//
//  1. every page claiming a reflected kind (source, notifier, logs) names an
//     id that is genuinely registered — a stale or typo'd id fails here;
//  2. every id genuinely registered under a reflected kind has a page — an
//     integration that ships in code but never got documented fails here.
//
// A page whose kind is anything else (llm, cloud, network, mcp, forge, ...)
// is silently skipped in both directions: those integrations have no runtime
// registry to reflect over (RunLore vendors them directly rather than
// through a self-registering plugin), so this guard has nothing to verify
// them against and must never manufacture a false failure on them.
func TestIntegrationPagesMatchRegistries(t *testing.T) {
	root := repoRoot(t)
	dir := filepath.Join(root, "website", "content", "docs", "integrations")

	pages := loadIntegrationPages(t, dir)

	registries := map[string]reflectedRegistry{
		"source":   {source: "source.Registered()", ids: idSet(sourceNames())},
		"notifier": {source: "notify.Registered()", ids: idSet(notifierNames())},
		"logs":     {source: "internal/logs/detect.go provider constants", ids: idSet([]string{logs.ProviderVictoriaLogs, logs.ProviderLoki})},
	}

	// Direction 1: every page claiming a reflected kind must resolve to a
	// genuinely registered id.
	for _, p := range pages {
		reg, ok := registries[p.Kind]
		if !ok {
			continue // not one of the three reflected kinds — nothing to check
		}
		if !reg.ids[p.ID] {
			t.Errorf("%s: integration.id %q claims kind %q but is not registered in %s — fix the id or remove the page", p.Path, p.ID, p.Kind, reg.source)
		}
	}

	// Direction 2: every registered id must have a page.
	for kind, reg := range registries {
		havePage := map[string]bool{}
		for _, p := range pages {
			if p.Kind == kind {
				havePage[p.ID] = true
			}
		}
		ids := make([]string, 0, len(reg.ids))
		for id := range reg.ids {
			ids = append(ids, id)
		}
		sort.Strings(ids) // deterministic failure order
		for _, id := range ids {
			if !havePage[id] {
				t.Errorf("%s %q (registered via %s) has no page under website/content/docs/integrations/ — add one with front matter `integration: {kind: %s, id: %s}`", kind, id, reg.source, kind, id)
			}
		}
	}
}

// sourceNames returns the Name field of every registered source descriptor.
func sourceNames() []string {
	descs := source.Registered()
	names := make([]string, len(descs))
	for i, d := range descs {
		names[i] = d.Name
	}
	return names
}

// notifierNames returns the Name field of every registered notifier descriptor.
func notifierNames() []string {
	descs := notify.Registered()
	names := make([]string, len(descs))
	for i, d := range descs {
		names[i] = d.Name
	}
	return names
}

func idSet(names []string) map[string]bool {
	out := make(map[string]bool, len(names))
	for _, n := range names {
		out[n] = true
	}
	return out
}

// loadIntegrationPages walks every markdown file under dir and returns the
// integration claim of each page that makes one. Pages without an
// `integration:` front-matter key (the section _index.md, and any future
// page that just isn't an integration doc) are skipped rather than treated
// as a malformed claim. A missing dir returns no pages rather than failing
// the test outright — direction 2 above already reports exactly what is
// missing, which is a clearer failure than "directory not found".
func loadIntegrationPages(t *testing.T, dir string) []integrationPage {
	t.Helper()
	var pages []integrationPage
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		fm := parseFrontMatter(t, path)
		if fm.Integration == nil {
			return nil
		}
		pages = append(pages, integrationPage{Path: path, Kind: fm.Integration.Kind, ID: fm.Integration.ID})
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		t.Fatalf("walk %s: %v", dir, err)
	}
	return pages
}

// parseFrontMatter decodes the YAML front matter (delimited by leading "---"
// lines, Hugo's default and the only form used under docs/) of a content
// file. It uses gopkg.in/yaml.v3, already a go.mod dependency (see
// internal/source/registry.go), rather than pulling in a second YAML library.
func parseFrontMatter(t *testing.T, path string) integrationFrontMatter {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: path comes from filepath.WalkDir over a fixed in-repo doc tree, not user input
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	const delim = "---\n"
	s := string(raw)
	if !strings.HasPrefix(s, delim) {
		t.Fatalf("%s: does not start with a YAML front-matter delimiter (---)", path)
	}
	rest := s[len(delim):]
	end := strings.Index(rest, "\n---")
	if end == -1 {
		t.Fatalf("%s: front matter is never closed with a trailing ---", path)
	}
	var fm integrationFrontMatter
	if err := yaml.Unmarshal([]byte(rest[:end]), &fm); err != nil {
		t.Fatalf("%s: invalid front-matter YAML: %v", path, err)
	}
	return fm
}

// repoRoot walks up from the test's working directory until it finds go.mod,
// so the doc-tree path resolves regardless of where `go test` is invoked
// from — same approach as internal/config/shipped_configs_test.go.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root (go.mod) not found")
		}
		dir = parent
	}
}
