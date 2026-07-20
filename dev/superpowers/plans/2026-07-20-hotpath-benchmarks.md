# Hot-Path Benchmarks Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give the audit's four hot paths (catalog reload/embed, recall query, outcome-ledger replay/read, ingest chokepoint) `testing.B` regression guardrails — the repo's only benchmarks today are the two clone-strategy ones in `internal/whatchanged/differ_bench_test.go` (PR #332).

**Architecture:** One `*_bench_test.go` file per package, mirroring the established `differ_bench_test.go` style: SPDX line 1, a package-level doc comment stating the exact run command, hermetic fixtures built by a `testing.TB` helper (temp dirs, deterministic inputs, no network), `b.ResetTimer()` after setup. No benchmark enters the CI gate; a docs section records how to run them locally (adding a bench job to the nightly workflows was considered and rejected — `eval.yaml` is an API-key-gated LLM eval and `ci.yaml` is the PR gate; neither fits without new machinery. YAGNI).

**Tech Stack:** Go (toolchain go1.26.5) `testing.B` only; existing packages `internal/{catalog,outcome,trigger,coalesce,investigate}`.

## Global Constraints

- Quality gate before every commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — `0 issues`, `gofmt -l` empty; plus `go test -race` on touched packages.
- SPDX header `// SPDX-License-Identifier: Apache-2.0` line 1 of every new `.go` file.
- Conventional Commits; **no co-author trailer**.
- **Hermetic only**: no network, all fixtures under `b.TempDir()`/in-memory, deterministic inputs (no randomness, no wall-clock dependence in measured code paths).
- Every benchmark carries a doc comment naming the regression it guards.
- **Wave-B independence:** this plan compiles against main `0045916` and uses only stable APIs. The deterministic embedder is REPLICATED here (~15 lines) rather than reused from PR #334 — that branch's `bowEmbedder` is an unexported type in `internal/investigate`'s test files, unreachable from `internal/catalog` even after merge. PR #335 may extend `outcome.Event`/`Aggregate` (a `confirm` kind); Task 3 writes only `open`/`resolve` events, which are stable. PR (N6) adds optional frontmatter fields; Task 1's fixture entries omit them, which must keep behaving identically (that PR's own constraint). If any wave-B PR merges first, expect zero conflicts; note surprises in the final report.

---

### Task 1: Catalog fixture + reload benchmarks

**Files:**
- Create: `internal/catalog/catalog_bench_test.go`

**Interfaces:**
- Consumes: `NewEmpty()`, `(*Catalog).Reload(dir)`, `(*Catalog).ReloadContext(ctx, dir)`, `(*Catalog).SetEmbedder(e)` (`catalog.go:65,74,82,37`), `Embedder` (`hybrid.go:21`: `Embed(ctx context.Context, texts []string) ([][]float32, error)`).
- Produces (used by Task 2): `writeBenchCorpus(tb testing.TB, n int) string` (returns the corpus dir), `benchEmbedder{}` (deterministic `Embedder`), `warmCatalog(tb testing.TB, dir string, hybrid bool) *Catalog`.

- [ ] **Step 1: Write the file with fixture helpers + three reload benchmarks**

```go
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Hermetic hot-path benchmarks for the catalog (audit 2026-07-19, roadmap Later
// wave). Fixtures are ~200 synthetic OKF entries. Run with:
//
//	go test ./internal/catalog/ -bench Benchmark -benchtime 5x -run '^$'

const benchCorpusSize = 200

// writeBenchCorpus writes n deterministic OKF entries and returns the dir.
// Each entry shares common vocabulary ("failure", "rollout") and carries unique
// tokens (svc-i, ns-i%10) so BM25 and the bag-of-words vectors both discriminate.
func writeBenchCorpus(tb testing.TB, n int) string {
	tb.Helper()
	dir := tb.TempDir()
	for i := 0; i < n; i++ {
		body := fmt.Sprintf(`---
