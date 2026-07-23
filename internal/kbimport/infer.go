// SPDX-License-Identifier: Apache-2.0

// Package kbimport converts existing markdown runbooks/postmortems into
// OKF-compatible catalog entries — the cold-start answer: deterministic
// frontmatter inference (Infer), dedup against the live catalog (Plan), and
// an optional LLM refinement (Enrich). It is pure (no I/O, no clock); the
// command layer in internal/app does the walking, validating, and writing.
package kbimport

import (
	"fmt"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/kbvalidate"
	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

// Result is one source document converted to an importable entry.
type Result struct {
	Entry    providers.KBEntry // reuses the curator/forge entry struct — no parallel format
	Meta     okf.Meta          // preserved source timestamp/status/last_validated
	DestPath string            // bundle-relative destination, e.g. playbooks/redis-failover.md
	Source   string            // source path, for reporting
	Warnings []string
}

// sourceMeta is the tolerant superset of frontmatter keys recognized in a
// source document. Unknown keys are collected via the raw map for a warning.
type sourceMeta struct {
	Type          string   `yaml:"type"`
	Title         string   `yaml:"title"`
	Description   string   `yaml:"description"`
	Resource      string   `yaml:"resource"`
	Tags          []string `yaml:"tags"`
	Timestamp     string   `yaml:"timestamp"`
	Date          string   `yaml:"date"` // common in postmortems; folded into timestamp
	Status        string   `yaml:"status"`
	LastValidated string   `yaml:"last_validated"`
	// Extra collects any frontmatter key not matched above (yaml.v3 inline), so
	// the "keys not carried over" warning comes from the SAME parse, not a second one.
	Extra map[string]any `yaml:",inline"`
}

var validTypes = map[string]bool{"Incident": true, "Playbook": true, "Concept": true}

// Infer derives normalized OKF frontmatter for one markdown document, purely
// from its existing frontmatter, headings, and content. Deterministic by
// construction — same input, same entry — so imports are reviewable diffs,
// not model output. It never fabricates: resource is passthrough-only, and a
// document is an Incident only when it already carries the OKF evidence
// sections the merge gate demands.
func Infer(data []byte, source string) Result {
	r := Result{Source: source}
	fm, body := catalog.SplitFrontmatter(data)
	var meta sourceMeta
	if len(fm) > 0 {
		if err := yaml.Unmarshal(fm, &meta); err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("unparseable frontmatter ignored: %v", err))
		} else if extra := unknownKeys(meta.Extra); len(extra) > 0 {
			r.Warnings = append(r.Warnings, "frontmatter keys not carried over: "+strings.Join(extra, ", "))
		}
	}
	b := string(body)
	lines := strings.Split(b, "\n") // split once; the line-scanning helpers share it

	title := inferTitle(meta.Title, lines, source)
	resource := strings.TrimSpace(meta.Resource)
	if strings.ContainsAny(resource, " \t\r\n") {
		r.Warnings = append(r.Warnings, fmt.Sprintf("resource %q dropped: must be whitespace-free (namespace/name)", resource))
		resource = ""
	}
	typ := inferType(meta.Type, b, resource)

	r.Entry = providers.KBEntry{
		Type:        typ,
		Title:       title,
		Description: inferDescription(meta.Description, lines, title),
		Resource:    resource,
		Tags:        inferTags(meta.Tags, lines, typ),
		Body:        b,
	}
	r.Meta = okf.Meta{Status: meta.Status, LastValidated: meta.LastValidated}
	if ts := firstNonEmpty(meta.Timestamp, meta.Date); ts != "" {
		if _, ok := catalog.ParseEntryDate(ts); ok {
			r.Meta.Timestamp = ts
		} else {
			r.Warnings = append(r.Warnings, fmt.Sprintf("unparseable date %q dropped (want RFC3339 or 2006-01-02)", ts))
		}
	}
	r.DestPath = destPath(typ, title)
	return r
}

// destPath is the single dest-path rule (Infer and Enrich share it) so the whole
// "<type>s/<slug>.md" layout — not just the slug — can't drift between the two.
func destPath(typ, title string) string {
	return fmt.Sprintf("%ss/%s.md", strings.ToLower(typ), okf.Slugify(title))
}

