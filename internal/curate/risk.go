// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"slices"
	"strings"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// possibleDuplicateHeading is the section the forge writes into a PR's body when the dedup decider
// flags the finding as a probable duplicate of an entry (suspectedDuplicateSection in each forge).
const possibleDuplicateHeading = "## Possible duplicate"

// securityLabel is applied by humans only: nothing in RunLore writes it.
const securityLabel = "security"

// Risk is why an open entry PR needs a human rather than the merge gate, riskiest first.
type Risk int

// The risks, in their order.
const (
	RiskContested   Risk = iota // a standing 👎 whose KB link is the PR
	RiskContradicts             // the PR carries the decider's possible-duplicate section
	RiskSecurity                // a human labelled it security
	RiskUnverified              // the PR's fingerprint has no held latest observation
)

// String names the risk.
func (r Risk) String() string {
	switch r {
	case RiskContested:
		return "contested"
	case RiskContradicts:
		return "contradicts"
	case RiskSecurity:
		return "security"
	case RiskUnverified:
		return "unverified"
	}
	return "unknown"
}

// Evidence is what the outcome ledger knows about the fixes behind open PRs.
type Evidence interface {
	// Verifications maps a dedup fingerprint to whether its latest observation held.
	Verifications() map[string]bool
	// ContestedTriggers lists the triggers with standing 👎 votes and a KB link.
	ContestedTriggers() []outcome.ContestedTrigger
}

// Risks lists every risk that applies to pr, riskiest first. contested is keyed by PR number and
// verified by dedup fingerprint. Empty means the PR's fix held and nothing stands against it.
func Risks(pr providers.CuratedIssue, contested map[int]bool, verified map[string]bool) []Risk {
	var risks []Risk
	if contested[pr.Number] {
		risks = append(risks, RiskContested)
	}
	if strings.Contains(pr.Body, possibleDuplicateHeading) {
		risks = append(risks, RiskContradicts)
	}
	if slices.Contains(pr.Labels, securityLabel) {
		risks = append(risks, RiskSecurity)
	}
	if !verified[providers.ParseFingerprintMarker(pr.Body)] {
		risks = append(risks, RiskUnverified)
	}
	return risks
}