type: Incident
title: svc-%d rollout failure in ns-%d
description: HelmRelease svc-%d upgrade failed with probe timeouts
resource: ns-%d/deployment/svc-%d
tags: [rollout, svc-%d]
---
The svc-%d deployment in ns-%d failed its rollout: readiness probes timed out
after the config change. Resolution: revert the values change and reconcile.
`, i, i%10, i, i%10, i, i, i, i%10)
		p := filepath.Join(dir, fmt.Sprintf("entry-%03d.md", i))
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	return dir
}

// benchEmbedder is a minimal deterministic bag-of-words embedder (fnv token
// buckets, L2-normalized). Replicates the shape of the eval's embedder; kept
// local because that one is unexported test code in another package.
type benchEmbedder struct{}

func (benchEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	const dims = 256
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, dims)
		start := -1
		for j := 0; j <= len(t); j++ {
			if j < len(t) && (t[j] >= 'a' && t[j] <= 'z' || t[j] >= '0' && t[j] <= '9') {
				if start < 0 {
					start = j
				}
				continue
			}
			if start >= 0 {
				h := fnv.New32a()
				_, _ = h.Write([]byte(t[start:j]))
				v[h.Sum32()%dims]++
				start = -1
			}
		}
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if norm > 0 {
			n := float32(math.Sqrt(norm))
			for k := range v {
				v[k] /= n
			}
		}
		out[i] = v
	}
	return out, nil
}

// warmCatalog returns a loaded catalog over dir; hybrid additionally wires the
// deterministic embedder and builds vectors.
func warmCatalog(tb testing.TB, dir string, hybrid bool) *Catalog {
	tb.Helper()
	c := NewEmpty()
	if hybrid {
		c.SetEmbedder(benchEmbedder{})
	}
	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		tb.Fatal(err)
	}
	if hybrid && !c.HasVectors() {
		tb.Fatal("bench: vectors not built")
	}
	return c
}

// BenchmarkReloadBM25 guards the cost of a full BM25 index rebuild — the price
// paid on every KB HEAD move regardless of embeddings.
func BenchmarkReloadBM25(b *testing.B) {
	dir := writeBenchCorpus(b, benchCorpusSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := NewEmpty()
		if _, err := c.Reload(dir); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReloadEmbedColdCache guards the worst-case reload: index rebuild plus
// embedding the ENTIRE corpus with an empty vector cache (first boot, cache loss).
func BenchmarkReloadEmbedColdCache(b *testing.B) {
	dir := writeBenchCorpus(b, benchCorpusSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := NewEmpty()
		c.SetEmbedder(benchEmbedder{})
		if _, err := c.ReloadContext(context.Background(), dir); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReloadEmbedWarmCache guards the steady-state reload after PR #328:
// unchanged entries must hit the content-hash cache, so a warm reload should sit
// close to BenchmarkReloadBM25, far below the cold-cache cost. A collapse of this
// gap means the cache regressed.
func BenchmarkReloadEmbedWarmCache(b *testing.B) {
	dir := writeBenchCorpus(b, benchCorpusSize)
	c := warmCatalog(b, dir, true) // warm the vecCache once, unmeasured
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ReloadContext(context.Background(), dir); err != nil {
			b.Fatal(err)
		}
	}
}
```

- [ ] **Step 2: Prove the benchmarks compile and run**

Run: `go test ./internal/catalog/ -bench BenchmarkReload -benchtime 1x -run '^$'`
Expected: three `BenchmarkReload*` lines with ns/op, exit 0. Sanity: warm-cache ns/op noticeably below cold-cache.

- [ ] **Step 3: Full package tests still green**

Run: `go test ./internal/catalog/`
Expected: `ok`

- [ ] **Step 4: Commit**

```bash
git add internal/catalog/catalog_bench_test.go
git commit -m "bench(catalog): reload benchmarks — BM25 rebuild, cold vs warm embed cache"
```

---

### Task 2: Recall query benchmarks

**Files:**
- Modify: `internal/catalog/catalog_bench_test.go` (append)

**Interfaces:**
- Consumes: Task 1's `writeBenchCorpus`/`warmCatalog`; `(*Catalog).SearchScored(query string, k int)` (`catalog.go:208`), `(*Catalog).SearchHybrid(ctx, query string, k int)` (`hybrid.go:109`).

- [ ] **Step 1: Append the two query benchmarks**

```go
// benchQuery mimics an enriched recall query (symptom + namespace + workload +
// alertname vocabulary) against the fixture corpus.
const benchQuery = "svc-42 rollout failure probe timeout ns-2 deployment"

// BenchmarkSearchScored guards the BM25 recall lookup — the per-incident hot
// path when hybrid is off.
func BenchmarkSearchScored(b *testing.B) {
	c := warmCatalog(b, writeBenchCorpus(b, benchCorpusSize), false)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.SearchScored(benchQuery, 20); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkSearchHybrid guards the hybrid recall lookup: query embed + BM25 +
// the brute-force cosine scan over the whole corpus (audit perf finding #3 —
// this is the O(n) path that caps KB size until an ANN lands).
func BenchmarkSearchHybrid(b *testing.B) {
	c := warmCatalog(b, writeBenchCorpus(b, benchCorpusSize), true)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.SearchHybrid(context.Background(), benchQuery, 20); err != nil {
			b.Fatal(err)
		}
	}
}
```

- [ ] **Step 2: Prove they run**

Run: `go test ./internal/catalog/ -bench BenchmarkSearch -benchtime 5x -run '^$'`
Expected: two benchmark lines, exit 0.

- [ ] **Step 3: Commit**

```bash
git add internal/catalog/catalog_bench_test.go
git commit -m "bench(catalog): recall query benchmarks — BM25 and hybrid cosine scan"
```

---

### Task 3: Outcome-ledger benchmarks

**Files:**
- Create: `internal/outcome/ledger_bench_test.go`

