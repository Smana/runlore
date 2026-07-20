// SPDX-License-Identifier: Apache-2.0

package kbvalidate

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
)

// validIncident is the "#48 standard": a fully-formed Incident that must pass
// with zero issues (no errors, no warnings).
func validIncident() catalog.Entry {
	return catalog.Entry{
		Type:        "Incident",
		Title:       "KubeContainerOOMKilled for oom-app",
		Description: "the container is OOMKilled because its memory limit is too low",
		Resource:    "runlore-test/oom-app",
		Tags:        []string{"runlore", "incident"},
		Body: "## Symptom\n\nKubeContainerOOMKilled\n\n" +
			"## Investigate\n\n- pod_status: OOMKilled (exit 137)\n\n" +
			"## Cause\n\n1. **memory limit too low** (90%)\n\n" +
			"## Resolution\n\n- raise the memory limit\n",
	}
}

func has(issues []Issue, sev Severity, field string) bool {
	for _, i := range issues {
		if i.Severity == sev && i.Field == field {
			return true
		}
	}
	return false
}

func TestValidateStructural(t *testing.T) {
	tests := []struct {
		name      string
		mutate    func(*catalog.Entry)
		errField  string // expect a Severity=Error issue on this field ("" = none)
		warnField string // expect a Severity=Warning issue on this field ("" = none)
	}{
		{"valid incident", func(*catalog.Entry) {}, "", ""},
		{"missing type", func(e *catalog.Entry) { e.Type = "" }, "type", ""},
		{"invalid type", func(e *catalog.Entry) { e.Type = "Bogus" }, "type", ""},
		{"playbook type ok", func(e *catalog.Entry) { e.Type = "Playbook" }, "", ""},
		{"concept type ok", func(e *catalog.Entry) { e.Type = "Concept" }, "", ""},
		{"empty title", func(e *catalog.Entry) { e.Title = "" }, "title", ""},
		{"long title", func(e *catalog.Entry) { e.Title = strings.Repeat("x", 121) }, "title", ""},
		{"multiline title", func(e *catalog.Entry) { e.Title = "a\nb" }, "title", ""},
		{"empty description", func(e *catalog.Entry) { e.Description = "" }, "description", ""},
		{"empty resource", func(e *catalog.Entry) { e.Resource = "" }, "resource", ""},
		{"resource with whitespace", func(e *catalog.Entry) { e.Resource = "ns / name" }, "resource", ""},
		// resource is required for Incident only: a Playbook/Concept is abstract,
		// generalized knowledge — OKF says resource is "omitted for abstract concepts",
		// and entryType only drafts a Playbook when the finding is resource-agnostic.
		{"playbook empty resource ok", func(e *catalog.Entry) { e.Type = "Playbook"; e.Resource = "" }, "", ""},
		{"concept empty resource ok", func(e *catalog.Entry) { e.Type = "Concept"; e.Resource = "" }, "", ""},
		{"playbook resource with whitespace", func(e *catalog.Entry) { e.Type = "Playbook"; e.Resource = "ns / name" }, "resource", ""},
		{"empty tags warns", func(e *catalog.Entry) { e.Tags = nil }, "", "tags"},
		// body, type-aware
		{"incident missing cause", func(e *catalog.Entry) {
			e.Body = "## Symptom\n\nx\n\n## Resolution\n\n- y\n"
		}, "cause", ""},
		{"incident empty resolution", func(e *catalog.Entry) {
			e.Body = "## Symptom\n\nx\n\n## Cause\n\n1. y\n\n## Resolution\n"
		}, "resolution", ""},
		{"incident missing investigate warns", func(e *catalog.Entry) {
			e.Body = "## Symptom\n\nx\n\n## Cause\n\n1. y\n\n## Resolution\n\n- z\n"
		}, "", "investigate"},
		{"playbook relaxed sections", func(e *catalog.Entry) {
			e.Type = "Playbook"
			e.Body = "Some free-form runbook content with no required sections."
		}, "", ""},
		{"empty body", func(e *catalog.Entry) { e.Type = "Playbook"; e.Body = "  \n" }, "body", ""},
		// OKF's conventional section headings are H1 ("# Citations") and the seed
		// entries follow that style — a hand-written Incident using H1 sections must
		// validate, not fail with "missing section".
		{"incident with H1 sections", func(e *catalog.Entry) {
			e.Body = "# Symptom\n\nx\n\n# Investigate\n\n- e\n\n# Cause\n\n1. y\n\n# Resolution\n\n- z\n"
		}, "", ""},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			e := validIncident()
			tc.mutate(&e)
			issues := ValidateStructural(e)

			if tc.errField == "" {
				if HasErrors(issues) {
					t.Fatalf("expected no errors, got %+v", issues)
				}
			} else if !has(issues, SeverityError, tc.errField) {
				t.Fatalf("expected Error on %q, got %+v", tc.errField, issues)
			}

			if tc.warnField != "" && !has(issues, SeverityWarning, tc.warnField) {
				t.Fatalf("expected Warning on %q, got %+v", tc.warnField, issues)
			}
		})
	}
}

