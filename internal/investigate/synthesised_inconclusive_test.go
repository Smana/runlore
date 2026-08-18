// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestSynthesisedInconclusivesAccountForThemselves pins the other side of the
// #471/2026-08-18 card contract: every terminal result RunLore synthesises itself
// is a LEGITIMATE inconclusive, and stays one. Each carries, in a channel the card
// renders, the ceiling or stop that ended the run — so the notification says what
// blocked the investigation instead of leaving a bare verdict badge.
//
// It asserts through providers.Investigation.UnaccountedInconclusive, the same
// predicate the notifier consults, so a synthesised path that stopped naming its
// blocker fails here rather than shipping an empty card.
func TestSynthesisedInconclusivesAccountForThemselves(t *testing.T) {
	req := Request{Title: "KubeNodeNotReady", Workload: providers.Workload{Kind: "Node", Name: "ip-10-20-24-144"}, Fingerprint: "fp-1"}
	cases := []struct {
		name string
		inv  providers.Investigation
		// blocker is the substring the delivered account must name, so the reader
		// learns WHICH ceiling or stop ended the run, not merely that one did.
		blocker string
	}{
		{"budget kill: the per-request ceiling", budgetKillResult(req, budgetReasonRequestTokens), "per-request token budget"},
		{"budget kill: the cumulative token ceiling", budgetKillResult(req, budgetReasonTotalTokens), "cumulative token budget"},
		{"budget kill: the cost ceiling", budgetKillResult(req, budgetReasonCost), "estimated cost ceiling"},
		{"the per-investigation deadline", timeoutResult(req), "per-investigation deadline exceeded"},
		{"a provider refusal", refusalResult(req), "the model declined to respond"},
		{"the step budget ran out", nonConvergenceResult(req, "investigation exhausted its 12-step budget without concluding"), "12-step budget"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.inv.Verdict != providers.VerdictInconclusive {
				t.Fatalf("a synthesised stop must be inconclusive, got %q", c.inv.Verdict)
			}
			if c.inv.UnaccountedInconclusive() {
				t.Fatalf("synthesised inconclusive gives no account of what stopped it: %+v", c.inv)
			}
			account := strings.Join(append(append([]string{}, c.inv.Unresolved...), c.inv.DataGaps...), "\n")
			if !strings.Contains(account, c.blocker) {
				t.Fatalf("the delivered account does not name the blocker %q:\n%s", c.blocker, account)
			}
		})
	}
}
