// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// The alert manifests are the one published surface that links INTO the docs site
// from outside the site, which is exactly why their links rot unnoticed: Hugo's
// refLinksErrorLevel only sees links written as relrefs inside content, and
// hack/check-anchors.sh only reads rendered HTML. A runbook_url is neither. It is a
// bare string in a YAML file that nothing parses.
//
// That gap already shipped: all 24 runbook_url annotations pointed at
// https://github.com/Smana/runlore/blob/main/docs/observability.md#<anchor> long after
// docs/observability.md was deleted in the move to website/. Every doc guard passed,
// the site built clean, and an operator following the link from a firing alert got a
// GitHub 404 — at the worst possible moment to go looking for a runbook.
const (
	alertsDir       = "deploy/observability/alerts"
	runbookDoc      = "website/content/docs/operations/observability.md"
	runbookBase     = "https://runlore.io/docs/operations/observability/"
	minRunbookLinks = 30 // 15 alerts x 2 manifest flavours; raise as rules are added
)

var (
	runbookURLRE = regexp.MustCompile(`runbook_url:\s*"([^"]+)"`)
	alertNameRE  = regexp.MustCompile(`(?m)^\s*- alert:\s*(\S+)`)
	headingRE    = regexp.MustCompile(`(?m)^#{2,6}\s+(.+?)\s*$`)
)

// TestRunbookURLsResolveToARealHeading pins every runbook_url in every shipped alert
// manifest to a heading that actually exists in the observability page.
//
// It recomputes Hugo's anchor from the heading text rather than reading rendered HTML,
// because this test must fail in `go test ./...` with no Hugo present. The recomputation
// is deliberately narrow: it is only ever applied to headings this guard also requires to
// be plain ASCII alphanumerics (the alert names), so the em-dash and emoji cases that make
// a general Hugo anchor reimplementation wrong cannot arise here. hack/check-anchors.sh
// remains the authority for anchors in prose.
func TestRunbookURLsResolveToARealHeading(t *testing.T) {
	root := repoRoot(t)

	body, err := os.ReadFile(filepath.Join(root, runbookDoc))
	if err != nil {
		t.Fatalf("read %s: %v", runbookDoc, err)
	}
	anchors := map[string]string{}
	for _, m := range headingRE.FindAllStringSubmatch(string(body), -1) {
		anchors[hugoAnchor(m[1])] = m[1]
	}
	if len(anchors) == 0 {
		t.Fatalf("no headings found in %s — the page moved or its shape changed, and this guard is inert", runbookDoc)
	}

	links := 0
	for _, path := range alertManifests(t, root) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range runbookURLRE.FindAllStringSubmatch(string(raw), -1) {
			links++
			url := m[1]
			if !strings.HasPrefix(url, runbookBase) {
				t.Errorf("%s: runbook_url %q does not point at the published observability page.\n"+
					"  want a %s#<anchor> URL.\n"+
					"  A runbook_url is read by a human with a firing alert; nothing else validates it, "+
					"so a stale host or a path from before the docs moved is invisible until then.",
					rel, url, runbookBase)
				continue
			}
			frag := strings.TrimPrefix(url, runbookBase)
			if !strings.HasPrefix(frag, "#") {
				t.Errorf("%s: runbook_url %q has no #anchor — it lands the operator at the top of a long page", rel, url)
				continue
			}
			if _, ok := anchors[strings.TrimPrefix(frag, "#")]; !ok {
				t.Errorf("%s: runbook_url %q points at an anchor no heading in %s produces.\n"+
					"  Add a `### <AlertName>` runbook section for it, or fix the anchor.\n"+
					"  known anchors: %s",
					rel, url, runbookDoc, strings.Join(sortedSet(toSet(anchors)), ", "))
			}
		}
	}
	if links < minRunbookLinks {
		t.Fatalf("found only %d runbook_url annotations across %s, want at least %d — "+
			"the manifests moved or their shape changed, and this guard is checking almost nothing",
			links, alertsDir, minRunbookLinks)
	}
}

// TestEveryAlertHasARunbookSection is the other direction: a rule added without a
// runbook section still ships a runbook_url, and the test above would catch a WRONG
// anchor but not a rule whose author simply reused a neighbour's. Pinning alert name
// to heading makes the section mandatory at the moment the rule is written.
func TestEveryAlertHasARunbookSection(t *testing.T) {
	root := repoRoot(t)

	body, err := os.ReadFile(filepath.Join(root, runbookDoc))
	if err != nil {
		t.Fatalf("read %s: %v", runbookDoc, err)
	}
	headings := map[string]bool{}
	for _, m := range headingRE.FindAllStringSubmatch(string(body), -1) {
		headings[strings.TrimSpace(m[1])] = true
	}

	seen := map[string][]string{}
	for _, path := range alertManifests(t, root) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		rel, _ := filepath.Rel(root, path)
		for _, m := range alertNameRE.FindAllStringSubmatch(string(raw), -1) {
			seen[m[1]] = appendUnique(seen[m[1]], rel)
		}
	}
	if len(seen) == 0 {
		t.Fatalf("no `- alert:` entries found under %s — this guard is inert", alertsDir)
	}

	for _, name := range sortedKeys(seen) {
		if !headings[name] {
			t.Errorf("alert %s (in %s) has no `### %s` runbook section in %s.\n"+
				"  Its runbook_url therefore cannot resolve. Every rule an operator can be paged by "+
				"needs somewhere to land that says what to do.",
				name, strings.Join(seen[name], ", "), name, runbookDoc)
		}
	}

	// The two flavours must stay in lockstep: the manifests say so in their own header
	// comments ("change one, change the other"), and a rule present in only one of them
	// is a rule half the fleet never gets.
	for _, name := range sortedKeys(seen) {
		if len(seen[name]) != 2 {
			t.Errorf("alert %s appears only in %s — the PrometheusRule and VMRule flavours must carry "+
				"identical rules; whichever operator your cluster runs, it must get this alert.",
				name, strings.Join(seen[name], ", "))
		}
	}
}

// alertManifests lists the shipped rule files. It fails loudly on an empty result so a
// rename of the directory turns into a failure rather than a silently passing test.
func alertManifests(t *testing.T, root string) []string {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(root, alertsDir, "*.yaml"))
	if err != nil {
		t.Fatalf("glob %s: %v", alertsDir, err)
	}
	if len(matches) == 0 {
		t.Fatalf("no *.yaml under %s — the manifests moved and this guard is inert", alertsDir)
	}
	sort.Strings(matches)
	return matches
}

// hugoAnchor reproduces Hugo's "github" anchor style for the ASCII-alphanumeric
// headings this guard applies it to: lowercase, runs of non-alphanumerics collapsed to
// a single hyphen. See the doc comment on TestRunbookURLsResolveToARealHeading for why
// the narrow scope is safe here and is not a general reimplementation.
func hugoAnchor(heading string) string {
	var b strings.Builder
	prevHyphen := false
	for _, r := range strings.ToLower(strings.TrimSpace(heading)) {
		switch {
		case (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9'):
			b.WriteRune(r)
			prevHyphen = false
		default:
			if !prevHyphen && b.Len() > 0 {
				b.WriteByte('-')
				prevHyphen = true
			}
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

func toSet(m map[string]string) map[string]bool {
	out := make(map[string]bool, len(m))
	for k := range m {
		out[k] = true
	}
	return out
}
