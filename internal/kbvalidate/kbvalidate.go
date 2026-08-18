// SPDX-License-Identifier: Apache-2.0

// Package kbvalidate provides deterministic structural validation of OKF
// knowledge-base entries (the merge gate) and an LLM-assisted semantic advisory.
// The structural checks mirror what curator.draftKBEntry emits and what
// catalog.parseEntry consumes.
//
// That correspondence used to be asserted here as holding "by construction", with
// nothing checking it: a drafted entry first met this validator in the catalog
// repo's CI, days later, by which point its pull request was already open and
// unmergeable. WarnDraft (see draft.go) now runs ValidateStructural on every entry
// RunLore drafts, before the PR is opened, logs what fails and COUNTS it
// (runlore_kb_draft_defects_total, labelled per condition — a log line is only
// read by someone already looking, and nothing was looking). It deliberately does
// not BLOCK — an entry a human has to fix beats an investigation thrown away.
//
// SCOPE, stated rather than implied: EVERY entry writer runs that guard, not just
// the one that first needed it. RunLore has two — curator.Curate, drafting a
// finding, and thread.ConceptEntry, opened by thread.Responder's standalone-note
// route — and each calls WarnDraft before opening its pull request. The report
// takes an entry's own type as given, which is what lets one guard serve both: a
// thread note is deliberately typed Concept, so the Incident-only resource and
// section rules do not apply to it, while the rules that DO apply (a description,
// a single-line title, a matchable recall index) are now checked at draft time on
// both paths instead of in the catalog repo's CI, days downstream.
package kbvalidate

import (
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
)

// Severity classifies an Issue. Only SeverityError fails the merge gate.
type Severity int

// Issue severities. Only SeverityError fails the merge gate; SeverityWarning is
// advisory.
const (
	SeverityError Severity = iota
	SeverityWarning
)

// String renders the severity for human/CI output.
func (s Severity) String() string {
	if s == SeverityWarning {
		return "warning"
	}
	return "error"
}

// Issue is one validation finding against a frontmatter field or body section.
type Issue struct {
	Severity Severity
	Field    string
	Message  string
}

var validTypes = map[string]bool{"Incident": true, "Playbook": true, "Concept": true}

// maxTitleLen is the gate's maximum title length in BYTES — it is compared
// against len(title), so a title of accented characters hits it at roughly half
// that many characters. A package const rather than a bare 120 restated in both
// the check and its error message, which also lets checklist_test.go pin the
// skill docs' restated limit to this single source of truth.
const maxTitleLen = 120

// requiredIncidentSections are the OKF body sections an Incident must carry
// (present and non-empty); curator.draftKBEntry always renders them.
var requiredIncidentSections = []struct{ key, head string }{
	{"symptom", "Symptom"},
	{"cause", "Cause"},
	{"resolution", "Resolution"},
}

// HasIncidentSections reports whether body carries every required Incident
// evidence section (Symptom/Cause/Resolution), each present and non-empty — the
// same rule ValidateStructural enforces for Incidents. Exposed so `lore kb
// import` classifies a document as an Incident by the exact gate it must pass,
// rather than restating the section set.
func HasIncidentSections(body string) bool {
	secs := Sections(body)
	for _, s := range requiredIncidentSections {
		if secs[s.key] == "" {
			return false
		}
	}
	return true
}

// WarnInvalid is the load-time strict-warn hook: it calls onInvalid(path, errs)
// for each invalid entry; the caller logs + increments a metric, but the entry
// is still served (one bad entry never empties the catalog). Returns the count
// of invalid entries. Warnings are not reported here.
//
// It is deliberately looser than the merge gate: OKF conformance (§9) requires
// consumers to tolerate unknown types gracefully, so an entry outside the
// RunLore vocabulary (a foreign bundle's "Metric", "API Endpoint", …) is checked
// only for OKF conformance — a non-empty `type`. Entries claiming a RunLore type
// are held to the full ValidateStructural shape, since those are the ones the
// merge gate promised were well-formed.
func WarnInvalid(entries []catalog.Entry, onInvalid func(path string, errs []Issue)) int {
	n := 0
	for _, e := range entries {
		var errs []Issue
		if validTypes[e.Type] {
			for _, i := range ValidateStructural(e) {
				if i.Severity == SeverityError {
					errs = append(errs, i)
				}
			}
		} else if strings.TrimSpace(e.Type) == "" {
			errs = append(errs, Issue{SeverityError, "type", "frontmatter `type` is required (OKF conformance)"})
		}
		if len(errs) > 0 {
			n++
			if onInvalid != nil {
				onInvalid(e.Path, errs)
			}
		}
	}
	return n
}

// HasErrors reports whether any issue is Severity=Error — the gate signal.
func HasErrors(issues []Issue) bool {
	for _, i := range issues {
		if i.Severity == SeverityError {
			return true
		}
	}
	return false
}

