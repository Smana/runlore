// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"testing"

	"github.com/Smana/runlore/internal/outcome"
)

func TestOutcomeGateCountsVerifiedFixes(t *testing.T) {
	for _, c := range []struct {
		name   string
		agg    outcome.Aggregate
		want   float64
		wantOK bool
	}{
		{"verified is evidence on a dead resolve channel", outcome.Aggregate{Verified: 1}, 2.0 / 3, true},
		{"failed fixes sink it below the floor", outcome.Aggregate{Failed: 3}, 0.2, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			r := soloRecall(fakeOutcome{counts: map[string]outcome.Aggregate{"x.md": c.agg}, deadResolveChannel: true})
			got, ok := r.outcomeGate(map[string]outcome.Aggregate{"x.md": c.agg}, "x.md")
			if ok != c.wantOK || got < c.want-1e-9 || got > c.want+1e-9 {
				t.Fatalf("outcomeGate = (%v, %v), want (%v, %v)", got, ok, c.want, c.wantOK)
			}
		})
	}
}
