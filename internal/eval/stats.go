// SPDX-License-Identifier: Apache-2.0

package eval

import "sort"

// Shared eval gating constants and statistics helpers, used by both the replay
// runner (eval.go) and the live runner (live.go).
const (
	evalRootCauseBar         = 2   // a run "reaches the root cause" at score >= 2 (live judge)
	evalMinPassRate          = 0.7 // fraction of runs that must pass (k-of-n); also the replay flaky band edge
	evalMaxRootCauseVariance = 0.5 // above this a live scenario is flaky → not a pass
)

// medianFloat returns the median of xs (0 for empty).
func medianFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	cp := append([]float64(nil), xs...)
	sort.Float64s(cp)
	m := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[m]
	}
	return (cp[m-1] + cp[m]) / 2
}

// variance returns the population variance of xs (0 for empty).
func variance(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	var sum float64
	for _, x := range xs {
		sum += x
	}
	mean := sum / float64(len(xs))
	var v float64
	for _, x := range xs {
		v += (x - mean) * (x - mean)
	}
	return v / float64(len(xs))
}