// ValidateStructural runs deterministic structural checks on a parsed catalog
// entry. Errors fail the merge gate; warnings are advisory.
func ValidateStructural(e catalog.Entry) []Issue {
	var out []Issue
	addErr := func(field, msg string) { out = append(out, Issue{SeverityError, field, msg}) }
	addWarn := func(field, msg string) { out = append(out, Issue{SeverityWarning, field, msg}) }

	switch {
	case strings.TrimSpace(e.Type) == "":
		addErr("type", "frontmatter `type` is required")
	case !validTypes[e.Type]:
		addErr("type", "type must be one of Incident, Playbook, Concept")
	}

	switch {
	case strings.TrimSpace(e.Title) == "":
		addErr("title", "frontmatter `title` is required")
	case strings.ContainsAny(e.Title, "\r\n"):
		addErr("title", "title must be a single line")
	case len(e.Title) > maxTitleLen:
		addErr("title", fmt.Sprintf("title must be at most %d bytes", maxTitleLen))
	}

	if strings.TrimSpace(e.Description) == "" {
		addErr("description", "frontmatter `description` is required")
	}

	// resource is required for Incident only: an incident is anchored to a concrete
	// affected object, while Playbook/Concept entries are abstract knowledge — OKF
	// leaves resource "omitted for abstract concepts", and curator.entryType drafts
	// a Playbook precisely when the finding is resource-agnostic.
	switch {
	case strings.TrimSpace(e.Resource) == "":
		if e.Type == "Incident" {
			addErr("resource", "frontmatter `resource` is required for Incident (namespace/name)")
		}
	case strings.ContainsAny(e.Resource, " \t\r\n"):
		addErr("resource", "resource must not contain whitespace")
	}

	if len(e.Tags) == 0 {
		addWarn("tags", "frontmatter `tags` is empty")
	}

	// Lifecycle fields are ADVISORY: an odd status or an unparseable date is a
	// warning, never an error — one strange entry must never fail the merge gate,
	// and recall's fail-safe already treats an unknown status as active and an
	// unparseable date as no-age-penalty.
	if s := strings.TrimSpace(e.Status); s != "" {
		switch s {
		case "active", "retired", "draft":
		default:
			addWarn("status", fmt.Sprintf("unknown status %q (known: active, retired, draft); treated as active", s))
		}
	}
	if e.LastValidated != "" {
		if _, ok := catalog.ParseEntryDate(e.LastValidated); !ok {
			addWarn("last_validated", fmt.Sprintf("unparseable date %q (want RFC3339 or 2006-01-02); age down-weighting will ignore it", e.LastValidated))
		}
	}

	if strings.TrimSpace(e.Body) == "" {
		addErr("body", "entry body is empty")
		return out
	}

	// Incident bodies must carry the OKF evidence sections; Playbook/Concept are
	// intentionally relaxed in v1 (free-form runbooks/concepts).
	if e.Type == "Incident" {
		secs := Sections(e.Body)
		for _, s := range requiredIncidentSections {
			content, ok := secs[s.key]
			switch {
			case !ok:
				addErr(s.key, "Incident body is missing the `## "+s.head+"` section")
			case content == "":
				addErr(s.key, "the `## "+s.head+"` section is empty")
			}
		}
		if _, ok := secs["investigate"]; !ok {
			addWarn("investigate", "Incident body has no `## Investigate` evidence section")
		}
	}

	return out
}

// Sections maps each "## Heading" (lowercased) to its trimmed content. Exported
// so `lore kb import` can infer Incident-vs-Playbook from a source document's
// OKF sections using the same heading parser the validator gates on.
//
// A heading inside a fenced code block is CODE, not a heading — the same answer
// catalog.Entry.Section gives, from the same scanner (catalog.FencedLines), so
// the merge gate and the read path cannot disagree about what a section is. See
// FencedLines for what the two cost while they did.
//
// Fence CONTENT is still part of the section, and that is where this parser and
// catalog.Entry.Section legitimately differ: that one builds a quotable prose
// excerpt for a chat notification and drops code, while this one only asks
// whether a section is present and non-empty — and a resolution's command block
// is part of the resolution. They must agree on headings; they need not agree on
// what is worth quoting.
func Sections(body string) map[string]string {
	out := map[string]string{}
	cur := ""
	var buf []string
	flush := func() {
		if cur != "" {
			out[cur] = strings.TrimSpace(strings.Join(buf, "\n"))
		}
		buf = nil
	}
	lines := strings.Split(body, "\n")
	fenced := catalog.FencedLines(lines)
	for i, line := range lines {
		if !fenced[i] {
			if label, ok := heading(line); ok {
				flush()
				cur = label
				continue
			}
		}
		if cur != "" {
			buf = append(buf, line)
		}
	}
	flush()
	return out
}

// heading returns the lowercased label of a "# X" or "## X" markdown heading
// line. Both levels are section headings here: OKF's conventional headings are H1
// (the seed entries follow that style) while curator-drafted entries use H2.
func heading(line string) (string, bool) {
	t := strings.TrimSpace(line)
	switch {
	case strings.HasPrefix(t, "## "):
		return strings.ToLower(strings.TrimSpace(t[3:])), true
	case strings.HasPrefix(t, "# "):
		return strings.ToLower(strings.TrimSpace(t[2:])), true
	}
	return "", false
}
