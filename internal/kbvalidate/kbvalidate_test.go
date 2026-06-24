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

func TestHasErrors(t *testing.T) {
	if HasErrors([]Issue{{Severity: SeverityWarning, Field: "tags"}}) {
		t.Fatal("warnings-only must not count as errors")
	}
	if !HasErrors([]Issue{{Severity: SeverityError, Field: "type"}}) {
		t.Fatal("an error must be reported")
	}
}
