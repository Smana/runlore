// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

// ignoreMarker matches the opt-out comment an author puts on the line before a
// YAML fence that is NOT a runlore.yaml — Helm chart values, a Kubernetes
// manifest, an Alertmanager receiver — or that is deliberately partial:
//
//	<!-- docsguard:ignore Helm chart values, not a runlore.yaml -->
//
// An HTML comment was chosen over a fence info-string suffix or a heading
// convention because it is the only one of the three that stays invisible to a
// reader (the site renders with goldmark `unsafe: true`, so the comment reaches
// the HTML as a comment and nothing else), carries a free-text reason a reviewer
// can weigh, and cannot collide with Chroma's info-string parsing. The reason is
// mandatory — see TestDocsYAMLFencesLoadAsConfig.
var ignoreMarker = regexp.MustCompile(`^<!--\s*docsguard:ignore\s*(.*?)\s*-->$`)

// docsFence is one fenced YAML block found in a docs page.
type docsFence struct {
	Page       string // repo-relative path, for failure messages
	Line       int    // 1-based line of the opening ``` fence
	Body       string // fence content, dedented to the fence's own indentation
	Ignore     bool   // an ignoreMarker sits immediately above it
	Reason     string // that marker's free-text reason
	MarkerLine int    // 1-based line of that marker
}

// docsMarker is an ignoreMarker occurrence, tracked separately so one that never
// reaches a fence can be reported instead of silently doing nothing.
type docsMarker struct {
	Line   int
	Reason string
}

// scanYAMLFences returns every fenced YAML block in one markdown page, plus any
// ignore marker that did not end up attached to a fence.
//
// This is a line scanner rather than one regular expression on purpose. The
// predecessor guard (TestIntegrationMinimalConfigsParse) checked exactly one
// fence per page and got there through two regex attempts that each silently
// skipped pages — first by requiring the fence to follow its heading
// immediately, then by excluding backticks from the prose in between. A guard
// that quietly checks a subset is worse than no guard, because it reads as
// coverage. A scanner that walks fences the way a markdown parser does has no
// such blind spot, and it also picks up the fences indented inside list items
// (getting-started.md, aws-cloud.md) that an anchored expression misses.
func scanYAMLFences(page string, src []byte) ([]docsFence, []docsMarker, error) {
	lines := strings.Split(string(src), "\n")
	var (
		fences  []docsFence
		orphans []docsMarker
		pending *docsMarker
	)
	dropPending := func() {
		if pending != nil {
			orphans = append(orphans, *pending)
			pending = nil
		}
	}
	for i := 0; i < len(lines); i++ {
		line := lines[i]
		trimmed := strings.TrimSpace(line)
		if m := ignoreMarker.FindStringSubmatch(trimmed); m != nil {
			dropPending() // two markers in a row: the first can never attach
			pending = &docsMarker{Line: i + 1, Reason: m[1]}
			continue
		}
		if trimmed == "" {
			continue // a blank line between marker and fence is still adjacency
		}
		if !strings.HasPrefix(trimmed, "```") {
			dropPending() // ordinary prose breaks marker -> fence adjacency
			continue
		}
		// Every fence is consumed, not just the YAML ones: skipping over a bash or
		// json block keeps its contents from being scanned as page text.
		indent := len(line) - len(strings.TrimLeft(line, " \t"))
		info := strings.TrimSpace(strings.TrimLeft(trimmed, "`"))
		end := -1
		for j := i + 1; j < len(lines); j++ {
			if strings.TrimSpace(lines[j]) == "```" {
				end = j
				break
			}
		}
		if end < 0 {
			return nil, nil, fmt.Errorf("%s:%d: fence is never closed", page, i+1)
		}
		if lang, _, _ := strings.Cut(info, " "); lang == "yaml" || lang == "yml" {
			f := docsFence{
				Page: page,
				Line: i + 1,
				Body: dedent(lines[i+1:end], indent),
			}
			if pending != nil {
				f.Ignore, f.Reason, f.MarkerLine = true, pending.Reason, pending.Line
				pending = nil
			}
			fences = append(fences, f)
		} else {
			dropPending()
		}
		i = end
	}
	dropPending()
	return fences, orphans, nil
}

// dedent removes up to n leading spaces/tabs from every line, so a fence nested
// in a list item parses as the YAML document the reader sees rather than as one
// uniformly indented block.
func dedent(lines []string, n int) string {
	out := make([]string, len(lines))
	for i, l := range lines {
		k := 0
		for k < n && k < len(l) && (l[k] == ' ' || l[k] == '\t') {
			k++
		}
		out[i] = l[k:]
	}
	return strings.Join(out, "\n") + "\n"
}

