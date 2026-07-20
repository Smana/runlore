// SPDX-License-Identifier: Apache-2.0

package coalesce

import (
	"fmt"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/investigate"
)

// BenchmarkCoalescerAdd guards per-alert admission cost at the coalescer: key
// derivation + batch bookkeeping under the mutex, across 1000 live correlation
// keys. Debounce is far in the future so nothing flushes mid-measurement.
// Run: go test ./internal/coalesce/ -bench . -benchtime 5x -run '^$'
func BenchmarkCoalescerAdd(b *testing.B) {
	c := New(Config{Debounce: time.Hour, MaxWait: 2 * time.Hour, MaxBatch: 1 << 20},
		func([]investigate.Request) {})
	reqs := make([]investigate.Request, 1000)
	at := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range reqs {
		reqs[i] = investigate.Request{
			Title:       fmt.Sprintf("HighErrorRate-%d", i),
			Message:     "error budget burn",
			Labels:      map[string]string{"namespace": fmt.Sprintf("ns-%d", i)},
			GroupKey:    fmt.Sprintf("group-%d", i),
			Fingerprint: fmt.Sprintf("fp-%d", i),
			At:          at,
		}
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c.Add(reqs[i%len(reqs)])
	}
}
