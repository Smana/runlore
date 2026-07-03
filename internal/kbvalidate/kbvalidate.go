// Package kbvalidate provides deterministic structural validation of OKF
// knowledge-base entries (the merge gate) and an LLM-assisted semantic advisory.
// The structural checks mirror what curator.draftKBEntry emits and what
// catalog.parseEntry consumes, so a RunLore-authored entry that passes meetsBar
// also passes ValidateStructural by construction.
package kbvalidate

import (
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

// requiredIncidentSections are the OKF body sections an Incident must carry
// (present and non-empty); curator.draftKBEntry always renders them.
var requiredIncidentSections = []struct{ key, head string }{
	{"symptom", "Symptom"},
	{"cause", "Cause"},
	{"resolution", "Resolution"},
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
	case len(e.Title) > 120:
		addErr("title", "title must be at most 120 characters")
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

	if strings.TrimSpace(e.Body) == "" {
		addErr("body", "entry body is empty")
		return out
	}

	// Incident bodies must carry the OKF evidence sections; Playbook/Concept are
	// intentionally relaxed in v1 (free-form runbooks/concepts).
	if e.Type == "Incident" {
		secs := sections(e.Body)
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

// sections maps each "## Heading" (lowercased) to its trimmed content.
func sections(body string) map[string]string {
	out := map[string]string{}
	cur := ""
	var buf []string
	flush := func() {
		if cur != "" {
			out[cur] = strings.TrimSpace(strings.Join(buf, "\n"))
		}
		buf = nil
	}
	for _, line := range strings.Split(body, "\n") {
		if label, ok := heading(line); ok {
			flush()
			cur = label
			continue
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
