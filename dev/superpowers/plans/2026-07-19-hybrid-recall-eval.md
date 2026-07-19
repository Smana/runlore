# Hybrid Recall Eval Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Give hybrid (cosine-gated) recall the same honest, pinned-baseline eval the BM25 path already has, derive **measured** values for `hybrid_min_score` / `hybrid_margin_gap` (the config comment currently calls the defaults "conservative placeholders, not measured values" — `internal/config/config.go:422-426`), and define the graduation criteria for dropping the EXPERIMENTAL label.

**Architecture:** Two regimes, mirroring the repo's eval philosophy. (1) **CI regime** — a deterministic bag-of-words embedder (token-hash buckets, L2-normalized: cosine ≈ lexical overlap, fully reproducible) drives `SearchHybrid` end-to-end over the existing fixture KB in `recalleval_test.go`, measuring hybrid Recall@1/3/5, MRR, and fire-rate at the production hybrid gates, with numbers **pinned after first honest run** (the repo's before→after tradition). (2) **Live regime** — an env-gated test embeds the same fixtures against a real `/embeddings` endpoint, prints per-case cosine/margin distributions and the derived threshold recommendation; a maintainer transcribes those into the defaults. CI never needs network; the live run is what "measured" means.

**Tech Stack:** Go stdlib (`hash/fnv`, `math`), existing `internal/catalog` (real `SearchHybrid`), `internal/embed` (live client), `internal/investigate` eval harness (`evalCatalogEntries`, `evalCases`, `computeRetrieval`/`computeFire` patterns). No new dependencies.

**Depends on:** `2026-07-19-embed-cache-chunking.md` (N2) merged first — the live run embeds the fixture corpus and must not depend on one unbounded batch; not a compile-time dependency, so tasks 1–3 can proceed in parallel with N2 review.

## Global Constraints

- Go toolchain `go1.26.5` (pinned in go.mod); do not touch go.mod.
- Quality gate before every commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — `0 issues`, `gofmt -l .` empty. `go test -race ./internal/investigate/...` on touched packages.
- New `.go` files start with `// SPDX-License-Identifier: Apache-2.0` on line 1.
- Conventional Commits; **no co-author trailer, no AI attribution**.
- **Never fabricate a pinned number.** Every `want*` constant is transcribed from a real `-v` run of the harness; the commit that pins it quotes the run in the commit body.
- Production thresholds only: the harness gates with the same defaults production applies (`HybridMinScore 0.80`, `HybridMarginGap 0.05` from `internal/app/investigate.go:103-108`) — no test ever tunes a gate down to force a fire.

## File Structure

- Create: `internal/investigate/hybrideval_test.go` — bag-of-words embedder + CI hybrid retrieval/fire sections + live-mode test. One file: it is one harness with two regimes, and it reuses `recalleval_test.go`'s unexported fixtures (same package).
- Modify: `internal/config/config.go:422-426` — comment update once measured (Task 4).
- Modify: `internal/app/investigate.go:103-108` — default values, only if the measurement warrants (Task 4).
- Modify: `docs/configuration.md` (hybrid/instant-recall section) — measured provenance + graduation criteria (Task 4).

---

### Task 1: Deterministic bag-of-words embedder

**Files:**
- Create: `internal/investigate/hybrideval_test.go`

**Interfaces:**
- Consumes: nothing from production code (test-only type).
- Produces: `type bowEmbedder struct{ dims int }` with `Embed(ctx context.Context, texts []string) ([][]float32, error)` satisfying `catalog.Embedder`; `newBowEmbedder() *bowEmbedder` (dims 512). Tasks 2–3 build catalogs with it.

- [ ] **Step 1: Write the failing test**