**Interfaces:**
- Consumes: `Event` (exported, JSON tags — `ledger.go:61`), `NewWithMaxEvents(path string, maxEvents int)` (`ledger.go:319`; per its doc `0` disables compaction), `(*Ledger).OpenCounts()` (`ledger.go:989`). Fixture writes JSONL directly via `json.Marshal(Event{...})` + `os.WriteFile` — the exact pattern `ledger_test.go:166-169` already uses.

- [ ] **Step 1: Write the file**

```go
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
```

- [ ] **Step 2: Prove they run**

Run: `go test ./internal/outcome/ -bench BenchmarkLedger -benchtime 1x -run '^$'`
Expected: three benchmark lines, exit 0. Sanity: OpenCounts ns/op orders of magnitude below Replay.

- [ ] **Step 3: Full package tests still green**

Run: `go test ./internal/outcome/`
Expected: `ok`

- [ ] **Step 4: Commit**

```bash
git add internal/outcome/ledger_bench_test.go
git commit -m "bench(outcome): ledger replay, compaction cycle, and OpenCounts read"
```

---

### Task 4: Ingest-chokepoint benchmarks

**Files:**
- Create: `internal/trigger/dedup_bench_test.go`
- Create: `internal/coalesce/coalescer_bench_test.go`

**Interfaces:**
- Consumes: `trigger.NewDeduper(window time.Duration)` / `(*Deduper).Seen(key string) bool` (`dedup.go:20,27`); `coalesce.New(cfg Config, out func([]investigate.Request)) *Coalescer` / `(*Coalescer).Add(r investigate.Request)` (`coalescer.go:68,172`), `Config{Debounce, MaxWait, MaxBatch, Cooldown, CorrelationLabels}` (`coalescer.go:23`), `investigate.Request{Title, Message, Labels, GroupKey, Fingerprint, At}` (`investigate.go:44`).

- [ ] **Step 1: Write `internal/trigger/dedup_bench_test.go`**

```go
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
```

- [ ] **Step 2: Write `internal/coalesce/coalescer_bench_test.go`**

```go
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
```

If `Add` on the current code requires more Request fields to take the batch path (compile or panic says so), set exactly the missing fields to deterministic values — do not restructure the benchmark.

- [ ] **Step 3: Prove both run**

Run: `go test ./internal/trigger/ ./internal/coalesce/ -bench . -benchtime 1x -run '^$'`
Expected: two benchmark lines, exit 0.

- [ ] **Step 4: Package tests still green**

Run: `go test ./internal/trigger/ ./internal/coalesce/`
Expected: both `ok`

- [ ] **Step 5: Commit**

```bash
git add internal/trigger/dedup_bench_test.go internal/coalesce/coalescer_bench_test.go
git commit -m "bench(ingest): deduper storm scan and coalescer admission"
```

---

### Task 5: Document the suite (no CI change)

**Files:**
- Modify: `docs/benchmarking.md` (append a section; read the file first and match its heading style — it currently covers the LLM/eval side)

- [ ] **Step 1: Append the section**

```markdown
## Go micro-benchmarks (hot paths)

Hermetic `testing.B` benchmarks guard the hot paths the 2026-07-19 audit named:
no network, deterministic fixtures, safe to run anywhere. They are deliberately
NOT part of the CI gate (numbers on shared runners are noise); run them locally
when touching these packages and compare against your own baseline:

    go test ./internal/whatchanged/ -bench BenchmarkRemote -benchtime 5x -run '^$'
    go test ./internal/catalog/     -bench Benchmark       -benchtime 5x -run '^$'
    go test ./internal/outcome/     -bench BenchmarkLedger -benchtime 5x -run '^$'
    go test ./internal/trigger/ ./internal/coalesce/ -bench . -benchtime 5x -run '^$'

What each guards: clone-vs-mirror (`whatchanged`), BM25 rebuild and cold-vs-warm
embed cache + BM25/hybrid query cost (`catalog`), cold-start replay, one
compaction cycle, and the O(1) OpenCounts read (`outcome`), and per-alert
admission under storm (`trigger`, `coalesce`).
```

- [ ] **Step 2: Full quality gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: build/vet/tests clean, `gofmt -l` empty, `0 issues.`
Then: `go test -race ./internal/catalog/... ./internal/outcome/... ./internal/trigger/... ./internal/coalesce/...`
Expected: all `ok`

- [ ] **Step 3: Commit**

```bash
git add docs/benchmarking.md
git commit -m "docs(benchmarking): document the Go hot-path benchmark suite"
```

---

## Self-Review

- Spec coverage: audit hot paths → Task 1 (reload/embed), Task 2 (recall query), Task 3 (ledger), Task 4 (ingest); CI decision documented in Task 5 (rejected, with rationale). ✓
- No placeholders; every step carries complete code and exact commands. ✓
- Type consistency: `writeBenchCorpus`/`warmCatalog`/`benchEmbedder` defined in Task 1, consumed in Task 2 with identical signatures; all external symbols verified against main `0045916` (`Embed(ctx, []string)`, `NewWithMaxEvents(path, int)`, `Event` JSON shape from `ledger_test.go:166`, `Config`/`Add`/`Request` fields). ✓
