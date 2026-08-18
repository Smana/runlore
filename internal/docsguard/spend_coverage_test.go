// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"strings"
	"testing"
)

// The "What these ceilings do *not* cover" table is the one place an operator learns
// the SCOPE of a spend ceiling, and scope claims rot in the worst direction: the page
// keeps reading as true while the code underneath it changes what is covered. It had
// already happened twice by the time these guards were written — the page still said
// `lore eval` "builds its investigators with no ceilings and no pricing at all" long
// after app.evalSpend started forwarding the operator's configured ones, and it said
// the embeddings endpoint's tokens "are not in the investigation's totals" while that
// was being made false.
//
// Anchors are the SENTENCES that carry the claim, not keywords: a table row can be
// rewritten into its own opposite without losing any single word in it.
var spendCoverageClaims = []string{
	// The half of the embeddings gap an investigation-scoped ceiling CAN close.
	"The hybrid-recall query embed's *tokens* now count against `max_tokens_per_investigation`",
	// …and the half it deliberately cannot, said plainly rather than implied.
	"there is no `model.embeddings.pricing`, so RunLore has no rate to price them with and refuses " +
		"to borrow a completion model's",
	// The bulk reload is outside every ceiling, and the page must say WHY rather than
	// leaving it looking like an oversight.
	"no investigation to charge, so no investigation-scoped ceiling can reach it, and RunLore ships " +
		"none of its own",
	// A campaign's real bound, replacing the stale "no ceilings and no pricing at all".
	"`--max-total-tokens` is the run-level ceiling; **unset, a campaign has none**",
}

// TestSpendCoverageTableStatesWhatIsAndIsNotBounded pins those four claims.
func TestSpendCoverageTableStatesWhatIsAndIsNotBounded(t *testing.T) {
	doc := flattenProse(readDoc(t, configurationPath))
	for _, want := range spendCoverageClaims {
		if !strings.Contains(doc, flattenProse(want)) {
			t.Errorf("configuration.md must state %q — the spend-coverage table is where an operator "+
				"learns the SCOPE of a ceiling, and a scope claim that has quietly become false is "+
				"worse than no claim at all", want)
		}
	}
}

// TestSpendCoverageStaleClaimsAreGone is the other direction: the superseded wording
// must not survive anywhere on the page. A table rewritten by adding rows while the
// old row stays put would satisfy every anchor above and still tell an operator that
// embedding tokens are unbounded and that eval runs with no ceilings.
func TestSpendCoverageStaleClaimsAreGone(t *testing.T) {
	doc := flattenProse(readDoc(t, configurationPath))
	for _, stale := range []string{
		"Its tokens are not in the investigation's totals and it is not gated by either ceiling",
		"Builds its investigators with no ceilings and no pricing at all",
	} {
		if strings.Contains(doc, flattenProse(stale)) {
			t.Errorf("configuration.md still carries the superseded claim %q — it was true when it "+
				"was written and is not now", stale)
		}
	}
}

// TestSpendCoverageAnchorsRejectTheWeakerWording is the mutation test for the guard
// above, in this package's convention: each anchor must FAIL to match prose that
// sounds like it makes the same claim but does not. Without this, an anchor loose
// enough to match any spend paragraph would pin nothing at all — which is how a guard
// passes forever while the thing it guards drifts.
func TestSpendCoverageAnchorsRejectTheWeakerWording(t *testing.T) {
	for _, weaker := range []string{
		// Visible is not bounded — the exact distinction these fixes turn on.
		"the /embeddings endpoint is fully instrumented: its calls, latency and tokens are all " +
			"visible under provider=\"embed\", so its spend is accounted for",
		// A cost ceiling that claims to cover embeddings by borrowing a rate.
		"embedding tokens are priced at the main model's rate so max_cost_per_investigation " +
			"covers them too",
		// The bulk reload described as bounded by the per-investigation ceiling.
		"the bulk corpus embed runs inside the investigation that triggered the reload, so " +
			"max_tokens_per_investigation bounds it",
		// A campaign described as bounded by the per-case ceilings.
		"lore eval runs every case under the configured investigation ceilings, so a campaign " +
			"is bounded",
	} {
		flat := flattenProse(weaker)
		for _, claim := range spendCoverageClaims {
			if strings.Contains(flat, flattenProse(claim)) {
				t.Errorf("anchor %q matches the weaker wording %q — it pins nothing", claim, weaker)
			}
		}
	}
}
