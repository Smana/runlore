// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestActionWithoutRemedy pins the other half of the shape contract: a verdict that
// tells the on-call to act must ship something to act on.
//
// Live card, 2026-08-24 02:18: header "🛠 Action suggested", body ending on
// "…1 more hypothesis below", and no next-steps section at all — because
// suggested_action and actions are both optional in the schema, so the payload was
// legal. A card that promises an action and has none costs more trust than a card
// that says inconclusive.
func TestActionWithoutRemedy(t *testing.T) {
	cause := func(action string) []providers.Hypothesis {
		return []providers.Hypothesis{{Summary: "s", SuggestedAction: action}}
	}
	cases := []struct {
		name string
		inv  providers.Investigation
		want bool
	}{
		{"action_suggested with no remedy anywhere",
			providers.Investigation{Verdict: providers.VerdictActionSuggested, RootCauses: cause("")}, true},
		{"action_required with no remedy anywhere",
			providers.Investigation{Verdict: providers.VerdictActionRequired, RootCauses: cause("")}, true},
		{"a suggested_action on the root cause accounts for it",
			providers.Investigation{Verdict: providers.VerdictActionSuggested, RootCauses: cause("delete the stale Job")}, false},
		{"a proposed action accounts for it",
			providers.Investigation{Verdict: providers.VerdictActionRequired, RootCauses: cause(""),
				Actions: []providers.Action{{Description: "restore Drive access"}}}, false},
		{"no_action is not claiming an action",
			providers.Investigation{Verdict: providers.VerdictNoAction, RootCauses: cause("")}, false},
		{"inconclusive is the other predicate's business",
			providers.Investigation{Verdict: providers.VerdictInconclusive, RootCauses: cause("")}, false},
		{"an omitted verdict is a parse concern, not this one",
			providers.Investigation{RootCauses: cause("")}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.inv.ActionWithoutRemedy(); got != c.want {
				t.Fatalf("ActionWithoutRemedy = %v, want %v", got, c.want)
			}
		})
	}
}

// TestUnevidencedConclusion: a conclusive verdict whose leading root cause cites no
// evidence at all. Live card, 2026-08-22 22:53 — "High confidence · 85%" with a Why
// paragraph and not one bullet under it. The prose may still be right, but nothing
// in the card lets a human check it, and nothing lets the verify pass trace it.
func TestUnevidencedConclusion(t *testing.T) {
	cases := []struct {
		name string
		inv  providers.Investigation
		want bool
	}{
		{"conclusive, leading cause cites nothing",
			providers.Investigation{Verdict: providers.VerdictActionSuggested,
				RootCauses: []providers.Hypothesis{{Summary: "stale Job never deleted"}}}, true},
		{"evidence on the leading cause accounts for it",
			providers.Investigation{Verdict: providers.VerdictActionSuggested,
				RootCauses: []providers.Hypothesis{{Summary: "s", Evidence: []string{"kube_job_failed=1"}}}}, false},
		{"no_action still has to show its work",
			providers.Investigation{Verdict: providers.VerdictNoAction,
				RootCauses: []providers.Hypothesis{{Summary: "self-healed"}}}, true},
		{"inconclusive is allowed to have nothing",
			providers.Investigation{Verdict: providers.VerdictInconclusive,
				RootCauses: []providers.Hypothesis{{Summary: "s"}}}, false},
		{"no root cause at all is the inconclusive predicate's business",
			providers.Investigation{Verdict: providers.VerdictActionSuggested}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := unevidencedConclusion(c.inv); got != c.want {
				t.Fatalf("unevidencedConclusion = %v, want %v", got, c.want)
			}
		})
	}
}

// TestSubmitFindingsRequiresEvidence guards the model-facing half of this fix, which
// has no compiler. The predicates above test Go functions; the schema is a JSON
// string literal, so a future edit that reflows or re-wraps that line can silently
// revert `evidence` to optional while every other test in this file keeps passing.
// Same reasoning as TestDataGapsForbidsSpeculation.
func TestSubmitFindingsRequiresEvidence(t *testing.T) {
	var schema struct {
		Properties struct {
			RootCauses struct {
				Items struct {
					Properties map[string]struct {
						MinItems    int    `json:"minItems"`
						Description string `json:"description"`
					} `json:"properties"`
					Required []string `json:"required"`
				} `json:"items"`
			} `json:"root_causes"`
			Verdict struct {
				Description string `json:"description"`
			} `json:"verdict"`
		} `json:"properties"`
	}
	if err := json.Unmarshal([]byte(submitFindingsSpec().Schema), &schema); err != nil {
		t.Fatalf("submit_findings schema is not valid JSON: %v", err)
	}
	rc := schema.Properties.RootCauses.Items

	if !slices.Contains(rc.Required, "evidence") {
		t.Errorf("a root cause must REQUIRE evidence; required = %v", rc.Required)
	}
	if got := rc.Properties["evidence"].MinItems; got != 1 {
		t.Errorf("evidence minItems = %d, want 1 — an empty array satisfies a bare `required`", got)
	}
	// The conditional requirement that pairs a remedy with a verdict claiming one.
	// ActionWithoutRemedy only WARNS, so this sentence is the only thing that stops
	// the payload being produced in the first place.
	if d := rc.Properties["suggested_action"].Description; !strings.Contains(d, "action_required") {
		t.Errorf("suggested_action must say when it is required:\n%s", d)
	}
	// The converse of the inconclusive rule: high confidence in "I could not tell"
	// is still inconclusive. Live 2026-08-24, an 85%% action_suggested whose finding
	// was that it could not identify the failing resource.
	if d := schema.Properties.Verdict.Description; !strings.Contains(d, "could not name the failing resource") {
		t.Errorf("verdict must state the converse of the recurrence rule:\n%s", d)
	}
}
