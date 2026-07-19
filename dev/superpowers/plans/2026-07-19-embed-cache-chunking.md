# Embedding Cache + Chunked Batches Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop re-embedding the entire KB corpus on every git-sync reload (content-hash vector cache), bound each `/embeddings` request (chunked batches), and make an embed failure observable (WARN + metric) instead of silently degrading hybrid recall to BM25-only.

**Architecture:** `embed.Client.Embed` gains an internal batch loop (the public signature is unchanged). `catalog.Catalog` gains a `vecCache map[sha256(entryText)] → vector` carried across reloads: `ReloadContext` reuses cached vectors for unchanged entries and embeds only the missing subset. The hybrid invariant stays **all-or-nothing** (`HasVectors()` still requires one vector per entry — a partial vector set would silently exclude new entries from the cosine ranking); on failure the swap drops vectors as today, but the cache survives, so the retry embeds only what is still missing, and the failure is WARN-logged (new nil-safe `Catalog.Log`) and counted (new `catalog_embed_degraded_total`).

**Tech Stack:** Go stdlib (`crypto/sha256`, `net/http/httptest`, `log/slog`), existing `internal/embed`, `internal/catalog`, `internal/telemetry` (OTel counters), no new dependencies.

## Global Constraints

- Go toolchain `go1.26.5` (pinned in go.mod); do not touch go.mod.
- Quality gate before every commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — golangci-lint must report `0 issues`, `gofmt -l .` empty. Additionally `go test -race ./internal/embed/... ./internal/catalog/...` on touched packages.
- New `.go` files start with `// SPDX-License-Identifier: Apache-2.0` on line 1.
- Conventional Commits; **no co-author trailer, no AI attribution**.
- Behavior-preserving defaults: BM25-only catalogs (no embedder) must be byte-for-byte unaffected; `Embed`'s public contract (one vector per input, in order) is unchanged.

## File Structure

- `internal/embed/embed.go` — batch loop + extracted `embedBatch` (former `Embed` body).
- `internal/embed/embed_test.go` — batching tests (existing file; add tests).
- `internal/catalog/catalog.go` — `vecCache`/`Log` fields, cache-aware `ReloadContext`.
- `internal/catalog/hybrid.go` — `embedWithCache` replaces `embedEntries`.
- `internal/catalog/hybrid_test.go` — cache reuse / failure-semantics tests (existing file; add tests).
- `internal/telemetry/metrics.go` — `CatalogEmbedDegraded` counter.
- `internal/app/catalog.go` — set `cat.Log`, count degraded reloads in both wiring paths.

---

### Task 1: Chunked embedding batches

**Files:**
- Modify: `internal/embed/embed.go:61-116`
- Test: `internal/embed/embed_test.go`

**Interfaces:**
- Consumes: existing `Client`, `embedRequest`, `embedResponse`, `httpx.DoWithRetry`.
- Produces: `Embed(ctx, texts)` unchanged signature; new unexported `func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error)`; new unexported `const maxEmbedBatch = 256`. Task 2 relies on `Embed` transparently handling any corpus size.

- [ ] **Step 1: Write the failing test**

Append to `internal/embed/embed_test.go` (it already has an httptest-based test to model imports on):

```go
// TestEmbedChunksLargeBatches proves Embed splits oversized input into bounded
// per-request batches while preserving input order across chunk boundaries.
func TestEmbedChunksLargeBatches(t *testing.T) {
	const n = 600 // 256 + 256 + 88
	var mu sync.Mutex
	var batchSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		batchSizes = append(batchSizes, len(req.Input))
		mu.Unlock()
		// Echo each input's numeric suffix back as its vector so the test can
		// verify global ordering end-to-end ("t42" → [42]).
		type datum struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		out := struct {
			Data []datum `json:"data"`
		}{}
		for i, s := range req.Input {
			v, err := strconv.Atoi(strings.TrimPrefix(s, "t"))
			if err != nil {
				t.Errorf("unexpected input %q", s)
			}
			out.Data = append(out.Data, datum{Index: i, Embedding: []float32{float32(v)}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	texts := make([]string, n)
	for i := range texts {
		texts[i] = "t" + strconv.Itoa(i)
	}
	c := New(srv.URL, "test-model", "")
	got, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d vectors, want %d", len(got), n)
	}
	for i, v := range got {
		if len(v) != 1 || v[0] != float32(i) {
			t.Fatalf("vector %d = %v, want [%d] (order broken across chunks)", i, v, i)
		}
	}
	wantSizes := []int{256, 256, 88}
	if !slices.Equal(batchSizes, wantSizes) {
		t.Fatalf("batch sizes = %v, want %v", batchSizes, wantSizes)
	}
}

// TestEmbedChunkFailurePropagates proves a failure in a later chunk fails the
// whole call (no partial result).
func TestEmbedChunkFailurePropagates(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 { // first chunk OK, second chunk 500s
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type datum struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		out := struct {
			Data []datum `json:"data"`
		}{}
		for i := range req.Input {
			out.Data = append(out.Data, datum{Index: i, Embedding: []float32{1}})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	texts := make([]string, 300) // 2 chunks
	for i := range texts {
		texts[i] = "x"
	}
	c := New(srv.URL, "test-model", "")
	if _, err := c.Embed(context.Background(), texts); err == nil {
		t.Fatal("want error when a chunk fails, got nil")
	}
}
```

