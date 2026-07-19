// SPDX-License-Identifier: Apache-2.0

package outcome

import "testing"

func TestAggregateFactor(t *testing.T) {
	cases := []struct {
		name string
		agg  Aggregate
		k    float64
		want float64
	}{
		{"no history is the prior mean", Aggregate{}, 2.0, 0.5},
		{"single unresolved recall decays fast", Aggregate{Recalls: 1}, 2.0, 1.0 / 3.0},
		{"single downvote decays identically", Aggregate{FeedbackDown: 1}, 2.0, 1.0 / 3.0},
		{"resolving entry climbs", Aggregate{Recalls: 4, Resolved: 4}, 2.0, 5.0 / 6.0},
		{"upvotes count as successes", Aggregate{FeedbackUp: 2}, 2.0, 3.0 / 4.0},
	}
	for _, c := range cases {
		if got := c.agg.Factor(c.k); got != c.want {
			t.Errorf("%s: Factor(%v) = %v, want %v", c.name, c.k, got, c.want)
		}
	}
}