Create `internal/investigate/hybrideval_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package investigate

// Hybrid-recall eval harness — the SearchHybrid counterpart of recalleval_test.go.
//
// CI regime: a deterministic bag-of-words embedder (fnv token buckets,
// L2-normalized — cosine ≈ lexical overlap) drives the REAL catalog.SearchHybrid
// fusion + the REAL production hybrid gates, with pinned honest baselines.
// It measures the MACHINERY and the gate philosophy, not semantic quality.
//
// Live regime (TestHybridRecallEvalLive): the same fixtures against a real
// /embeddings endpoint, env-gated; prints the cosine distributions the
// hybrid_min_score / hybrid_margin_gap defaults must be derived from. Semantic
// quality is ONLY measurable there — never pin its numbers into CI.

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"testing"
)

// bowEmbedder is a deterministic bag-of-words embedder: each lowercase token is
// hashed into one of dims buckets, counts are L2-normalized. No network, no
// randomness — same text, same vector, forever.
type bowEmbedder struct{ dims int }

func newBowEmbedder() *bowEmbedder { return &bowEmbedder{dims: 512} }

func (b *bowEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, b.dims)
		for _, tok := range strings.Fields(strings.ToLower(text)) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(tok))
			v[h.Sum32()%uint32(b.dims)]++
		}
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if norm > 0 {
			n := float32(math.Sqrt(norm))
			for j := range v {
				v[j] /= n
			}
		}
		out[i] = v
	}
	return out, nil
}

// TestBowEmbedderDeterministicAndDiscriminative pins the embedder's contract:
// identical texts → cosine 1, overlapping texts → cosine strictly between the
// identical and disjoint cases, disjoint texts → cosine ~0.
func TestBowEmbedderDeterministicAndDiscriminative(t *testing.T) {
	b := newBowEmbedder()
	vs, err := b.Embed(context.Background(), []string{
		"harbor registry iam quota exhausted",
		"harbor registry iam quota exhausted",
		"harbor registry credential rotation",
		"kafka broker partitions isr",
	})
	if err != nil {
		t.Fatal(err)
	}
	cos := func(a, c []float32) float64 {
		var dot float64
		for i := range a {
			dot += float64(a[i]) * float64(c[i])
		}
		return dot // vectors are L2-normalized
	}
	if got := cos(vs[0], vs[1]); math.Abs(got-1.0) > 1e-6 {
		t.Fatalf("identical texts: cosine=%v, want 1.0", got)
	}
	overlap, disjoint := cos(vs[0], vs[2]), cos(vs[0], vs[3])
	if !(overlap > disjoint && overlap < 1.0) {
		t.Fatalf("cosine ordering broken: overlap=%v disjoint=%v", overlap, disjoint)
	}
	if disjoint > 0.10 {
		t.Fatalf("disjoint texts: cosine=%v, want ~0 (hash collisions only)", disjoint)
	}
}
```

- [ ] **Step 2: Run test to verify it fails, then passes**

Run: `go test ./internal/investigate/ -run TestBowEmbedderDeterministic -v`
Expected: PASS on first run (type + test land together; the "failing" state is the pre-file compile absence). Confirm no other test broke: `go test ./internal/investigate/`.

- [ ] **Step 3: Commit**

```bash
git add internal/investigate/hybrideval_test.go
git commit -m "test(investigate): deterministic bag-of-words embedder for hybrid recall eval"
```

---

### Task 2: CI hybrid retrieval-quality section (Recall@k / MRR through real SearchHybrid)

**Files:**
- Modify: `internal/investigate/hybrideval_test.go`

**Interfaces:**
- Consumes: `writeEvalCatalog`-style fixture building — but hybrid needs an embedder attached before load, so add `writeHybridEvalCatalog(t)` here reusing `evalCatalogEntries` (recalleval_test.go:133) and mirroring `writeEvalCatalog` (recalleval_test.go:327) with `catalog.NewEmpty()` + `SetEmbedder` + `ReloadContext`; `evalCases()` (recalleval_test.go:349); `rankOfTarget` (recalleval_test.go:478); `logRetrieval` (recalleval_test.go:582).
- Produces: `writeHybridEvalCatalog(t *testing.T) *catalog.Catalog`, `computeRetrievalHybrid(t, cat, cases) retrievalMetrics`, pinned constants `wantHybridRetrievalHitsAt1`, used again by Task 3.