Add any missing imports (`slices`, `strconv`, `strings`, `sync`, `sync/atomic`) to the test file's import block.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/embed/ -run 'TestEmbedChunk' -v`
Expected: FAIL — `TestEmbedChunksLargeBatches` gets `batch sizes = [600], want [256 256 88]` (today the whole corpus goes in one request). Note: retries make the failure-test pass trivially today only if the 500 exhausts retries — `httpx.DoWithRetry(…, 3, …)` retries 5xx, so `calls.Add(1) > 1` keeps failing and the error propagates; the test still FAILS at compile time first if imports are wrong — fix those, then confirm the sizes assertion is the failure.

- [ ] **Step 3: Implement the batch loop**

In `internal/embed/embed.go`, rename the body of `Embed` to `embedBatch` and add the loop. The final shape:

```go
// maxEmbedBatch bounds the inputs per /embeddings request. Providers cap the
// batch (OpenAI ~2048; many vLLM/Ollama deployments far less), and one oversized
// request would fail the WHOLE corpus embed — chunking keeps a growing KB
// embeddable and each request well under every common cap.
const maxEmbedBatch = 256

// Embed returns one vector per input text, in input order. Empty input → nil.
// Large inputs are transparently split into maxEmbedBatch-sized requests; any
// failing chunk fails the whole call (callers rely on all-or-nothing).
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxEmbedBatch {
		end := min(start+maxEmbedBatch, len(texts))
		vecs, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("inputs %d-%d: %w", start, end-1, err)
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// embedBatch performs one bounded /embeddings request (the pre-chunking Embed body).
func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.model, Input: texts})
	// … the exact former Embed body from the `body, err := json.Marshal(…)` line
	// down to the final `return out, nil`, UNCHANGED (marshal, newReq closure,
	// DoWithRetry, status check, index-placement, missing-vector check).
}
```

The former body moves verbatim — only the `len(texts) == 0` guard stays in `Embed`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/embed/ -v && go test -race ./internal/embed/`
Expected: PASS, all existing tests included (existing single-request tests now exercise a single chunk — same wire shape).

- [ ] **Step 5: Commit**

```bash
git add internal/embed/embed.go internal/embed/embed_test.go
git commit -m "feat(embed): chunk /embeddings requests into bounded batches"
```

---

### Task 2: Content-hash vector cache in the catalog

**Files:**
- Modify: `internal/catalog/catalog.go:19-29` (fields), `:72-100` (`ReloadContext`)
- Modify: `internal/catalog/hybrid.go:44-53` (`embedEntries` → `embedWithCache`)
- Test: `internal/catalog/hybrid_test.go`

**Interfaces:**
- Consumes: `entryText(e Entry) string` (catalog.go:123), `Embedder.Embed`.
- Produces: unexported `func (c *Catalog) embedWithCache(ctx context.Context, entries []Entry) ([][]float32, map[string][]float32)`; new `Catalog` fields `vecCache map[string][]float32` and `Log *slog.Logger` (exported, nil-safe — set at wiring time before the first Reload). Task 3 relies on the `(nil vectors, prev cache)` failure return; Task 4 relies on `Log`.

- [ ] **Step 1: Write the failing test**

Append to `internal/catalog/hybrid_test.go` (reuse its existing entry-file helper if one exists; otherwise use this one):

```go
// countingEmbedder records every Embed call so tests can assert exactly which
// texts were sent (the cache's whole point: unchanged entries are never re-sent).
type countingEmbedder struct {
	calls [][]string
	fail  bool
}

