# Persisted Vectors + Incremental Index Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Persist the N2 content-hash vector cache to disk (restarts/HA failovers stop re-embedding the whole corpus) and make the bleve index update incrementally from the git-sync diff instead of rebuilding wholesale on every KB HEAD move.

**Architecture:** Three seams, all fail-safe to today's behavior. (1) Doc IDs switch from slice positions to `Entry.Path` (stable across reloads) with a `pathIdx` map for hit resolution — the prerequisite for incremental updates. (2) `Syncer.Sync` reports a `SyncDelta` (changed/removed repo-relative paths via `object.DiffTree` between the previous and new HEAD); `Catalog.ReloadDelta` applies it as targeted `Index`/`Delete` mutations on the live index (bleve is concurrency-safe), falling back to the existing full `ReloadContext` whenever the delta is nil, the catalog is cold, or any mutation errors. (3) The in-memory `vecCache` gains a gob-encoded disk file with a `{Version, Model, Dim}` header, loaded before the first reload and rewritten (temp+fsync+rename, the ledger's pattern) after each successful embed pass; any load problem — missing, corrupt, model or dimension mismatch — is a WARN + cold start, never an error.

**Tech Stack:** Go (toolchain go1.26.5); stdlib `encoding/gob` (compact binary for `map[string][]float32`, one-shot encode/decode, no new dependencies — JSON of float32 corpora is ~10× the bytes and parse cost for a file no human reads); existing `go-git/v5` (`plumbing/object.DiffTree`); bleve v2 MemOnly.

## Global Constraints