// inferType: a valid declared type wins; else Incident iff the body already
// carries the gate's required sections AND a resource (Incident requires one);
// else Playbook — the relaxed, free-form-runbook type.
func inferType(declared, body, resource string) string {
	if validTypes[strings.TrimSpace(declared)] {
		return strings.TrimSpace(declared)
	}
	if resource != "" && kbvalidate.HasIncidentSections(body) {
		return "Incident"
	}
	return "Playbook"
}

// headingPrefixes are the markdown heading markers inferTitle promotes to a title.
var headingPrefixes = []string{"# ", "## "}

// inferTitle: frontmatter title → first #/## heading → humanized filename
// stem. Always capped to the merge gate's single-line 120-byte budget.
func inferTitle(declared string, lines []string, source string) string {
	if t := curator.CapTitle(declared); t != "" {
		return t
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		for _, p := range headingPrefixes {
			if strings.HasPrefix(t, p) {
				if h := curator.CapTitle(strings.TrimPrefix(t, p)); h != "" {
					return h
				}
			}
		}
	}
	stem := strings.TrimSuffix(filepath.Base(source), ".md")
	return curator.CapTitle(strings.NewReplacer("-", " ", "_", " ").Replace(stem))
}

// descriptionMaxRunes bounds the inferred description; it only has to be
// non-empty (the gate) and scannable in `lore kb search` output.
const descriptionMaxRunes = 240

// inferDescription: frontmatter description → first prose line of the body
// (headings, code fences, and blank lines skipped; list/quote markers
// stripped) → the title, so the gate's non-empty requirement always holds.
func inferDescription(declared string, lines []string, title string) string {
	if d := strings.TrimSpace(declared); d != "" {
		return capRunes(strings.Join(strings.Fields(d), " "), descriptionMaxRunes)
	}
	inFence := false
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			continue
		}
		if inFence || t == "" || strings.HasPrefix(t, "#") || t == "---" {
			continue
		}
		t = strings.TrimSpace(strings.TrimLeft(t, "-*> "))
		if t != "" {
			return capRunes(strings.Join(strings.Fields(t), " "), descriptionMaxRunes)
		}
	}
	return title
}

// maxTags bounds the tag list; tags feed the BM25 corpus, and past ~10 the
// extra ones are noise, not recall signal.
const maxTags = 10

// alertNameRe matches CamelCase tokens with ≥2 humps — the Prometheus
// alert-name shape (KubePodCrashLooping, TargetDown), including embedded
// acronym runs (KubeContainerOOMKilled). It requires a leading Upper+lower
// hump so lone acronyms (OOM, API) and ordinary single words (Redis) don't
// match; only headings and alert-mentioning lines are scanned, which keeps
// ordinary CamelCase prose (product names) from flooding the tags.
var alertNameRe = regexp.MustCompile(`\b[A-Z][a-z0-9]+(?:[A-Z]+[a-z0-9]*)+\b`)

// inferTags mirrors curator.entryTags' shape (constant pair first, derived
// signal after, deduped, lowercased): imported + type, then the source's own
// tags, then detected alert-name patterns.
func inferTags(declared []string, lines []string, typ string) []string {
	tags := []string{"imported", strings.ToLower(typ)}
	seen := map[string]bool{tags[0]: true, tags[1]: true}
	add := func(t string) {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" && !seen[t] && len(tags) < maxTags {
			seen[t] = true
			tags = append(tags, t)
		}
	}
	for _, t := range declared {
		add(t)
	}
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "#") && !strings.Contains(strings.ToLower(t), "alert") {
			continue
		}
		for _, m := range alertNameRe.FindAllString(t, -1) {
			add(m)
		}
	}
	return tags
}

// unknownKeys lists (sorted) the frontmatter keys Infer did not carry over —
// the yaml.v3 inline catch-all (sourceMeta.Extra) already holds exactly those,
// so the import report tells the user what was left behind without a re-parse.
func unknownKeys(extra map[string]any) []string {
	out := make([]string, 0, len(extra))
	for k := range extra {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n]), " ") + "…"
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return strings.TrimSpace(b)
}