- [ ] **Step 1: Write the harness section**

Append to `hybrideval_test.go` (add `"os"`, `"path/filepath"`, and the `catalog` import as needed):

```go
// writeHybridEvalCatalog is writeEvalCatalog with vectors: same fixture KB, real
// bleve index, PLUS bag-of-words embeddings built through the same ReloadContext
// path production uses (NewEmpty → SetEmbedder → ReloadContext).
func writeHybridEvalCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	dir := t.TempDir()
	for name, md := range evalCatalogEntries {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(md), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cat := catalog.NewEmpty()
	cat.SetEmbedder(newBowEmbedder())
	if _, err := cat.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if !cat.HasVectors() {
		t.Fatal("fixture catalog has no vectors — hybrid harness would silently test BM25")
	}
	return cat
}

// computeRetrievalHybrid mirrors computeRetrieval but ranks with the REAL
// SearchHybrid (RRF fusion + cosine ordering) instead of SearchScored.
func computeRetrievalHybrid(t *testing.T, cat *catalog.Catalog, cases []evalCase) retrievalMetrics {
	t.Helper()
	m := retrievalMetrics{ranks: map[string]int{}}
	for _, c := range cases {
		if c.negative() {
			continue
		}
		m.positives++
		q := buildRecallQuery(c.request())
		hits, err := cat.SearchHybrid(context.Background(), q, recallCandidateK)
		if err != nil {
			t.Fatalf("%s: SearchHybrid: %v", c.name, err)
		}
		rank := rankOfTarget(hits, c.targets)
		m.ranks[c.name] = rank
		switch {
		case rank == 1:
			m.r1c++
			fallthrough
		case rank >= 1 && rank <= 3:
			m.r3c++
			fallthrough
		case rank >= 1 && rank <= 5:
			m.r5c++
		}
		if rank >= 1 {
			m.mrr += 1.0 / float64(rank)
		}
	}
	// (finalize r1/r3/r5/mrr exactly as computeRetrieval does — read it and mirror
	// its normalization verbatim; if computeRetrieval already factors this out,
	// call the shared helper instead of duplicating.)
	return m
}

// PINNED AFTER FIRST HONEST RUN — transcribe from `go test -run
// TestHybridRecallEvalRetrieval -v`, do not guess, and record the before/after in
// the commit body (repo tradition: recalleval_test.go's "Measured metrics" block).
const wantHybridRetrievalHitsAt1 = -1 // SENTINEL: replace with the measured value before commit

func TestHybridRecallEvalRetrieval(t *testing.T) {
	cat := writeHybridEvalCatalog(t)
	m := computeRetrievalHybrid(t, cat, evalCases())
	logRetrieval(t, "hybrid/bow", m)
	if wantHybridRetrievalHitsAt1 < 0 {
		t.Fatal("pin wantHybridRetrievalHitsAt1 from the -v output above before committing")
	}
	if m.r1c != wantHybridRetrievalHitsAt1 {
		t.Fatalf("hybrid Recall@1 hits = %d, want pinned %d (fusion changed — re-measure and re-pin deliberately)", m.r1c, wantHybridRetrievalHitsAt1)
	}
}
```

NOTE for the implementer: `retrievalMetrics`'s exact field names live at `recalleval_test.go:491` (`computeRetrieval`) — mirror its accumulation/normalization **verbatim** (the sketch above names fields `r1c/r3c/r5c` illustratively; use the real ones). Same package, so everything is directly reachable.

- [ ] **Step 2: First honest run + pin**

Run: `go test ./internal/investigate/ -run TestHybridRecallEvalRetrieval -v`
Expected: FAIL at the sentinel with the measured `Recall@1/3/5 / MRR` logged above it. Transcribe the logged Recall@1 hit count into `wantHybridRetrievalHitsAt1`, and copy the full logged metrics line into a comment above the constant (the before→after tradition).

Run again: `go test ./internal/investigate/ -run TestHybridRecallEvalRetrieval -v`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/investigate/hybrideval_test.go
git commit -m "test(investigate): pinned hybrid retrieval-quality eval (bag-of-words CI regime)"
```

