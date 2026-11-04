// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/threatcheck"
)

// securityLabel is applied by humans only: nothing in RunLore writes it.
const securityLabel = "security"

// Risk is why an open entry PR needs a human rather than the merge gate, riskiest first. A threat
// ranks first because Decide shows one risk a row, and only a threat's row withholds the PR's text.
type Risk int

// The risks, in their order.
const (
	RiskThreat      Risk = iota // the threat check hit on the PR's entry, or could not read it
	RiskContested               // a standing 👎 whose KB link is the PR
	RiskContradicts             // the PR carries the decider's possible-duplicate section
	RiskSecurity                // a human labelled it security
	RiskConflict                // it conflicts and RunLore refused to rebuild it; Risks does not report it (RefreshEntries does)
	RiskUnverified              // the PR's fingerprint has no held latest observation
)

// String names the risk.
func (r Risk) String() string {
	switch r {
	case RiskThreat:
		return "threat"
	case RiskContested:
		return "contested"
	case RiskContradicts:
		return "contradicts"
	case RiskSecurity:
		return "security"
	case RiskConflict:
		return "conflict"
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
// verified by dedup fingerprint; threats are the threat check's rules for pr's entry (entryThreats).
// Empty means the PR's fix held and nothing stands against it.
func Risks(pr providers.CuratedIssue, contested map[int]bool, verified map[string]bool, threats []string) []Risk {
	var risks []Risk
	if len(threats) > 0 {
		risks = append(risks, RiskThreat)
	}
	if contested[pr.Number] {
		risks = append(risks, RiskContested)
	}
	if strings.Contains(pr.Body, providers.PossibleDuplicateHeading) {
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

// ruleEntryUnreadable names a threat that is no rule of the check: the entry could not be screened.
const ruleEntryUnreadable = "entry-unreadable"

// entryThreats runs the threat check on the entry pr carries at its head, and returns the rules it
// broke. A human's accepted label is the override, so an accepted PR is not read. What is logged is
// the rules' names, never the text they matched.
//
// An entry that certainly cannot be screened (providers.ErrEntryUnscreenable) is a hit. Any other
// read failure says nothing about the entry, so it comes back as an error: the caller grants nothing
// on it, takes nothing away for it, and tries again. A missing label is terminal at Maku's merge gate
// (label_missing), so one 5xx must not hand off every labelled PR.
func entryThreats(ctx context.Context, f EntryReader, log *slog.Logger, pr providers.CuratedIssue) ([]string, error) {
	if slices.Contains(pr.Labels, acceptedLabel) {
		return nil, nil
	}
	text, err := f.PREntryText(ctx, pr.Number)
	switch {
	case errors.Is(err, providers.ErrEntryUnscreenable):
		log.Warn("threat check: the entry cannot be screened, so it is withheld", "pr", pr.Number, "rules", ruleEntryUnreadable, "err", err)
		return []string{ruleEntryUnreadable}, nil
	case err != nil:
		log.Warn("threat check: the entry could not be read; no label added, none removed, retried", "pr", pr.Number, "err", err)
		return nil, err
	}
	rules := threatcheck.Check(string(text))
	if len(rules) > 0 {
		log.Warn("threat check: the entry is withheld", "pr", pr.Number, "rules", strings.Join(rules, ","))
	}
	return rules, nil
}
