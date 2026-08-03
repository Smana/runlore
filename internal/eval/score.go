// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// Result is the score for one case.
type Result struct {
	Name        string
	Pass        bool
	Confidence  float64
	Missing     []string // expected keywords/entities not found (or an error note); includes "over-claimed: <e>" markers
	OverClaimed []string // distractor entities the investigation wrongly blamed (over-claim/FP)

	// Recall telemetry (populated only for cases with a catalog fixture): whether
	// instant recall fired, and whether its answer short-circuited the loop. Surfaced
	// so recall behaviour is asserted mechanically, not inferred from the finding.
	RecallFired        bool
	RecallShortCircuit bool

	// Usage is the provider-reported token spend of THIS run, attributed by the
	// runner differencing its cumulative counter. Zero means the provider reported
	// nothing — treated downstream as unknown, never as free.
	Usage providers.Usage
}

// Score reports whether the investigation identifies the expected root cause.
// Everything is matched over the CLAIM text (what was blamed — see claimText):
// keyword recall (must_contain), entity recall (root_cause_entities) and the
// over-claim penalty (distractors). Matching must_contain over the full findings
// text would make the gate self-fulfilling in a REPLAY eval — the tool output is
// evidence the harness itself handed the model, so an agent that merely quotes it
// back, or that names the keyword only while ruling it out, would score a pass.
// Entity scoring engages only when root_cause_entities is set. A case passes when
// nothing is missing, no distractor was blamed, and confidence meets the floor.
func Score(name string, inv providers.Investigation, exp Expected) Result {
	claim := strings.ToLower(claimText(inv))
	var missing []string
	for _, kw := range exp.MustContain {
		if !strings.Contains(claim, strings.ToLower(kw)) {
			missing = append(missing, kw)
		}
	}

	var overClaimed []string
	if len(exp.RootCauseEntities) > 0 {
		for _, e := range exp.RootCauseEntities {
			if !strings.Contains(claim, strings.ToLower(e)) {
				missing = append(missing, e)
			}
		}
		for _, d := range exp.Distractors {
			dl := strings.ToLower(d)
			if !strings.Contains(claim, dl) {
				continue
			}
			// Suppress the penalty when the distractor is merely a substring of a
			// legitimately-named entity (e.g. distractor "apps/worker" inside the
			// required "apps/worker-db"): the hit is explained by a correct claim,
			// not an over-claim.
			covered := false
			for _, e := range exp.RootCauseEntities {
				if strings.Contains(strings.ToLower(e), dl) {
					covered = true
					break
				}
			}
			if !covered {
				overClaimed = append(overClaimed, d)
				missing = append(missing, "over-claimed: "+d)
			}
		}
	}

	return Result{
		Name:        name,
		Pass:        len(missing) == 0 && inv.Confidence >= exp.MinConfidence,
		Confidence:  inv.Confidence,
		Missing:     missing,
		OverClaimed: overClaimed,
	}
}

// claimText is what the investigation BLAMED: the title plus each hypothesis's
// summary and suggested action. It deliberately excludes Evidence and Unresolved
// so matching means "named as a cause", not "mentioned while ruling out".
func claimText(inv providers.Investigation) string {
	var b strings.Builder
	b.WriteString(inv.Title)
	for _, rc := range inv.RootCauses {
		b.WriteString(" " + rc.Summary + " " + rc.SuggestedAction)
	}
	return b.String()
}

// investigationText flattens the findings into one searchable string. Used to show
// the LLM judge the WHOLE answer (claim plus the evidence and caveats behind it);
// deterministic scoring uses claimText instead — see Score.
func investigationText(inv providers.Investigation) string {
	var b strings.Builder
	b.WriteString(inv.Title)
	for _, rc := range inv.RootCauses {
		b.WriteString(" " + rc.Summary + " " + rc.SuggestedAction + " " + strings.Join(rc.Evidence, " "))
	}
	b.WriteString(" " + strings.Join(inv.Unresolved, " "))
	b.WriteString(" " + strings.Join(inv.RuledOut, " "))
	b.WriteString(" " + strings.Join(inv.DataGaps, " "))
	return b.String()
}
