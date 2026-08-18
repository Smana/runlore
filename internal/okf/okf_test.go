// SPDX-License-Identifier: Apache-2.0

package okf

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

func TestRenderFrontmatterAndBody(t *testing.T) {
	out := Render(providers.KBEntry{
		Type: "Playbook", Title: "Redis failover", Description: "how to fail over redis",
		Tags: []string{"imported", "playbook"}, Body: "# Redis failover\n\nsteps",
	}, Meta{Timestamp: "2024-03-01"})
	for _, want := range []string{
		"---\n", "type: Playbook\n", "title: Redis failover\n",
		"timestamp: \"2024-03-01\"", "tags:\n", "- imported\n",
		"# Redis failover\n\nsteps\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Render missing %q:\n%s", want, out)
		}
	}
}

func TestRenderOmitsEmptyMeta(t *testing.T) {
	out := Render(providers.KBEntry{Type: "Playbook", Title: "T", Description: "d"}, Meta{})
	for _, absent := range []string{"timestamp:", "status:", "last_validated:", "fingerprint:", "resource:"} {
		if strings.Contains(out, absent) {
			t.Fatalf("empty %s must be omitted:\n%s", absent, out)
		}
	}
}

func TestRenderYAMLInjectionSafeTitle(t *testing.T) {
	// Marshaled (not string-formatted), so a colon-bearing title can't inject keys.
	out := Render(providers.KBEntry{Type: "Playbook", Title: "a: b\nresource: evil", Description: "d"}, Meta{})
	if strings.Contains(out, "\nresource: evil\n") {
		t.Fatalf("newline title must not inject a frontmatter key:\n%s", out)
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Redis failover — March 2024!", "redis-failover-march-2024"},
		{"  KubePodCrashLooping  ", "kubepodcrashlooping"},
		{"---", ""},
	}
	for _, c := range cases {
		if got := Slugify(c.in); got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestAsEntryIsWhatTheLoaderWouldProduce pins AsEntry as Render's read-side
// twin: for the same KBEntry and Meta, the struct it returns must equal the one
// catalog.Load parses back out of the file Render wrote.
//
// That equality is the whole point of the adapter. Every caller that validates
// an entry it has NOT yet written — the curator before it opens a pull request,
// thread capture before it opens one, `lore kb import` before it copies a
// document in — has to hand the validator a catalog.Entry it built by hand, and
// each hand-built copy silently answers a slightly different question than the
// merge gate will. Asserting it against the real loader, rather than against a
// field list restated here, is what makes "valid at draft" and "valid at merge"
// one predicate instead of two that look alike.
func TestAsEntryIsWhatTheLoaderWouldProduce(t *testing.T) {
	e := providers.KBEntry{
		Type: "Incident", Title: "Harbor registry unavailable",
		Description: "the registry pod was OOMKilled after the chart bump",
		Resource:    "tooling/harbor-registry", AlertResource: "tooling/harbor",
		Tags: []string{"runlore", "incident"}, Fingerprint: "fp-1",
		Confidence: 0.9, Provenance: []string{"crossplane/xplane-harbor"},
		Body: "## Symptom\n\ns\n\n## Cause\n\nc\n\n## Resolution\n\nr",
	}
	m := Meta{Timestamp: "2026-08-18", Status: "active", LastValidated: "2026-08-18"}

	const rel = "incidents/harbor-registry-unavailable.md"
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, filepath.Dir(rel)), 0o750); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, rel), []byte(Render(e, m)), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, skipped, err := catalog.Load(dir)
	if err != nil {
		t.Fatalf("catalog.Load: %v", err)
	}
	if len(skipped) != 0 || len(loaded) != 1 {
		t.Fatalf("loaded %d entries, skipped %v; want exactly the one Render wrote", len(loaded), skipped)
	}

	got, want := AsEntry(e, m, rel), loaded[0]
	// The only licensed difference: Render frames the body with the separating
	// blank line and a trailing newline the file format needs, which the loader
	// hands back. Nothing else may differ.
	got.Body, want.Body = strings.TrimSpace(got.Body), strings.TrimSpace(want.Body)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("AsEntry disagrees with the loader:\n got: %#v\nwant: %#v", got, want)
	}
}