(Quote the measured metrics block in the commit body.)

---

### Task 3: CI hybrid fire-rate at production hybrid gates

**Files:**
- Modify: `internal/investigate/hybrideval_test.go`

**Interfaces:**
- Consumes: `Recall` fields `Hybrid`, `HybridMinScore`, `HybridMarginGap` (`internal/investigate/recall.go:44-46`), production defaults from `internal/app/investigate.go:103-108`; `computeFire`'s structure (`recalleval_test.go:551`) and `logFire`.
- Produces: pinned constants `wantHybridFireCount`, `wantHybridNegFired` (must be 0).

- [ ] **Step 1: Write the fire section**

Append:

```go
// Production hybrid gates (mirror app/investigate.go defaults — never tuned down).
const (
	prodHybridMinScore  = 0.80
	prodHybridMarginGap = 0.05
)

// computeFireHybrid mirrors computeFire with the hybrid searcher + gates wired,
// exactly as BuildInvestigator wires production (Hybrid=catalog, cosine gates).
func computeFireHybrid(t *testing.T, cat *catalog.Catalog, cases []evalCase) fireMetrics {
	t.Helper()
	r := &Recall{
		Catalog:         cat,
		Hybrid:          cat,
		HybridMinScore:  prodHybridMinScore,
		HybridMarginGap: prodHybridMarginGap,
		MinScore:        prodMinScore,
		MarginGap:       prodMarginGap,
		SoloFloor:       prodSoloFloor,
	}
	// (Mirror any additional Recall fields computeFire sets — read
	// recalleval_test.go:551 and copy its construction, adding ONLY the three
	// hybrid fields. The BM25 gates above are dead code in hybrid mode —
	// recall.go:142-144 swaps them for the Hybrid* values — but keeping them set
	// proves the mode switch, not the test, selects the gates.)
	var f fireMetrics
	for _, c := range cases {
		if c.regime != "label" && c.regime != "gitops" {
			continue
		}
		entry, _ := r.lookup(context.Background(), c.request())
		if c.negative() {
			f.negatives++
			if entry != nil {
				f.negFired++
			}
			continue
		}
		f.labelPositives++
		if entry == nil {
			continue
		}
		f.fired++
		if rankOfTarget([]catalog.ScoredEntry{{Entry: *entry}}, c.targets) == 1 {
			f.firedCorrect++
		}
	}
	return f
}

// PINNED AFTER FIRST HONEST RUN (see Task 2's discipline).
const (
	wantHybridFireCount = -1 // SENTINEL: replace with measured fired count
	wantHybridNegFired  = 0  // negatives must NEVER fire — this one is a requirement, not a measurement
)

func TestHybridRecallEvalProductionFireRate(t *testing.T) {
	cat := writeHybridEvalCatalog(t)
	f := computeFireHybrid(t, cat, evalCases())
	logFire(t, "hybrid/bow@prod-gates", f)
	if wantHybridFireCount < 0 {
		t.Fatal("pin wantHybridFireCount from the -v output above before committing")
	}
	if f.fired != wantHybridFireCount {
		t.Fatalf("hybrid fire count = %d, want pinned %d", f.fired, wantHybridFireCount)
	}
	if f.negFired != wantHybridNegFired {
		t.Fatalf("NEGATIVE case fired under hybrid gates (%d) — false recall, the one unacceptable outcome", f.negFired)
	}
}
```

- [ ] **Step 2: First honest run + pin**

Run: `go test ./internal/investigate/ -run TestHybridRecallEvalProductionFireRate -v`
Expected: FAIL at the sentinel with the measured fire line. Pin `wantHybridFireCount` from the log. If any negative fired, **stop and investigate before pinning** — that is a real false-recall finding at default gates, worth its own issue; do not pin a nonzero `wantHybridNegFired`.