func (f *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.calls = append(f.calls, append([]string(nil), texts...))
	if f.fail {
		return nil, errors.New("embed boom")
	}
	out := make([][]float32, len(texts))
	for i := range texts {
		out[i] = []float32{float32(len(texts[i])), 1}
	}
	return out, nil
}

func writeHybridEntry(t *testing.T, dir, name, title, body string) {
	t.Helper()
	md := "---\ntype: Incident\ntitle: " + title + "\ndescription: d\nresource: ns/app\n---\n\n" + body + "\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(md), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestReloadEmbedsOnlyChangedEntries: reload #1 embeds the full corpus; editing
// ONE entry and reloading embeds exactly that one; an unchanged reload embeds
// nothing. Deleted entries are evicted from the cache.
func TestReloadEmbedsOnlyChangedEntries(t *testing.T) {
	dir := t.TempDir()
	writeHybridEntry(t, dir, "a.md", "Alpha incident", "alpha body")
	writeHybridEntry(t, dir, "b.md", "Beta incident", "beta body")

	emb := &countingEmbedder{}
	c := NewEmpty()
	c.SetEmbedder(emb)

	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(emb.calls) != 1 || len(emb.calls[0]) != 2 {
		t.Fatalf("first reload: calls=%v, want one call with 2 texts", emb.calls)
	}
	if !c.HasVectors() {
		t.Fatal("first reload: HasVectors=false, want true")
	}

	// Edit ONE entry → only its text is re-embedded.
	writeHybridEntry(t, dir, "b.md", "Beta incident", "beta body EDITED")
	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(emb.calls) != 2 || len(emb.calls[1]) != 1 {
		t.Fatalf("edited reload: calls=%v, want second call with exactly 1 text", emb.calls)
	}
	if !strings.Contains(emb.calls[1][0], "EDITED") {
		t.Fatalf("edited reload embedded the wrong text: %q", emb.calls[1][0])
	}
	if !c.HasVectors() {
		t.Fatal("edited reload: HasVectors=false, want true")
	}

	// Unchanged reload → zero embed traffic.
	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if len(emb.calls) != 2 {
		t.Fatalf("unchanged reload: calls=%d, want 2 (no new embed call)", len(emb.calls))
	}

	// Delete an entry → cache must not pin it forever: re-adding it later re-embeds.
	if err := os.Remove(filepath.Join(dir, "a.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	writeHybridEntry(t, dir, "a.md", "Alpha incident", "alpha body")
	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	last := emb.calls[len(emb.calls)-1]
	if len(last) != 1 || !strings.Contains(last[0], "alpha") {
		t.Fatalf("re-added entry not re-embedded after eviction: %v", emb.calls)
	}
}
```

Add missing imports (`errors`, `os`, `path/filepath`, `strings`) to the test file.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/catalog/ -run TestReloadEmbedsOnlyChangedEntries -v`
Expected: FAIL at `edited reload: … want second call with exactly 1 text` — today every reload re-embeds the full corpus (second call has 2 texts).

- [ ] **Step 3: Implement the cache**

In `internal/catalog/catalog.go`, extend the struct (keep existing comments; add):

```go
	// vecCache maps sha256(entryText) → embedding, carried ACROSS reloads so a KB
	// sync only embeds entries whose text actually changed (RunLore merges its own
	// PRs — without this, every merge re-embeds the whole corpus). Rebuilt from the
	// current corpus on each successful embed pass so deleted entries are evicted.
	// Guarded by mu.
	vecCache map[string][]float32
	// Log, when set (wiring time, before the first Reload), surfaces non-fatal
	// reload degradations — an embed failure that leaves hybrid BM25-only. Nil-safe.
	Log *slog.Logger
```

Add `"log/slog"` to imports. In `ReloadContext`, replace the vectors block (`var vectors [][]float32 … }`) with:

```go
	vectors, cache := c.embedWithCache(ctx, entries)
```

and inside the locked swap, after `c.index, c.entries, c.vectors = idx, entries, vectors`, add:

```go
	if cache != nil {
		c.vecCache = cache
	}
```

In `internal/catalog/hybrid.go`, replace `embedEntries` with:

```go
// embedWithCache returns one vector per entry plus the refreshed cache, reusing
// cached vectors for unchanged texts and embedding only the missing subset (the
// client already chunks oversized batches). The hybrid invariant is
// ALL-OR-NOTHING — on any embed failure it returns (nil, previous cache):
// vectors drop for this reload (HasVectors goes false, recall degrades to BM25 —
// never a partial vector set that would silently exclude new entries from the
// cosine ranking), while the surviving cache makes the next attempt embed only
// what is still missing. nil embedder or empty corpus → (nil, nil).
func (c *Catalog) embedWithCache(ctx context.Context, entries []Entry) ([][]float32, map[string][]float32) {
	if c.embedder == nil || len(entries) == 0 {
		return nil, nil
	}
	c.mu.RLock()
	prev := c.vecCache
	c.mu.RUnlock()

	keys := make([]string, len(entries))
	vectors := make([][]float32, len(entries))
	var missTexts []string
	var missIdx []int
	for i, e := range entries {
		text := entryText(e)
		sum := sha256.Sum256([]byte(text))
		keys[i] = hex.EncodeToString(sum[:])
		if v, ok := prev[keys[i]]; ok {
			vectors[i] = v
			continue
		}
		missTexts = append(missTexts, text)
		missIdx = append(missIdx, i)
	}
	if len(missTexts) > 0 {
		vecs, err := c.embedder.Embed(ctx, missTexts)
		if err != nil {
			if c.Log != nil {
				c.Log.Warn("catalog embed failed; hybrid recall degrades to BM25-only until the next successful sync",
					"missing", len(missTexts), "entries", len(entries), "err", err)
			}
			return nil, prev
		}
		for j, v := range vecs {
			vectors[missIdx[j]] = v
		}
	}
	cache := make(map[string][]float32, len(entries))
	for i, k := range keys {
		cache[k] = vectors[i]
	}
	return vectors, cache
}
```

Add `"crypto/sha256"` and `"encoding/hex"` to hybrid.go's imports. Delete the old `embedEntries`.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/catalog/ -v && go test -race ./internal/catalog/`
Expected: PASS (all pre-existing hybrid/catalog tests too — the success-path behavior is identical, just cheaper).

- [ ] **Step 5: Commit**

```bash
git add internal/catalog/catalog.go internal/catalog/hybrid.go internal/catalog/hybrid_test.go
git commit -m "feat(catalog): content-hash vector cache — reload embeds only changed entries"
```

---

### Task 3: Failure semantics — WARN log, cache survival, cheap retry

**Files:**
- Test: `internal/catalog/hybrid_test.go` (implementation already landed in Task 2 — this task pins the failure contract so it can't regress)

**Interfaces:**
- Consumes: `embedWithCache`'s `(nil, prev)` failure return, `Catalog.Log`.
- Produces: pinned behavioral contract for Task 4's wiring (`HasVectors()==false` after a failed reload).

- [ ] **Step 1: Write the failing-or-pinning test**

Append to `internal/catalog/hybrid_test.go`:

```go
// TestReloadEmbedFailureKeepsCacheAndWarns pins the failure contract: a failed
// embed drops vectors (all-or-nothing → BM25-only), logs a WARN, KEEPS the cache
// from prior successful reloads, and the retry embeds only the missing subset.
func TestReloadEmbedFailureKeepsCacheAndWarns(t *testing.T) {
	dir := t.TempDir()
	writeHybridEntry(t, dir, "a.md", "Alpha incident", "alpha body")

	emb := &countingEmbedder{}
	var logBuf bytes.Buffer
	c := NewEmpty()
	c.SetEmbedder(emb)
	c.Log = slog.New(slog.NewTextHandler(&logBuf, nil))

	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if !c.HasVectors() {
		t.Fatal("setup: HasVectors=false after successful reload")
	}

	// Add a new entry, then fail its embed.
	writeHybridEntry(t, dir, "b.md", "Beta incident", "beta body")
	emb.fail = true
	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatalf("reload must stay non-fatal on embed failure: %v", err)
	}
	if c.HasVectors() {
		t.Fatal("failed embed: HasVectors=true, want false (all-or-nothing)")
	}
	if !strings.Contains(logBuf.String(), "hybrid recall degrades to BM25-only") {
		t.Fatalf("no WARN logged on embed failure; log=%q", logBuf.String())
	}

	// Retry succeeds and embeds ONLY the new entry — the cache survived the failure.
	emb.fail = false
	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	last := emb.calls[len(emb.calls)-1]
	if len(last) != 1 || !strings.Contains(last[0], "beta") {
		t.Fatalf("retry after failure re-embedded %d texts (%v), want only the missing 'beta' entry", len(last), last)
	}
	if !c.HasVectors() {
		t.Fatal("retry: HasVectors=false, want true")
	}
}
```

Add `bytes` and `log/slog` to the test imports.

- [ ] **Step 2: Run test**

Run: `go test ./internal/catalog/ -run TestReloadEmbedFailureKeepsCacheAndWarns -v`
Expected: PASS (Task 2 implemented the contract). If it FAILS, the Task 2 implementation drifted from this plan — fix `embedWithCache` until this passes; do not weaken the test.

- [ ] **Step 3: Commit**

```bash
git add internal/catalog/hybrid_test.go
git commit -m "test(catalog): pin embed-failure contract (WARN, cache survival, cheap retry)"
```

---

### Task 4: Degraded-reload metric + app wiring

**Files:**
- Modify: `internal/telemetry/metrics.go` (struct field + constructor entry)
- Modify: `internal/app/catalog.go:47-110` (set `cat.Log`; count degradation in git-sync `onSync` and the static-dir path)

**Interfaces:**
- Consumes: `Catalog.Log` (Task 2), `Catalog.HasVectors()`.
- Produces: `Metrics.CatalogEmbedDegraded metric.Int64Counter`, Prometheus name `runlore_catalog_embed_degraded_total` (via the existing `ctr` helper's naming, matching `catalog_invalid_entries_total`).

- [ ] **Step 1: Add the counter**

In `internal/telemetry/metrics.go`, next to `CatalogInvalidEntries` in the struct:

```go
	CatalogEmbedDegraded metric.Int64Counter // catalog reloads that left hybrid recall without vectors (embed failure — recall silently BM25-only until it clears)
```

and in `NewMetrics()`, next to the `CatalogInvalidEntries` line:

```go
		CatalogEmbedDegraded: ctr("catalog_embed_degraded_total", "catalog reloads that left hybrid recall without vectors (embed failure)"),
```

- [ ] **Step 2: Wire the catalog logger + counter in app**

In `internal/app/catalog.go`:

1. Git-sync path — after `cat := catalog.NewEmpty()` add `cat.Log = log`. In the `onSync` closure, after the `log.Info("catalog synced", …)` line, add:

```go
			if embedder != nil && !cat.HasVectors() {
				if metrics != nil {
					metrics.CatalogEmbedDegraded.Add(ctx, 1)
				}
			}
```

2. Static-dir hybrid path — after `cat = catalog.NewEmpty()` (the `embedder != nil` branch) add `cat.Log = log`, and after the successful `ReloadContext` add the same `if embedder != nil && !cat.HasVectors() { … }` block.

(The detailed WARN with the underlying error already comes from `Catalog.Log` inside `embedWithCache`; app only counts.)

- [ ] **Step 3: Run the full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: build/vet/tests clean, `gofmt -l .` empty, `0 issues.`
Run: `go test -race ./internal/embed/... ./internal/catalog/... ./internal/app/...`
Expected: PASS.

- [ ] **Step 4: Commit**

```bash
git add internal/telemetry/metrics.go internal/app/catalog.go
git commit -m "feat(telemetry): count catalog reloads that degrade hybrid recall to BM25-only"
```

---

## Self-review checklist

- Spec coverage: chunking (Task 1), content-hash cache (Task 2), observable failure + cache-survival retry (Tasks 2/3), metric + wiring (Task 4). `HasVectors` semantics deliberately unchanged (all-or-nothing) and documented in `embedWithCache`'s comment.
- Docs: `docs/observability.md` lists the metric set — if it enumerates counters, add `runlore_catalog_embed_degraded_total` there in Task 4's commit.
- Out of scope (YAGNI): persisted (on-disk) vectors, ANN indexing, incremental bleve indexing, batch-size configurability.
