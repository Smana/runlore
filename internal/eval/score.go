// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"strings"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
	"github.com/Smana/runlore/internal/thread"
)

// Result is the score for one case.
type Result struct {
	Name        string
	Pass        bool
	Confidence  float64
	Missing     []string // expected keywords/entities not found (or an error note); includes "over-claimed: <e>" markers
	OverClaimed []string // distractor entities the investigation wrongly blamed (over-claim/FP)

	// Claim is WHAT this run blamed: the claim text Score matched over, flattened and
	// secret-redacted. Missing names the absent term and never the answer given, which is
	// what made six weeks of red nightlies unreadable — the canonical statement of that,
	// referenced by the fold and the report rather than repeated there.
	//
	// Set only on a FAILING run: a right answer is not a finding, and nothing should carry
	// model text it will never publish. NOT capped here — callers dedup on the full text
	// and cap at store time (capClaim).
	Claim string

	// Recall telemetry (populated only for cases with a catalog fixture): whether
	// instant recall fired, and whether its answer short-circuited the loop. Surfaced
	// so recall behaviour is asserted mechanically, not inferred from the finding.
	RecallFired        bool
	RecallShortCircuit bool

	// ShadowRan / ShadowAgreed mirror RecallDecision's, so the replay corpus measures
	// shadow-mode agreement per case rather than only in a process-global metric.
	ShadowRan    bool
	ShadowAgreed bool

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
	blamed := claimText(inv)
	claim := strings.ToLower(blamed)
	missing := notInClaim(claim, exp.MustContain)

	var overClaimed []string
	if len(exp.RootCauseEntities) > 0 {
		missing = append(missing, notInClaim(claim, exp.RootCauseEntities)...)
		for _, d := range exp.Distractors {
			// coveredByEntity suppresses the penalty when the distractor is merely a
			// substring of a legitimately-named entity: the hit is explained by a
			// correct claim, not an over-claim.
			if !strings.Contains(claim, strings.ToLower(d)) || coveredByEntity(exp.RootCauseEntities, d) {
				continue
			}
			overClaimed = append(overClaimed, d)
			missing = append(missing, "over-claimed: "+d)
		}
	}

	res := Result{
		Name:        name,
		Pass:        len(missing) == 0 && inv.Confidence >= exp.MinConfidence,
		Confidence:  inv.Confidence,
		Missing:     missing,
		OverClaimed: overClaimed,
	}
	if !res.Pass {
		res.Claim = reportableClaim(blamed)
	}
	return res
}

// notInClaim returns the terms absent from claim, which must already be lower-cased.
// must_contain and root_cause_entities are matched identically — the same haystack,
// the same case-insensitive substring rule — so the rule is stated once here.
func notInClaim(claim string, terms []string) []string {
	var missing []string
	for _, t := range terms {
		if !strings.Contains(claim, strings.ToLower(t)) {
			missing = append(missing, t)
		}
	}
	return missing
}

// coveredByEntity reports whether a distractor hit is already explained by a correctly
// named entity — the distractor is merely a substring of one (e.g. distractor
// "apps/worker" inside the required "apps/worker-db"). Score suppresses the over-claim
// penalty in that case, so a distractor covered this way can never fire.
func coveredByEntity(entities []string, distractor string) bool {
	d := strings.ToLower(distractor)
	for _, e := range entities {
		if strings.Contains(strings.ToLower(e), d) {
			return true
		}
	}
	return false
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

// reportableClaim renders a claim for the log and the report: secret-redacted and
// flattened to one line.
//
// Flattening is a forgery guard. The claim is untrusted model text printed at the left
// margin of a one-line-per-case table, so a break inside it emits a line a reader cannot
// tell from the harness's own verdict lines. thread.SingleLine owns the break list, and
// its doc records why that list lives in exactly one place; Fields collapses the runs it
// leaves behind. A markdown sink would still owe cellEscaper its pipe escaping.
func reportableClaim(s string) string {
	return strings.Join(strings.Fields(thread.SingleLine(redact.Secrets(s))), " ")
}
