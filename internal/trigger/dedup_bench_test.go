// SPDX-License-Identifier: Apache-2.0

package trigger

import (
	"fmt"
	"testing"
	"time"
)

// BenchmarkDeduperSeenStorm guards the ingest chokepoint under an alert storm
// with many DISTINCT fingerprints: Seen currently evicts by scanning the whole
// map under the mutex on every call (audit perf finding — demoted, but pinned
// here so a regression, or the eventual amortized-sweep fix, is visible).
// Run: go test ./internal/trigger/ -bench . -benchtime 5x -run '^$'
func BenchmarkDeduperSeenStorm(b *testing.B) {
	d := NewDeduper(10 * time.Minute)
	keys := make([]string, 5000)
	for i := range keys {
		keys[i] = fmt.Sprintf("fp-%d", i)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		d.Seen(keys[i%len(keys)])
	}
}