// TestDocsYAMLFencesLoadAsConfig loads EVERY YAML block on the docs site through
// the REAL loader, not a lookalike, and fails on any that a reader could paste
// into runlore.yaml only to have the agent refuse to start.
//
// Why this exists: config.Load decodes with KnownFields(true) and then runs
// Config.Validate, so a single key that does not exist — or a required companion
// key that is missing — is a hard startup failure. RunLore refuses to serve
// rather than silently ignoring what might be a safety-critical setting. That
// makes a wrong example on these pages worse than a missing one: the reader
// pastes it, the agent fails closed, and the page looks authoritative.
//
// It replaces a narrower guard that took the first fence after each "## Minimal
// config" heading, and so validated exactly one block per page while every
// per-feature snippet further down went unchecked. That gap shipped: a
// documented Slack block missing its delivery target reached final review, and a
// human caught it, not CI.
//
// The guard already has two catches to its name. Two pages once wrapped their
// block in a top-level `config:` key — the Helm values.yaml convention, where the
// chart unwraps .Values.config before writing runlore.yaml — rather than the raw
// config file the block claimed to be. Both failed with "field config not found
// in type config.Config": invisible to a human reader, obvious to the parser.
//
// Validation is opt-OUT, deliberately. The mistake worth catching is an author
// adding a complete config snippet that then goes unvalidated, so a new fence is
// checked unless explicitly marked (see ignoreMarker) — the marker costs a line,
// being wrong costs a broken page. And the marker cannot be used as a rubber
// stamp: a marked fence that loads cleanly is itself a failure below.
func TestDocsYAMLFencesLoadAsConfig(t *testing.T) {
	root := repoRoot(t)
	// The whole content tree, not just docs/: a config example on a landing or
	// comparison page misleads a reader exactly as much as one in the manual.
	dir := filepath.Join(root, "website", "content")

	var pages []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !d.IsDir() && filepath.Ext(d.Name()) == ".md" {
			pages = append(pages, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", dir, err)
	}

	tmp := t.TempDir()
	var total, validated, ignored int
	for _, page := range pages {
		body, rerr := os.ReadFile(page) //nolint:gosec // G304: path comes from WalkDir over a fixed in-repo doc tree
		if rerr != nil {
			t.Fatalf("read %s: %v", page, rerr)
		}
		rel, rerr := filepath.Rel(root, page)
		if rerr != nil {
			t.Fatalf("relativise %s: %v", page, rerr)
		}
		rel = filepath.ToSlash(rel)

		fences, orphans, serr := scanYAMLFences(rel, body)
		if serr != nil {
			t.Fatalf("scan fences: %v", serr)
		}
		for _, o := range orphans {
			t.Errorf("%s:%d: `docsguard:ignore` is not immediately above a YAML fence, so it "+
				"exempts nothing — move it onto the line before the fence it belongs to, or delete it",
				rel, o.Line)
		}

		for _, f := range fences {
			total++
			path := filepath.Join(tmp, fmt.Sprintf("fence-%d.yaml", total))
			if werr := os.WriteFile(path, []byte(f.Body), 0o600); werr != nil {
				t.Fatalf("write temp config for %s:%d: %v", f.Page, f.Line, werr)
			}
			_, lerr := config.Load(path)

			if f.Ignore {
				ignored++
				if f.Reason == "" {
					t.Errorf("%s:%d: `docsguard:ignore` carries no reason — write why this fence is "+
						"exempt (`<!-- docsguard:ignore Helm chart values, not a runlore.yaml -->`) "+
						"so a reviewer can tell an honest exemption from a silenced failure",
						f.Page, f.MarkerLine)
				}
				// The marker is for blocks that genuinely are not a loadable runlore.yaml.
				// One that loads cleanly is either a real config being needlessly skipped or
				// a stale exemption left behind after the block was fixed — both hide the
				// fence from the guard, which is the failure mode this test exists for.
				if lerr == nil {
					t.Errorf("%s:%d: marked `docsguard:ignore` (%q) but it loads cleanly as a "+
						"runlore.yaml — remove the marker at line %d so the guard keeps checking it",
						f.Page, f.Line, f.Reason, f.MarkerLine)
				}
				continue
			}

			validated++
			if lerr != nil {
				t.Errorf("%s:%d: this YAML block does not load as a runlore.yaml — a reader pasting "+
					"it gets a hard startup failure: %v\n\tIf it is not a runlore config (Helm values, "+
					"a Kubernetes manifest, an Alertmanager receiver) or is deliberately partial, put "+
					"`<!-- docsguard:ignore <reason> -->` on the line above the fence. "+
					"See CONTRIBUTING.md -> Docs site (Hugo).", f.Page, f.Line, lerr)
			}
		}
	}

	// Guard the guard, in both of the ways this can go quietly inert: a restructure
	// that moves the content tree or a scanner change that stops recognising fences
	// leaves nothing found at all; a blanket-marking spree leaves fences found but
	// none actually loaded. Either would let the test pass while checking nothing.
	if total == 0 {
		t.Fatal("no YAML fences found under website/content — the content tree moved or " +
			"scanYAMLFences stopped recognising fences, and this guard is now inert")
	}
	if validated == 0 {
		t.Fatalf("all %d YAML fences are marked `docsguard:ignore` — this guard now validates "+
			"nothing", total)
	}
	t.Logf("loaded %d/%d docs YAML fences through config.Load (%d marked docsguard:ignore)",
		validated, total, ignored)
}