- Quality gate before every commit: `go build ./... && go vet ./... && go test ./... && gofmt -l .` (empty) and `golangci-lint run ./...` → `0 issues.`; plus `go test -race ./internal/catalog/... ./internal/app/... ./internal/config/...` before the final push.
- SPDX header `// SPDX-License-Identifier: Apache-2.0` as line 1 of every new `.go` file.
- Conventional Commits; **no co-author / AI-attribution trailers**.
- **Fail-safe:** cache problems ⇒ cold start (re-embed, WARN); incremental problems ⇒ full rebuild — never a wrong or partial index/vector set. The all-or-nothing `HasVectors` invariant from N2 (PR #328) is untouched.
- **No new dependencies** (`go.mod` unchanged).
- **Hash-key stability is NOT assumed:** if N6 (okf-staleness) merges first, `Entry` gains `Status`/`LastValidated` and `entryText` may change shape — every cached key then misses once and the corpus re-embeds one time (harmless by design). Read `entryText` as it exists when you execute; never hard-code its field list in tests (derive expected hashes by calling it).
- The cache file is **pod-local** (leader and standbys each maintain their own; they already each run the syncer). Do not attempt file sharing/locking across replicas.
- Sibling-work note: wave-B PRs (#334 eval, #335 outcome, N6 staleness) may land before this; none touch `sync.go`/`hybrid.go` structurally. Rebase, expect only line drift.

## File Structure

- `internal/catalog/catalog.go` — path-keyed doc IDs, `pathIdx`, `ReloadDelta`, cache save hook
- `internal/catalog/hybrid.go` — path IDs in the cosine ranking / RRF fusion
- `internal/catalog/sync.go` — `SyncDelta`, `diffPaths`, `Sync` third return, `Run` passes the delta
- `internal/catalog/veccache.go` (new) — gob load/save with header validation
- `internal/catalog/{catalog,hybrid,sync,veccache,delta}_test.go` — per-task tests
- `internal/config/config.go` + `internal/config/load.go` — `instant_recall.vector_cache {enabled, dir}`
- `internal/app/catalog.go` — wiring: `EnableVectorCache` + delta-aware sync closure
- `docs/configuration.md` — the new knob + PV note

---

### Task 1: Path-keyed doc IDs + `pathIdx` (pure refactor)

**Files:**
- Modify: `internal/catalog/catalog.go` (`buildIndex`, `Catalog` struct, `NewEmpty`, `ReloadContext`, `SearchScored`)
- Modify: `internal/catalog/hybrid.go` (`SearchHybrid` cosine-ID space, pool resolution)
- Test: `internal/catalog/catalog_test.go` (extend)

**Interfaces:**
- Produces: bleve doc ID = `Entry.Path`; `Catalog.pathIdx map[string]int` (guarded by `mu`, always parallel to `entries`); helper `pathIndex(entries []Entry) map[string]int`. Consumed by Tasks 3 (incremental Index/Delete by path) — nothing outside the package sees a change.

- [ ] **Step 1: Write the failing test** (in `catalog_test.go`, matching its existing `t.TempDir()`+`os.WriteFile` style):

```go
func TestSearchResolvesPathKeyedDocs(t *testing.T) {
	dir := t.TempDir()
	for name, title := range map[string]string{
		"a.md": "cilium agent crashloop",
		"b.md": "postgres disk pressure",
	} {
		entry := "---\ntype: Incident\ntitle: " + title + "\n---\nBody.\n"
		if err := os.WriteFile(filepath.Join(dir, name), []byte(entry), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	hits, err := c.SearchScored("cilium crashloop", 5)
	if err != nil || len(hits) == 0 {
		t.Fatalf("hits=%v err=%v", hits, err)
	}
	if hits[0].Entry.Path != "a.md" {
		t.Errorf("top hit path = %q, want a.md", hits[0].Entry.Path)
	}
	// The doc ID space is now paths: deleting by path must remove the doc.
	c.mu.Lock()
	err = c.index.Delete("a.md")
	c.mu.Unlock()
	if err != nil {
		t.Fatalf("delete by path: %v", err)
	}
	hits, _ = c.SearchScored("cilium crashloop", 5)
	for _, h := range hits {
		if h.Entry.Path == "a.md" {
			t.Error("deleted doc still returned")
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/catalog/ -run TestSearchResolvesPathKeyedDocs -v`
Expected: FAIL — `deleted doc still returned` (Delete("a.md") is a no-op against numeric IDs; the doc was indexed as "0").

- [ ] **Step 3: Implement.** `catalog.go`:

Add the field and helper:

```go
	// pathIdx maps Entry.Path → position in entries; maintained in lockstep with
	// entries under mu. It exists because bleve docs are keyed by Path (stable
	// across reloads — the prerequisite for incremental Index/Delete), so search
	// hits resolve IDs through it instead of parsing positions.
	pathIdx map[string]int
```

```go
func pathIndex(entries []Entry) map[string]int {
	m := make(map[string]int, len(entries))
	for i, e := range entries {
		m[e.Path] = i
	}
	return m
}
```

`buildIndex` — key by path:

```go
	for _, e := range entries {
		doc := map[string]any{
			"title": e.Title,
			"text":  entryText(e),
		}
		if err := idx.Index(e.Path, doc); err != nil {
			return nil, fmt.Errorf("index entry %s: %w", e.Path, err)
		}
	}
```

`NewEmpty`: `return &Catalog{index: idx, pathIdx: map[string]int{}}`.

`ReloadContext` swap: `c.index, c.entries, c.vectors, c.pathIdx = idx, entries, vectors, pathIndex(entries)`.

`SearchScored` hit resolution:

```go
	for _, hit := range res.Hits {
		i, ok := c.pathIdx[hit.ID]
		if !ok {
			continue
		}
		out = append(out, ScoredEntry{Entry: c.entries[i], Score: hit.Score})
	}
```

Drop the now-unused `strconv` import from `catalog.go`.

`hybrid.go` — the two rankings must share the path ID space for RRF to fuse them:

```go
	cosIDs := make([]string, len(ranked))
	for i, idx := range ranked {
		cosIDs[i] = c.entries[idx].Path
	}
```

and pool resolution:

```go
	for _, f := range pool {
		i, ok := c.pathIdx[f.ID]
		if !ok {
			continue
		}
		out = append(out, ScoredEntry{Entry: c.entries[i], Score: cos[i]})
	}
```

Drop the now-unused `strconv` import from `hybrid.go`.

- [ ] **Step 4: Run the full package** — this is a refactor; every existing test must pass unchanged.

Run: `go test ./internal/catalog/ ./internal/investigate/ ./internal/kbmcp/ -count=1`
Expected: all `ok` (recall/eval/kbmcp consume entries, never doc IDs).

- [ ] **Step 5: Commit**

```bash
git add internal/catalog/catalog.go internal/catalog/hybrid.go internal/catalog/catalog_test.go
git commit -m "refactor(catalog): key bleve docs by entry path

Positional doc IDs can't survive an incremental update (any
insert/remove shifts every ID). Path-keyed docs + a pathIdx resolution
map keep IDs stable across reloads — the prerequisite for delta
indexing. Pure refactor: search results are byte-identical."
```

---

### Task 2: `SyncDelta` — the syncer reports changed/removed paths

**Files:**
- Modify: `internal/catalog/sync.go` (`Sync` third return, `diffPaths`, `Run` onSync signature)
- Modify: `internal/app/catalog.go` (closure signature only — mechanical, keeps compiling; real delta use arrives in Task 3)
- Test: `internal/catalog/sync_test.go` (extend; reuse `initBareUpstream`/`commitToUpstream` helpers verbatim)

**Interfaces:**
- Produces: `type SyncDelta struct { Changed, Removed []string }` (repo-relative paths; a rename contributes its old name to Removed and new name to Changed); `Sync(ctx) (changed bool, delta *SyncDelta, err error)` — **delta nil means "unknown, do a full reload"** (first sync, or any diff error); `Run(ctx, interval, onSync func(*SyncDelta) error)`.
- Consumes: `s.lastRev` (existing), `object.DiffTree`.

- [ ] **Step 1: Write the failing test** (in `sync_test.go`):

```go
func TestSyncReportsDelta(t *testing.T) {
	src := initBareUpstream(t)
	commitToUpstream(t, src, "a.md", "alpha v1")
	commitToUpstream(t, src, "b.md", "beta v1")
	s := &Syncer{URL: src, Dir: t.TempDir(), Log: testLogger()}

	// First sync: change reported, delta unknown (nil) — full reload territory.
	changed, delta, err := s.Sync(context.Background())
	if err != nil || !changed {
		t.Fatalf("first sync: changed=%v err=%v", changed, err)
	}
	if delta != nil {
		t.Fatalf("first sync delta = %+v, want nil (unknown)", delta)
	}

	// Modify one file, add one: delta lists exactly those, nothing removed.
	commitToUpstream(t, src, "a.md", "alpha v2")
	commitToUpstream(t, src, "c.md", "gamma v1")
	changed, delta, err = s.Sync(context.Background())
	if err != nil || !changed || delta == nil {
		t.Fatalf("second sync: changed=%v delta=%v err=%v", changed, delta, err)
	}
	sort.Strings(delta.Changed)
	if want := []string{"a.md", "c.md"}; !slices.Equal(delta.Changed, want) {
		t.Errorf("Changed = %v, want %v", delta.Changed, want)
	}
	if len(delta.Removed) != 0 {
		t.Errorf("Removed = %v, want empty", delta.Removed)
	}

	// No upstream movement: no change, no delta.
	changed, delta, err = s.Sync(context.Background())
	if err != nil || changed || delta != nil {
		t.Errorf("idle sync: changed=%v delta=%v err=%v", changed, delta, err)
	}
}
```

(Add `"slices"` and `"sort"` to the test imports if absent. If `commitToUpstream` can't overwrite an existing file, read its body — it writes via the worktree — and adjust only if it errors; the existing helper writes+`Add`+`Commit`, which handles modification fine.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/catalog/ -run TestSyncReportsDelta -v`
Expected: FAIL to compile — `s.Sync` returns 2 values, test wants 3.

- [ ] **Step 3: Implement.** `sync.go`:

```go
// SyncDelta names the repo-relative paths that changed between two synced
// revisions. A nil *SyncDelta means "unknown" — the caller must do a full
// reload. Renames contribute the old name to Removed and the new to Changed.
type SyncDelta struct {
	Changed []string // added or modified
	Removed []string // deleted (or rename sources)
}
```

New import: `"github.com/go-git/go-git/v5/plumbing/object"`.

```go
// diffPaths lists the paths that differ between two commits. Any failure
// returns nil — "unknown", never fatal: the delta is an optimization and the
// caller falls back to a full reload.
func (s *Syncer) diffPaths(repo *git.Repository, from, to plumbing.Hash) *SyncDelta {
	fromC, err := repo.CommitObject(from)
	if err != nil {
		return nil
	}
	toC, err := repo.CommitObject(to)
	if err != nil {
		return nil
	}
	fromT, err := fromC.Tree()
	if err != nil {
		return nil
	}
	toT, err := toC.Tree()
	if err != nil {
		return nil
	}
	changes, err := object.DiffTree(fromT, toT)
	if err != nil {
		return nil
	}
	d := &SyncDelta{}
	for _, ch := range changes {
		if ch.To.Name != "" {
			d.Changed = append(d.Changed, ch.To.Name)
		}
		if ch.From.Name != "" && ch.From.Name != ch.To.Name {
			d.Removed = append(d.Removed, ch.From.Name)
		}
	}
	return d
}
```

`Sync` — change the tail (after `head, err := repo.Head()` succeeds) and the two earlier `return false, ...` error sites to three values (`return false, nil, err` / `return false, nil, fmt.Errorf(...)` — every early return gains a `nil` delta):

```go
	rev := head.Hash()
	changed := rev != s.lastRev
	var delta *SyncDelta
	if changed && s.lastRev != (plumbing.Hash{}) {
		delta = s.diffPaths(repo, s.lastRev, rev)
	}
	s.lastRev = rev
	return changed, delta, nil
```

`Run` — signature `onSync func(*SyncDelta) error`; inside `do`:

```go
		changed, delta, err := s.Sync(ctx)
		...
		if err := onSync(delta); err != nil {
```

(the rollback-on-reindex-failure logic is untouched — after a rollback the next `Sync` diffs from the rolled-back rev, so the retried delta covers both moves).

`internal/app/catalog.go` — mechanical for now (Task 3 uses the delta):

```go
		go syncer.Run(ctx, cfg.Catalog.Git.Interval.Std(), func(_ *catalog.SyncDelta) error {
```

Also update the existing `Run`-based test `TestRunReloadsOnlyOnChange` callback signature (`func(*SyncDelta) error`).

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/catalog/ ./internal/app/ -count=1`
Expected: all `ok`, including the new `TestSyncReportsDelta`.

- [ ] **Step 5: Commit**

```bash
git add internal/catalog/sync.go internal/catalog/sync_test.go internal/app/catalog.go
git commit -m "feat(catalog): git sync reports a changed/removed path delta

Sync already knows exactly which revisions moved; DiffTree turns that
into the path list an incremental re-index needs. nil delta = unknown
(first sync or any diff error) and always means full reload — the
delta is an optimization, never a correctness input."
```

---

### Task 3: `Catalog.ReloadDelta` — incremental Index/Delete with full-rebuild fallback

**Files:**
- Modify: `internal/catalog/catalog.go` (`ReloadDelta`, extract `docFor(e Entry)`)
- Modify: `internal/app/catalog.go` (sync closure calls `ReloadDelta`)
- Test: `internal/catalog/delta_test.go` (new, SPDX line 1)

**Interfaces:**
- Consumes: `SyncDelta` (Task 2), path-keyed docs + `pathIdx` (Task 1), `embedWithCache` (existing).
- Produces: `ReloadDelta(ctx context.Context, dir string, delta *SyncDelta) ([]string, error)` — same contract as `ReloadContext` (returns skipped paths); routes to `ReloadContext` when `delta == nil` or the catalog is cold; on ANY incremental mutation error, falls back to a full rebuild in the same call.

- [ ] **Step 1: Write the failing tests** (`internal/catalog/delta_test.go`, `// SPDX-License-Identifier: Apache-2.0` line 1, package `catalog`):

```go
func writeEntry(t *testing.T, dir, name, title string) {
	t.Helper()
	entry := "---\ntype: Incident\ntitle: " + title + "\n---\nBody about " + title + ".\n"
	if err := os.WriteFile(filepath.Join(dir, name), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
}

// searchTitles returns the top-k result titles for q — the comparable view used
// by the parity property below.
func searchTitles(t *testing.T, c *Catalog, q string) []string {
	t.Helper()
	hits, err := c.SearchScored(q, 10)
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, len(hits))
	for i, h := range hits {
		out[i] = h.Entry.Title
	}
	return out
}

// TestReloadDeltaMatchesFullRebuild pins the core property: an incremental
// reload must be indistinguishable from a from-scratch load of the same dir.
func TestReloadDeltaMatchesFullRebuild(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "a.md", "cilium agent crashloop")
	writeEntry(t, dir, "b.md", "postgres disk pressure")
	writeEntry(t, dir, "c.md", "dns resolution flaking")
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}

	// Mutate: modify a, delete b, add d.
	writeEntry(t, dir, "a.md", "cilium agent oomkilled")
	if err := os.Remove(filepath.Join(dir, "b.md")); err != nil {
		t.Fatal(err)
	}
	writeEntry(t, dir, "d.md", "ingress certificate expired")

	skipped, err := c.ReloadDelta(context.Background(), dir,
		&SyncDelta{Changed: []string{"a.md", "d.md"}, Removed: []string{"b.md"}})
	if err != nil || len(skipped) != 0 {
		t.Fatalf("delta reload: skipped=%v err=%v", skipped, err)
	}

	fresh, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if c.Len() != fresh.Len() {
		t.Fatalf("Len: delta=%d fresh=%d", c.Len(), fresh.Len())
	}
	for _, q := range []string{"cilium oomkilled", "postgres disk", "certificate expired", "dns flaking"} {
		if got, want := searchTitles(t, c, q), searchTitles(t, fresh, q); !slices.Equal(got, want) {
			t.Errorf("query %q: delta=%v fresh=%v", q, got, want)
		}
	}
}

// TestReloadDeltaChangedEntryNowUnparseable: a changed file that fails to parse
// is skipped by Load — its stale doc must not linger in the index.
func TestReloadDeltaChangedEntryNowUnparseable(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "a.md", "cilium agent crashloop")
	writeEntry(t, dir, "b.md", "postgres disk pressure")
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("---\ntitle: [broken\n---\nx"), 0o600); err != nil {
		t.Fatal(err)
	}
	skipped, err := c.ReloadDelta(context.Background(), dir, &SyncDelta{Changed: []string{"a.md"}})
	if err != nil || len(skipped) != 1 {
		t.Fatalf("skipped=%v err=%v, want exactly the broken entry skipped", skipped, err)
	}
	for _, title := range searchTitles(t, c, "cilium crashloop") {
		if title == "cilium agent crashloop" {
			t.Error("stale doc for now-unparseable entry still indexed")
		}
	}
}

// TestReloadDeltaNilFallsBackToFull: nil delta must behave exactly like
// ReloadContext (the first-sync / diff-error path).
func TestReloadDeltaNilFallsBackToFull(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "a.md", "cilium agent crashloop")
	c, err := New(dir)
	if err != nil {
		t.Fatal(err)
	}
	writeEntry(t, dir, "b.md", "postgres disk pressure")
	if _, err := c.ReloadDelta(context.Background(), dir, nil); err != nil {
		t.Fatal(err)
	}
	if c.Len() != 2 {
		t.Errorf("Len=%d, want 2 after nil-delta full reload", c.Len())
	}
}
```

(Imports: `context`, `os`, `path/filepath`, `slices`, `testing`.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/catalog/ -run TestReloadDelta -v`
Expected: FAIL to compile — `c.ReloadDelta undefined`.

- [ ] **Step 3: Implement.** `catalog.go` — extract the doc shape (used by both `buildIndex` and `ReloadDelta`):

```go
func docFor(e Entry) map[string]any {
	return map[string]any{
		"title": e.Title,
		"text":  entryText(e),
	}
}
```

(`buildIndex`'s loop body becomes `if err := idx.Index(e.Path, docFor(e)); ...`.)

```go
// ReloadDelta refreshes the catalog for a known set of changed/removed paths,
// mutating the live index (bleve is safe for concurrent search+index) instead
// of rebuilding it. Entries, vectors, and the embed cache still refresh from a
// full Load walk — files are cheap; only the bleve analysis pass is not — so
// slice/vector/pathIdx consistency is trivially preserved. Falls back to a
// full ReloadContext when the delta is nil (unknown), the catalog is cold, or
// ANY index mutation fails: an incremental error must never leave a wrong
// index behind.
func (c *Catalog) ReloadDelta(ctx context.Context, dir string, delta *SyncDelta) ([]string, error) {
	if delta == nil || !c.ready.Load() {
		return c.ReloadContext(ctx, dir)
	}
	entries, skipped, err := Load(dir)
	if err != nil {
		return nil, err
	}
	vectors, cache := c.embedWithCache(ctx, entries)
	loaded := pathIndex(entries)

	// Mutate the live index outside c.mu: concurrent searches resolve hits via
	// pathIdx, so a just-indexed unknown path is skipped and a just-deleted one
	// simply stops matching — momentary staleness, never a wrong entry.
	c.mu.RLock()
	idx := c.index
	c.mu.RUnlock()
	incremental := func() error {
		for _, p := range delta.Removed {
			if err := idx.Delete(p); err != nil {
				return fmt.Errorf("delete %s: %w", p, err)
			}
		}
		for _, p := range delta.Changed {
			i, ok := loaded[p]
			if !ok {
				// Changed upstream but skipped by Load (now unparseable, or a
				// reserved/non-.md name): drop any stale doc.
				if err := idx.Delete(p); err != nil {
					return fmt.Errorf("delete stale %s: %w", p, err)
				}
				continue
			}
			if err := idx.Index(p, docFor(entries[i])); err != nil {
				return fmt.Errorf("index %s: %w", p, err)
			}
		}
		return nil
	}
	if err := incremental(); err != nil {
		if c.Log != nil {
			c.Log.Warn("catalog incremental re-index failed; rebuilding fully", "err", err)
		}
		return c.ReloadContext(ctx, dir)
	}
	c.mu.Lock()
	c.entries, c.vectors, c.pathIdx = entries, vectors, loaded
	if cache != nil {
		c.vecCache = cache
	}
	c.mu.Unlock()
	return skipped, nil
}
```

`internal/app/catalog.go` — the sync closure uses the delta:

```go
		go syncer.Run(ctx, cfg.Catalog.Git.Interval.Std(), func(delta *catalog.SyncDelta) error {
			skipped, err := cat.ReloadDelta(ctx, dir, delta)
```

(the rest of the closure body — warn on skipped, entries log line, degraded metric, `warnInvalid` — is unchanged).

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/catalog/ ./internal/app/ -count=1 -race`
Expected: all `ok`, including the three new tests.

- [ ] **Step 5: Commit**

```bash
git add internal/catalog/catalog.go internal/catalog/delta_test.go internal/app/catalog.go
git commit -m "feat(catalog): incremental re-index from the git-sync delta

A one-entry KB merge no longer re-analyzes the whole corpus: removed
paths are Delete()d and changed paths re-Index()ed on the live bleve
index, with a full rebuild whenever the delta is unknown or any
mutation errors. Pinned by a delta-vs-fresh-rebuild parity test."
```

---

### Task 4: Persisted vector cache (gob, atomic write, header-validated load)

**Files:**
- Create: `internal/catalog/veccache.go`
- Modify: `internal/catalog/catalog.go` (`vecCachePath`/`vecCacheModel` fields, `EnableVectorCache`, save hook in `ReloadContext` + `ReloadDelta`)
- Test: `internal/catalog/veccache_test.go` (new, SPDX line 1)

**Interfaces:**
- Produces: `(c *Catalog) EnableVectorCache(path, model string)` — call at wiring time, before the first reload; loads the file into `vecCache` (any problem = WARN + empty) and arms saving after every successful embed pass. Unexported `loadVecCache(path, model string, log *slog.Logger) map[string][]float32` and `saveVecCache(path, model string, cache map[string][]float32) error`.
- File format: gob-encoded `vecCacheFile{Version, Model string, Dim int, Vectors map[string][]float32}`; `vecCacheVersion = 1`.

- [ ] **Step 1: Write the failing tests** (`internal/catalog/veccache_test.go`, SPDX line 1, package `catalog`):

```go
func TestVecCacheRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.gob")
	in := map[string][]float32{"k1": {1, 0}, "k2": {0.5, 0.5}}
	if err := saveVecCache(path, "bge-m3", in); err != nil {
		t.Fatal(err)
	}
	out := loadVecCache(path, "bge-m3", nil)
	if len(out) != 2 || out["k1"][0] != 1 || out["k2"][1] != 0.5 {
		t.Fatalf("round trip = %v", out)
	}
	// No temp residue from the atomic write.
	matches, _ := filepath.Glob(filepath.Join(filepath.Dir(path), ".veccache-*"))
	if len(matches) != 0 {
		t.Errorf("temp files left behind: %v", matches)
	}
}

// A model swap must never serve stale vectors: the whole cache is discarded.
func TestVecCacheModelMismatchDiscards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.gob")
	if err := saveVecCache(path, "bge-m3", map[string][]float32{"k": {1}}); err != nil {
		t.Fatal(err)
	}
	if out := loadVecCache(path, "text-embedding-3-small", nil); out != nil {
		t.Fatalf("model mismatch returned %v, want nil (cold start)", out)
	}
}

func TestVecCacheCorruptFileColdStarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.gob")
	if err := os.WriteFile(path, []byte("not gob at all"), 0o600); err != nil {
		t.Fatal(err)
	}
	if out := loadVecCache(path, "bge-m3", nil); out != nil {
		t.Fatalf("corrupt file returned %v, want nil", out)
	}
	if out := loadVecCache(filepath.Join(t.TempDir(), "absent.gob"), "bge-m3", nil); out != nil {
		t.Fatalf("absent file returned %v, want nil", out)
	}
}

// Dimension coherence: a cache whose vectors disagree with the recorded Dim is
// corrupt — discard, don't serve.
func TestVecCacheDimMismatchDiscards(t *testing.T) {
	path := filepath.Join(t.TempDir(), "vectors.gob")
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := gob.NewEncoder(f).Encode(vecCacheFile{
		Version: vecCacheVersion, Model: "bge-m3", Dim: 3,
		Vectors: map[string][]float32{"k": {1, 0}}, // len 2 ≠ Dim 3
	}); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()
	if out := loadVecCache(path, "bge-m3", nil); out != nil {
		t.Fatalf("dim mismatch returned %v, want nil", out)
	}
}

// End-to-end: a second catalog with the same cache file embeds NOTHING on its
// first reload — the restart/HA-failover win this feature exists for.
func TestVecCachePersistsAcrossCatalogs(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "a.md", "cilium agent crashloop")
	writeEntry(t, dir, "b.md", "postgres disk pressure")
	cachePath := filepath.Join(t.TempDir(), "vectors.gob")

	first := NewEmpty()
	emb1 := &countingEmbedder{}
	first.SetEmbedder(emb1)
	first.EnableVectorCache(cachePath, "fake-model")
	if _, err := first.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if emb1.calls == 0 || !first.HasVectors() {
		t.Fatalf("first catalog: calls=%d hasVectors=%v", emb1.calls, first.HasVectors())
	}

	second := NewEmpty()
	emb2 := &countingEmbedder{}
	second.SetEmbedder(emb2)
	second.EnableVectorCache(cachePath, "fake-model")
	if _, err := second.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if emb2.calls != 0 {
		t.Errorf("second catalog embedded %d batches, want 0 (cache warm from disk)", emb2.calls)
	}
	if !second.HasVectors() {
		t.Error("second catalog has no vectors despite warm cache")
	}
}
```

(Reuse the existing `countingEmbedder` from `hybrid_test.go` — same package. Check its field name for the call counter (`calls` at `hybrid_test.go:98`'s receiver) and match it; if its vectors are per-text deterministic, nothing else is needed. Imports: `context`, `encoding/gob`, `os`, `path/filepath`, `testing`.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/catalog/ -run TestVecCache -v`
Expected: FAIL to compile — `saveVecCache`/`loadVecCache`/`EnableVectorCache`/`vecCacheFile` undefined.

- [ ] **Step 3: Implement.** New `internal/catalog/veccache.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"encoding/gob"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
)

// vecCacheFile is the on-disk shape of the persisted embedding cache. gob over
// JSON: map[string][]float32 in one stdlib call, ~1/10th the bytes, and no
// human ever reads this file. The header makes staleness detectable — a cache
// written by a different embedding model (or dimensionality) must never be
// served, so any mismatch discards the whole file and the corpus re-embeds.
type vecCacheFile struct {
	Version int
	Model   string
	Dim     int
	Vectors map[string][]float32
}

const vecCacheVersion = 1

// loadVecCache reads a persisted cache, returning nil (cold start) on ANY
// problem: absent, unreadable, corrupt, version/model/dimension mismatch.
// Fail-safe by contract — the cache is an optimization, never a correctness
// input. log is nil-safe.
func loadVecCache(path, model string, log *slog.Logger) map[string][]float32 {
	f, err := os.Open(path) //nolint:gosec // G304: operator-configured cache path
	if err != nil {
		return nil // absent is the common cold-start case; not worth a warn
	}
	defer func() { _ = f.Close() }()
	var vf vecCacheFile
	if err := gob.NewDecoder(f).Decode(&vf); err != nil {
		if log != nil {
			log.Warn("vector cache unreadable; re-embedding from scratch", "path", path, "err", err)
		}
		return nil
	}
	if vf.Version != vecCacheVersion || vf.Model != model || vf.Dim <= 0 {
		if log != nil {
			log.Warn("vector cache stale (version/model changed); re-embedding from scratch",
				"path", path, "cache_model", vf.Model, "model", model)
		}
		return nil
	}
	for _, v := range vf.Vectors {
		if len(v) != vf.Dim {
			if log != nil {
				log.Warn("vector cache dimension-incoherent; re-embedding from scratch", "path", path)
			}
			return nil
		}
	}
	return vf.Vectors
}

// saveVecCache writes the cache atomically (temp + rename, the ledger's
// pattern) so an interrupted write can never leave a torn file for the next
// startup to trip over. Empty caches are not persisted.
func saveVecCache(path, model string, cache map[string][]float32) error {
	if len(cache) == 0 {
		return nil
	}
	dim := 0
	for _, v := range cache {
		dim = len(v)
		break
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".veccache-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }() // no-op after a successful rename
	if err := gob.NewEncoder(tmp).Encode(vecCacheFile{
		Version: vecCacheVersion, Model: model, Dim: dim, Vectors: cache,
	}); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("rename cache into place: %w", err)
	}
	return nil
}
```

`catalog.go` — fields on `Catalog` (below `vecCache`):

```go
	// vecCachePath/vecCacheModel arm disk persistence of vecCache (EnableVectorCache).
	// Empty path ⇒ in-memory only. Set once at wiring time, before the first Reload.
	vecCachePath  string
	vecCacheModel string
```

```go
// EnableVectorCache persists the embedding cache at path, keyed to the given
// embedding-model identifier: the file is loaded now (before the first reload —
// a warm cache means a restart embeds nothing) and rewritten after each
// successful embed pass. Any load problem is a cold start, never an error.
func (c *Catalog) EnableVectorCache(path, model string) {
	c.vecCachePath, c.vecCacheModel = path, model
	if warm := loadVecCache(path, model, c.Log); warm != nil {
		c.mu.Lock()
		c.vecCache = warm
		c.mu.Unlock()
		if c.Log != nil {
			c.Log.Info("vector cache loaded from disk", "path", path, "vectors", len(warm))
		}
	}
}

// persistVecCache is the post-reload save hook: only a successful, complete
// embed pass (vectors non-nil ⇒ all-or-nothing invariant held) writes the file.
func (c *Catalog) persistVecCache(vectors [][]float32, cache map[string][]float32) {
	if c.vecCachePath == "" || vectors == nil || cache == nil {
		return
	}
	if err := saveVecCache(c.vecCachePath, c.vecCacheModel, cache); err != nil && c.Log != nil {
		c.Log.Warn("vector cache save failed; next restart re-embeds", "path", c.vecCachePath, "err", err)
	}
}
```

Hook it into **both** reload paths, after their `c.mu.Unlock()` (the local `vectors`/`cache` are this reload's — no lock needed):

- `ReloadContext`: add `c.persistVecCache(vectors, cache)` right before `c.ready.Store(true)`.
- `ReloadDelta`: add `c.persistVecCache(vectors, cache)` after its unlock, before `return skipped, nil`.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/catalog/ -count=1 -race`
Expected: all `ok`, including the five new `TestVecCache*` tests.

- [ ] **Step 5: Commit**

```bash
git add internal/catalog/veccache.go internal/catalog/veccache_test.go internal/catalog/catalog.go
git commit -m "feat(catalog): persist the embedding cache across restarts

A restart or HA failover re-embedded the entire corpus; now the
content-hash cache is gob-persisted (atomic temp+rename) after each
successful embed pass and loaded before the first reload. The header
pins version+model+dimension: any mismatch, corruption, or absence is
a WARN + cold start — the cache is never a correctness input."
```

---

### Task 5: Config knob + wiring + docs

**Files:**
- Modify: `internal/config/config.go` (`VectorCache` struct on `InstantRecall`), `internal/config/load.go` (no default needed — zero value is correct)
- Modify: `internal/app/catalog.go` (wire `EnableVectorCache` in both catalog paths)
- Modify: `docs/configuration.md`
- Test: `internal/config/config_test.go` or a focused sibling file; `internal/app` compile coverage via existing tests

**Interfaces:**
- Produces: `cfg.Catalog.InstantRecall.VectorCache` with `IsEnabled()` (nil ⇒ on, mirroring `GitOpsMirror`); effective only when hybrid+embeddings are configured.

- [ ] **Step 1: Write the failing test** (alongside the existing instant-recall config tests):

```go
func TestVectorCacheConfigDefaults(t *testing.T) {
	var vc VectorCache
	if !vc.IsEnabled() {
		t.Error("zero-value vector_cache must be enabled (persistence only ever helps)")
	}
	off := false
	vc = VectorCache{Enabled: &off}
	if vc.IsEnabled() {
		t.Error("explicit enabled:false must disable")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/config/ -run TestVectorCacheConfig -v`
Expected: FAIL to compile — `VectorCache` undefined.

- [ ] **Step 3: Implement.** `config.go`, appended to `InstantRecall` (after the Hybrid fields):

```go
	// VectorCache persists hybrid recall's per-entry embedding cache to disk so
	// a restart or HA failover re-embeds nothing (the in-memory cache already
	// spares unchanged entries WITHIN a process lifetime). Only meaningful when
	// hybrid is on. Default on: persistence only ever helps, and every failure
	// mode (corrupt/stale/missing file) degrades to a cold re-embed.
	VectorCache VectorCache `yaml:"vector_cache"`
```

and next to `GitOpsMirror` (same three-state idiom):

```go
// VectorCache configures on-disk persistence of the hybrid-recall embedding
// cache. Enabled by default (nil/true).
type VectorCache struct {
	Enabled *bool  `yaml:"enabled"`
	Dir     string `yaml:"dir"` // cache dir; "" ⇒ <tmp>/runlore-veccache (ephemeral; point at a PV to persist across restarts)
}

// IsEnabled reports whether cache persistence is on (nil ⇒ default on).
func (v VectorCache) IsEnabled() bool { return v.Enabled == nil || *v.Enabled }
```

`internal/app/catalog.go` — immediately after each `cat.SetEmbedder(embedder)` call (both the git-sync and static-dir paths), arm persistence:

```go
		if embedder != nil {
			cat.SetEmbedder(embedder)
			if vc := cfg.Catalog.InstantRecall.VectorCache; vc.IsEnabled() {
				vdir := vc.Dir
				if vdir == "" {
					vdir = filepath.Join(os.TempDir(), "runlore-veccache")
				}
				cat.EnableVectorCache(filepath.Join(vdir, "vectors.gob"), cfg.Model.Embeddings.Model)
			}
		}
```

(add `"path/filepath"` to the imports; the static-dir branch already guards `embedder != nil` — arm inside it the same way).

`docs/configuration.md` — in the instant-recall/hybrid section, one row/paragraph:

```markdown
| `catalog.instant_recall.vector_cache.enabled` | `true` | Persist the hybrid embedding cache to disk so restarts/failovers re-embed nothing. Every failure mode (missing/corrupt/model-changed file) is a cold re-embed, never an error. |
| `catalog.instant_recall.vector_cache.dir` | `<tmp>/runlore-veccache` | Cache directory. Ephemeral by default — point it at a PersistentVolume to keep it across pod restarts (same pattern as `gitops.mirror.dir`). |
```

- [ ] **Step 4: Run the gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: build/vet/tests clean, `gofmt -l` empty, `0 issues.`
Run: `go test -race ./internal/catalog/... ./internal/app/... ./internal/config/... -count=1`
Expected: all `ok`.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go internal/app/catalog.go docs/configuration.md
git commit -m "feat(config): vector_cache persistence knob, wired and documented

Default on whenever hybrid embeddings are configured; dir defaults to
an ephemeral tmp path with a documented PV upgrade, mirroring
gitops.mirror. Explicit enabled:false keeps the cache in-memory only."
```

---

## Non-goals (say why, so nobody "helpfully" adds them)

- **ANN / vector index**: brute-force cosine over the in-RAM corpus stays. Revisit only when real catalogs exceed ~1–2k entries (the audit's criterion) — below that, an ANN structure costs more in build time and code than the linear scan it replaces.
- **Benchmarks**: regression benchmarks for reload/search belong to the Later-wave benchmark item (L5), not here.
- **Cross-replica cache sharing / file locking**: each pod owns its cache file; the failure mode of not sharing is one redundant embed pass per replica, which the chunked client absorbs.
- **Persisting the bleve index itself**: bleve's on-disk formats bring compaction/upgrade concerns; the index rebuilds in-memory from the (already-local) git mirror in well under a second at current corpus sizes. The expensive network step was the embeddings — that is what persistence targets.

## Execution notes

- Tasks are strictly ordered: 1 → 2 → 3 → 4 → 5 (each consumes the previous task's symbols).
- If N6 (okf-staleness) has merged by execution time, `Entry` carries `Status`/`LastValidated` and `entryText` may include more fields — nothing in this plan changes, but expect `TestReloadEmbedsOnlyChangedEntries`-style tests to exercise the new shape; derive hashes by calling `entryText`, never by re-implementing it.
- One worktree, one PR: `feat/persisted-vectors-incremental-index`, PR title `feat(catalog): persisted embedding cache and incremental re-index`.