Run again + full package: `go test ./internal/investigate/ && go test -race ./internal/investigate/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add internal/investigate/hybrideval_test.go
git commit -m "test(investigate): pinned hybrid fire-rate eval at production cosine gates"
```

---

### Task 4: Live measurement mode + threshold transcription + graduation criteria

**Files:**
- Modify: `internal/investigate/hybrideval_test.go` (live test)
- Modify: `internal/config/config.go:422-426`, `internal/app/investigate.go:103-108`, `docs/configuration.md` (after the live run)

**Interfaces:**
- Consumes: `embed.New(baseURL, model, apiKey)` (`internal/embed/embed.go:40`), `writeHybridEvalCatalog`'s shape (rebuilt here with the live embedder).
- Produces: `TestHybridRecallEvalLive` (env-gated), measured defaults + provenance comment.

- [ ] **Step 1: Write the live test**

Append (add the `embed` and `sort` imports):

```go
// TestHybridRecallEvalLive is the MEASUREMENT run: same fixtures, a real
// /embeddings endpoint. It prints, per positive case, the target's cosine +
// rank + top-vs-runner-up margin, and per negative case the top cosine; then the
// two numbers the defaults are derived from:
//
//	floor candidate  = min(positive top-cosine)   — fire everything real
//	ceiling guard    = max(negative top-cosine)   — never fire a negative
//
// hybrid_min_score must sit between them with margin; hybrid_margin_gap comes
// from the printed margin distribution. Run:
//
//	RUNLORE_EVAL_EMBED_BASE_URL=http://localhost:11434/v1 \
//	RUNLORE_EVAL_EMBED_MODEL=nomic-embed-text \
//	go test ./internal/investigate/ -run TestHybridRecallEvalLive -v
func TestHybridRecallEvalLive(t *testing.T) {
	base := os.Getenv("RUNLORE_EVAL_EMBED_BASE_URL")
	if base == "" {
		t.Skipf("SKIPPED (loud): live hybrid eval needs RUNLORE_EVAL_EMBED_BASE_URL (+RUNLORE_EVAL_EMBED_MODEL, optional RUNLORE_EVAL_EMBED_API_KEY). CI runs the deterministic bag-of-words regime; THIS run is what makes the thresholds 'measured'.")
	}
	model := os.Getenv("RUNLORE_EVAL_EMBED_MODEL")
	if model == "" {
		t.Fatal("RUNLORE_EVAL_EMBED_MODEL is required with RUNLORE_EVAL_EMBED_BASE_URL")
	}
	dir := t.TempDir()
	for name, md := range evalCatalogEntries {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(md), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cat := catalog.NewEmpty()
	cat.SetEmbedder(embed.New(base, model, os.Getenv("RUNLORE_EVAL_EMBED_API_KEY")))
	if _, err := cat.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if !cat.HasVectors() {
		t.Fatal("live embedder produced no vectors (endpoint down? see the catalog WARN)")
	}

	var posTop, negTop, margins []float64
	for _, c := range evalCases() {
		q := buildRecallQuery(c.request())
		hits, err := cat.SearchHybrid(context.Background(), q, recallCandidateK)
		if err != nil || len(hits) == 0 {
			t.Logf("case %-28s: NO HITS (err=%v)", c.name, err)
			continue
		}
		margin := hits[0].Score
		if len(hits) > 1 {
			margin = hits[0].Score - hits[1].Score
		}
		if c.negative() {
			negTop = append(negTop, hits[0].Score)
			t.Logf("case %-28s: NEGATIVE top=%.3f", c.name, hits[0].Score)
			continue
		}
		rank := rankOfTarget(hits, c.targets)
		posTop = append(posTop, hits[0].Score)
		margins = append(margins, margin)
		t.Logf("case %-28s: rank=%d top=%.3f margin=%.3f", c.name, rank, hits[0].Score, margin)
	}
	sort.Float64s(posTop)
	sort.Float64s(negTop)
	sort.Float64s(margins)
	if len(posTop) == 0 || len(negTop) == 0 {
		t.Fatal("distribution incomplete — cannot derive thresholds")
	}
	t.Logf("DERIVE: min positive top=%.3f | max negative top=%.3f | median margin=%.3f | model=%s",
		posTop[0], negTop[len(negTop)-1], margins[len(margins)/2], model)
	t.Logf("RECOMMEND: hybrid_min_score between %.3f and %.3f (with headroom); hybrid_margin_gap ≤ %.3f",
		negTop[len(negTop)-1], posTop[0], margins[len(margins)/2])
	if posTop[0] <= negTop[len(negTop)-1] {
		t.Log("WARNING: distributions OVERLAP — no cosine floor separates positives from negatives for this model; hybrid_min_score alone cannot gate safely (graduation criterion 3 fails)")
	}
}
```