func TestWarnInvalid(t *testing.T) {
	good := validIncident()
	good.Path = "good.md"
	bad := validIncident()
	bad.Path = "bad.md"
	bad.Body = "## Symptom\n\nx\n" // missing ## Cause and ## Resolution

	var flagged []string
	n := WarnInvalid([]catalog.Entry{good, bad}, func(path string, errs []Issue) {
		if len(errs) == 0 {
			t.Fatalf("onInvalid called with no errors for %s", path)
		}
		flagged = append(flagged, path)
	})
	if n != 1 {
		t.Fatalf("want 1 invalid entry, got %d", n)
	}
	if len(flagged) != 1 || flagged[0] != "bad.md" {
		t.Fatalf("want bad.md flagged, got %v", flagged)
	}
}

// TestWarnInvalidToleratesForeignTypes: OKF conformance (§9) requires consumers
// to tolerate unknown types gracefully. A foreign OKF bundle entry (type
// "Metric", no resource, free-form body) is conformant knowledge — the load-time
// hook must serve it without flagging. Only RunLore-vocabulary entries are held
// to the structural merge-gate shape at load time, and OKF non-conformance
// (empty type) is still flagged.
func TestWarnInvalidToleratesForeignTypes(t *testing.T) {
	foreign := catalog.Entry{
		Path: "foreign.md", Type: "Metric", Title: "requests_total",
		Body: "A counter of HTTP requests.",
	}
	noType := catalog.Entry{Path: "no-type.md", Title: "t", Body: "b"}
	brokenIncident := validIncident()
	brokenIncident.Path = "broken.md"
	brokenIncident.Body = "## Symptom\n\nx\n" // missing Cause/Resolution

	var flagged []string
	n := WarnInvalid([]catalog.Entry{foreign, noType, brokenIncident}, func(path string, _ []Issue) {
		flagged = append(flagged, path)
	})
	if n != 2 {
		t.Fatalf("want 2 invalid (no-type + broken incident), got %d: %v", n, flagged)
	}
	for _, p := range flagged {
		if p == "foreign.md" {
			t.Fatal("a foreign-typed OKF entry must not be flagged at load time")
		}
	}
}

// TestValidateStatusAndLastValidated: the lifecycle fields are ADVISORY at the
// merge gate — an odd status or an unparseable date is a warning, never an error
// (one strange entry never fails the gate). Valid vocabulary and dates warn about
// nothing.
func TestValidateStatusAndLastValidated(t *testing.T) {
	// (a) unknown status → warning on `status`, and HasErrors stays false.
	e := validIncident()
	e.Status = "bogus"
	issues := ValidateStructural(e)
	if HasErrors(issues) {
		t.Fatalf("unknown status must not be an error: %+v", issues)
	}
	if !has(issues, SeverityWarning, "status") {
		t.Fatalf("expected a warning on status, got %+v", issues)
	}

	// (b) unparseable last_validated → warning on `last_validated`, never an error.
	e = validIncident()
	e.LastValidated = "not-a-date"
	issues = ValidateStructural(e)
	if HasErrors(issues) {
		t.Fatalf("bad last_validated must not be an error: %+v", issues)
	}
	if !has(issues, SeverityWarning, "last_validated") {
		t.Fatalf("expected a warning on last_validated, got %+v", issues)
	}

	// (c) known statuses (incl. empty) + a valid date → no lifecycle warning at all.
	for _, s := range []string{"", "active", "retired", "draft"} {
		e = validIncident()
		e.Status = s
		e.LastValidated = "2026-01-10"
		issues = ValidateStructural(e)
		if has(issues, SeverityWarning, "status") || has(issues, SeverityWarning, "last_validated") {
			t.Fatalf("status=%q with a valid date must not warn: %+v", s, issues)
		}
	}
	// An RFC3339 last_validated is equally valid.
	e = validIncident()
	e.LastValidated = "2026-01-10T09:30:00Z"
	if has(ValidateStructural(e), SeverityWarning, "last_validated") {
		t.Fatal("RFC3339 last_validated must not warn")
	}
}

func TestHasErrors(t *testing.T) {
	if HasErrors([]Issue{{Severity: SeverityWarning, Field: "tags"}}) {
		t.Fatal("warnings-only must not count as errors")
	}
	if !HasErrors([]Issue{{Severity: SeverityError, Field: "type"}}) {
		t.Fatal("an error must be reported")
	}
}
