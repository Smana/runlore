// SPDX-License-Identifier: Apache-2.0

package github

import (
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// The kb-steward skill (plugins/kb-steward) restates facts about the PRs this
// package opens. The pins live here — not in internal/catalog's
// skillcontract_test.go — because they check unexported behavior (renderEntry's
// output, the label sets) that only this package can see.

const (
	stewardSkillPath     = "../../../plugins/kb-steward/skills/kb-steward/SKILL.md"
	stewardChecklistPath = "../../../plugins/kb-steward/skills/kb-steward/references/entry-quality-checklist.md"
)

func readStewardDoc(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path) //nolint:gosec // G304: fixed in-repo doc paths
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(raw)
}

// The checklist's triage-allowances block states whether RunLore drafts carry
// last_validated. Both possible claims are phrase-anchored on the field name so
// unrelated prose (the authoring block also says "last_validated set to today")
// cannot satisfy the pin.
var (
	stampedClaimRE = regexp.MustCompile("`last_validated` stamped at draft time")
	absentClaimRE  = regexp.MustCompile("`last_validated` absent")
)

// TestChecklistMatchesRenderEntryLastValidated pins the checklist's claim about
// last_validated on RunLore drafts to what renderEntry actually writes. The
// truth is derived from the rendered output, not restated here: if the forge
// stops (or starts) stamping the field, the required doc phrase flips and this
// test fails until the checklist follows.
func TestChecklistMatchesRenderEntryLastValidated(t *testing.T) {
	out := renderEntry(providers.KBEntry{Type: "Incident", Title: "T", Body: "## body"})
	stamped := strings.Contains(out, "last_validated:")

	doc := readStewardDoc(t, stewardChecklistPath)
	claimsStamped := stampedClaimRE.MatchString(doc)
	claimsAbsent := absentClaimRE.MatchString(doc)

	switch {
	case !claimsStamped && !claimsAbsent:
		t.Fatalf("checklist states no claim about last_validated on RunLore drafts; "+
			"its triage-allowances block must say one of %q or %q", stampedClaimRE, absentClaimRE)
	case claimsStamped && claimsAbsent:
		t.Fatal("checklist claims both that drafts stamp last_validated and that it is absent — fix the doc")
	case stamped && claimsAbsent:
		t.Error("renderEntry stamps last_validated on every draft, but the checklist tells triagers it is absent — update the triage-allowances block")
	case !stamped && claimsStamped:
		t.Error("renderEntry no longer stamps last_validated, but the checklist tells triagers it does — update the triage-allowances block")
	}
}

// TestLastValidatedClaimREs is the mutation test for the two matchers above:
// each regex must fire on its own claim and stay quiet on the other, and the
// authoring-block phrasing must satisfy neither.
func TestLastValidatedClaimREs(t *testing.T) {
	cases := []struct {
		name, text            string
		wantStamp, wantAbsent bool
	}{
		{"stamped claim", "- `last_validated` stamped at draft time: RunLore sets it", true, false},
		{"absent claim", "- `last_validated` absent: RunLore drafts don't set it", false, true},
		{"authoring block is neither", "- [ ] `last_validated` set to today", false, false},
		{"bare field mention is neither", "suggest bumping `last_validated` when refining", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := stampedClaimRE.MatchString(c.text); got != c.wantStamp {
				t.Errorf("stampedClaimRE.MatchString(%q) = %v, want %v", c.text, got, c.wantStamp)
			}
			if got := absentClaimRE.MatchString(c.text); got != c.wantAbsent {
				t.Errorf("absentClaimRE.MatchString(%q) = %v, want %v", c.text, got, c.wantAbsent)
			}
		})
	}
}

// TestSkillNamesRealForgeLabels pins the label names the triage flow keys on to
// the sets this package actually applies. The flow lists PRs with
// `--label runlore` and tells retirement PRs apart by `runlore-retire`; rename
// either label in code and the flow goes blind, so the rename must reach the
// skill in the same change.
func TestSkillNamesRealForgeLabels(t *testing.T) {
	applied := map[string]bool{}
	for _, l := range lifecycleLabels {
		applied[l] = true
	}
	for _, l := range retireLabels {
		applied[l] = true
	}
	for label, docPhrase := range map[string]string{
		"runlore":        "--label runlore",
		"runlore-retire": "`runlore-retire`",
	} {
		if !applied[label] {
			t.Errorf("the forge no longer applies label %q — update the skill's triage flow to match", label)
		}
		if doc := readStewardDoc(t, stewardSkillPath); !strings.Contains(doc, docPhrase) {
			t.Errorf("SKILL.md flow 3 must mention %q (the triage flow keys on label %q)", docPhrase, label)
		}
	}
}