- [ ] **Step 2: Run gate + live run**

Run: `go test ./internal/investigate/ -run TestHybridRecallEvalLive -v` (no env)
Expected: `SKIPPED (loud): …` — visible, self-explaining skip.

Maintainer (or an env-equipped session) runs it with a real endpoint and keeps the `DERIVE`/`RECOMMEND` lines.

- [ ] **Step 3: Transcribe the measurement**

Only after a real live run, in the same PR or a follow-up commit:

1. `internal/app/investigate.go:103-108` — replace `0.80` / `0.05` with the recommended values **iff** they differ meaningfully; otherwise keep and note they were confirmed.
2. `internal/config/config.go:422-426` — replace the sentence `the defaults are conservative placeholders, not measured values` with e.g.:

```go
	// configured (else recall stays BM25). EXPERIMENTAL — thresholds measured
	// 2026-07-XX against <model> via TestHybridRecallEvalLive (see
	// dev/superpowers/plans/2026-07-19-hybrid-recall-eval.md); re-measure when
	// changing the embedding model: cosine scales are model-specific.
```

3. `docs/configuration.md` hybrid section — add the measured provenance line and the graduation criteria:

```markdown
**Graduating hybrid out of EXPERIMENTAL** requires all of:
1. Live-measured thresholds (`TestHybridRecallEvalLive`) for at least one recommended embedding model, recorded here with model + date.
2. Hybrid Recall@1 ≥ the BM25 baseline on the same fixture set (`TestHybridRecallEvalRetrieval` vs `TestRecallEvalRetrieval`).
3. Zero negative-case fires at the shipped default gates, live regime included.
4. The N2 embedding cache merged (reload cost no longer scales with corpus size).
```

- [ ] **Step 4: Full gate + commit**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` → clean, `0 issues.`

```bash
git add internal/investigate/hybrideval_test.go internal/config/config.go internal/app/investigate.go docs/configuration.md
git commit -m "feat(recall): live hybrid eval mode + measured cosine thresholds"
```

(If the live run hasn't happened yet, split: commit the live test alone as `test(investigate): env-gated live hybrid measurement mode`, and leave config/docs transcription to the measurement PR — the plan is complete when both are merged.)

---

## Self-review checklist

- Spec coverage: real-hybrid CI eval (Tasks 1–3), live measurement (Task 4), measured thresholds + comment update (Task 4 step 3), graduation criteria (Task 4 step 3.3). Fire-rate at production gates only — mirrors the BM25 harness's discipline.
- The two sketches that say "mirror the real helper verbatim" (`retrievalMetrics` accumulation, `computeFire`'s `Recall` construction) are deliberate: those helpers are in the same package and their exact shapes are authoritative; duplicating stale copies here would rot. The implementer MUST read `recalleval_test.go:491-580` before Task 2.
- No fabricated numbers: every pinned constant starts as a `-1` sentinel that fails loudly until transcribed from a real run.
- Out of scope (YAGNI): committed fixture-vector files (bag-of-words is cheaper and dependency-free), eval/ scenario-harness integration, reranker×hybrid interaction (rerank replaces the magnitude gate — measuring that combination is a follow-up once hybrid alone is measured).
