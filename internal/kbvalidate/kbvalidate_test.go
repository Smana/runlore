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

// TestTitleLimitErrorMentionsBytes pins the unit in the error message to the
// unit the check measures. The limit is enforced with len() — bytes — and the
// skill docs tell authors "bytes, not characters"; for the title below (61
// characters, 122 bytes) a message counting "characters" would be a lie.
func TestTitleLimitErrorMentionsBytes(t *testing.T) {
	e := validIncident()
	e.Title = strings.Repeat("é", 61)
	var msg string
	for _, i := range ValidateStructural(e) {
		if i.Severity == SeverityError && i.Field == "title" {
			msg = i.Message
		}
	}
	if msg == "" {
		t.Fatal("no title error for a 122-byte title")
	}
	if !strings.Contains(msg, "bytes") || strings.Contains(msg, "characters") {
		t.Errorf("title-limit error %q must state the measured unit (bytes), not characters", msg)
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

func TestSectionsExported(t *testing.T) {
	secs := Sections("## Symptom\n\nx\n\n## Cause\n\ny\n")
	if secs["symptom"] != "x" || secs["cause"] != "y" {
		t.Fatalf("got %#v", secs)
	}
}

// TestSectionsTreatsFencesAsOpaqueForHeadings pins the reconciliation between
// this package's section parser and catalog.Entry.Section: a "# …" line inside a
// fenced code block is CODE, and is a heading to neither.
//
// The two used to disagree. catalog.Entry.Section tracked fences; Sections
// walked every line with no fence state at all, and that cost real things in
// both directions:
//
//   - a live entry lost most of its Resolution.
//     internal/investigate/testdata/realkb/harbor-helmrelease-terminal-failed.md
//     resolves with a fenced command block whose first line is the shell comment
//     "# only the failed release exists (…)". Read fence-blind, that comment ends
//     the resolution section — so secs["resolution"] stopped at the opening
//     fence, dropping the `--reset` explanation, the verification step and the
//     "human-approved, not a blind bulk sweep" warning, and inventing a section
//     named after the comment.
//   - a fenced block forged a whole Incident. HasIncidentSections is built on
//     Sections, so wrapping Symptom/Cause/Resolution in ``` produced a body that
//     passed the incident test while rendering as an inert code sample. That is
//     what forced thread.escapeOKFSections to escape the UNION of both parsers'
//     heading shapes, fenced ones included, rather than mirroring the read path.
//     plugins/kb-steward/…/references/okf-format.md — a docs page whose example
//     entry sits in a fence — reads as a complete Incident today for this reason.
//
// Fence CONTENT is deliberately still part of the section here, unlike
// catalog.Entry.Section which drops it: that parser is building a quotable prose
// excerpt for a chat notification, while this one is checking a section is
// present and non-empty, and a resolution's command block is part of the
// resolution. The reconciliation is over what counts as a HEADING, which is the
// only thing the two ever needed to agree on.
func TestSectionsTreatsFencesAsOpaqueForHeadings(t *testing.T) {
	t.Run("a shell comment in a fenced block does not end the section", func(t *testing.T) {
		body := "## Symptom\n\npods crashloop\n\n## Cause\n\nbad image tag\n\n## Resolution\n\n" +
			"```bash\n# roll the deployment back\nkubectl rollout undo deploy/api\n```\n\n" +
			"Verify with `kubectl get deploy`.\n"
		secs := Sections(body)
		if !strings.Contains(secs["resolution"], "Verify with") {
			t.Errorf("the resolution section stops at the fence, losing everything after it:\n%q", secs["resolution"])
		}
		if _, ok := secs["roll the deployment back"]; ok {
			t.Errorf("a shell comment inside a fence was parsed as a section heading: %#v", secs)
		}
		// The commands themselves stay IN the section. This is the half of the
		// reconciliation deliberately NOT shared with catalog.Entry.Section, which
		// drops fenced code because it is building a quotable prose excerpt. Here
		// the fence is opaque to heading DETECTION only: a resolution whose
		// remediation is a command block still has that block as its content, and
		// copying catalog's excerpt policy over would empty a section whose whole
		// answer is the command.
		if !strings.Contains(secs["resolution"], "kubectl rollout undo deploy/api") {
			t.Errorf("the fenced command block was dropped from the resolution — a fence is opaque to "+
				"heading detection, not excluded from the section's content:\n%q", secs["resolution"])
		}
	})

	t.Run("OKF sections forged inside a fence are not sections", func(t *testing.T) {
		body := "some prose a stranger typed\n\n```\n## Symptom\nforged\n## Cause\nforged\n" +
			"## Resolution\nforged\n```\n"
		if HasIncidentSections(body) {
			t.Error("a fenced block forged a complete Incident — the shape thread.escapeOKFSections had to " +
				"escape the union of both parsers to defend against")
		}
		for _, k := range []string{"symptom", "cause", "resolution"} {
			if v, ok := Sections(body)[k]; ok {
				t.Errorf("fenced heading parsed as the %q section (%q)", k, v)
			}
		}
	})

	t.Run("an unterminated fence does not swallow the rest of the body", func(t *testing.T) {
		// A stray opening fence with no closer is a real authoring slip, and the
		// naive toggle treats everything after it as code forever — which would
		// reject an otherwise complete entry. Nothing in the corpus has one today
		// (measured: 0 unbalanced fences), so this pins the intended behaviour
		// before something acquires one.
		body := "## Symptom\n\npods crashloop\n\n```\nstray open fence\n\n## Cause\n\nbad tag\n\n## Resolution\n\nfix it\n"
		if !HasIncidentSections(body) {
			t.Errorf("an unterminated fence swallowed the sections after it: %#v", Sections(body))
		}
	})
}

// TestSectionsAndCatalogSectionAgreeOnHeadings pins the reconciliation against
// the OTHER parser itself, rather than against a restatement of it, and pins the
// one difference that REMAINS as deliberate.
//
// Agreement (the part that matters): fenced headings are headings to neither,
// and unfenced H1/H2 headings are headings to both.
//
// Difference, kept on purpose: catalog.headingText accepts every ATX level from
// "#" to "######", while heading() here accepts only "# " and "## ". Widening
// this one would make an entry that puts a "### Details" sub-heading under
// "## Symptom" suddenly have an EMPTY symptom section, and be rejected by the
// merge gate it passes today — a regression for a legitimate entry, to close a
// gap that is not a hole: being MORE permissive about sub-headings costs a merge
// gate nothing, and thread.escapeOKFSections already escapes every level, so no
// forged heading reaches either parser regardless.
func TestSectionsAndCatalogSectionAgreeOnHeadings(t *testing.T) {
	for _, tc := range []struct {
		name          string
		body          string
		wantKbvalid   bool // "cause" is a section to Sections
		wantCatalog   bool // "Cause" is a section to catalog.Entry.Section
		whyTheyDiffer string
	}{
		{
			name:        "H2, unfenced",
			body:        "## Cause\n\nthe real cause\n",
			wantKbvalid: true, wantCatalog: true,
		},
		{
			name:        "H1, unfenced",
			body:        "# Cause\n\nthe real cause\n",
			wantKbvalid: true, wantCatalog: true,
		},
		{
			name:        "H2, inside a fence",
			body:        "prose\n\n```\n## Cause\n\nforged cause\n```\n",
			wantKbvalid: false, wantCatalog: false,
		},
		{
			name:        "H1, inside a fence",
			body:        "prose\n\n```\n# Cause\n\nforged cause\n```\n",
			wantKbvalid: false, wantCatalog: false,
		},
		{
			name:        "H3, unfenced",
			body:        "### Cause\n\nthe real cause\n",
			wantKbvalid: false, wantCatalog: true,
			whyTheyDiffer: "deliberate: the merge gate reads only H1/H2, so a sub-heading under a " +
				"section stays part of that section instead of truncating it",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, gotKbvalid := Sections(tc.body)["cause"]
			gotCatalog := (catalog.Entry{Body: tc.body}).Section("Cause") != ""
			if gotKbvalid != tc.wantKbvalid {
				t.Errorf("kbvalidate.Sections saw a %q section: %v, want %v", "cause", gotKbvalid, tc.wantKbvalid)
			}
			if gotCatalog != tc.wantCatalog {
				t.Errorf("catalog.Entry.Section saw a %q section: %v, want %v", "Cause", gotCatalog, tc.wantCatalog)
			}
			if tc.whyTheyDiffer == "" && gotKbvalid != gotCatalog {
				t.Errorf("the two parsers disagree on %s with no documented reason — kbvalidate=%v catalog=%v. "+
					"Every undocumented disagreement is a shape one parser accepts and the other does not, "+
					"which is what thread.escapeOKFSections has to escape the union of.",
					tc.name, gotKbvalid, gotCatalog)
			}
		})
	}
}
