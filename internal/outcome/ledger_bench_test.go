// SPDX-License-Identifier: Apache-2.0

package outcome

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// Hermetic ledger benchmarks (audit 2026-07-19, roadmap Later wave). The fixture
// is a 10k-event JSONL written directly (no fsync-per-append), the same shape
// ledger_test.go builds for corruption tests. Run with:
//
//	go test ./internal/outcome/ -bench BenchmarkLedger -benchtime 5x -run '^$'

const benchEventPairs = 5000 // 5000 open + 5000 resolve = 10k events

// writeBenchLedger writes nPairs open("recall")/resolve pairs across 50 entries
// and returns the file path. Deterministic timestamps; resolves always after
// their opens so pairing takes the fast path.
func writeBenchLedger(tb testing.TB, nPairs int) string {
	tb.Helper()
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var buf bytes.Buffer
	for i := 0; i < nPairs; i++ {
		fp := fmt.Sprintf("fp-%d", i)
		open, err := json.Marshal(Event{
			Event: "open", Fingerprint: fp, Kind: "recall",
			Entry: fmt.Sprintf("entry-%02d.md", i%50),
			At:    t0.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			tb.Fatal(err)
		}
		res, err := json.Marshal(Event{
			Event: "resolve", Fingerprint: fp,
			At: t0.Add(time.Duration(i)*time.Minute + 30*time.Second),
		})
		if err != nil {
			tb.Fatal(err)
		}
		buf.Write(open)
		buf.WriteByte('\n')
		buf.Write(res)
		buf.WriteByte('\n')
	}
	p := filepath.Join(tb.TempDir(), "ledger.jsonl")
	if err := os.WriteFile(p, buf.Bytes(), 0o600); err != nil {
		tb.Fatal(err)
	}
	return p
}

// BenchmarkLedgerReplay guards cold-start replay cost of a 10k-event file with
// compaction disabled — the "6-month-old ledger with max_events: 0" audit case.
func BenchmarkLedgerReplay(b *testing.B) {
	p := writeBenchLedger(b, benchEventPairs)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := NewWithMaxEvents(p, 0); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLedgerCompactedLoad guards one replay+compaction cycle: loading 10k
// events over a 1k bound rewrites the file with a checkpoint. Each iteration
// copies the fixture to a fresh path (compaction mutates it), unmeasured.
func BenchmarkLedgerCompactedLoad(b *testing.B) {
	src := writeBenchLedger(b, benchEventPairs)
	raw, err := os.ReadFile(src)
	if err != nil {
		b.Fatal(err)
	}
	dir := b.TempDir()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		b.StopTimer()
		p := filepath.Join(dir, fmt.Sprintf("ledger-%d.jsonl", i))
		if err := os.WriteFile(p, raw, 0o600); err != nil {
			b.Fatal(err)
		}
		b.StartTimer()
		if _, err := NewWithMaxEvents(p, 1000); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkLedgerOpenCounts guards the recall hot-path read: OpenCounts must
// stay an O(entries) copy of the incrementally-maintained aggregate, never a
// file re-read (the audit confirmed this design — this pins it).
func BenchmarkLedgerOpenCounts(b *testing.B) {
	l, err := NewWithMaxEvents(writeBenchLedger(b, benchEventPairs), 0)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := l.OpenCounts(); err != nil {
			b.Fatal(err)
		}
	}
}
