# HEAD-diff sync + readyz catalog gate Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop rebuilding the catalog index on every poll (gate `Reload` on a real git HEAD change) and stop the leader from advertising `readyz` before its knowledge base is warm.

**Architecture:** `Syncer` tracks the last-synced commit hash and reports whether each sync changed HEAD; `Run` re-indexes only on change. `Catalog` carries an atomic "ready" flag set on first successful `Reload`; `runServe` composes catalog-warmth with leadership into the `readyz` callback.

**Tech Stack:** Go 1.26, `github.com/go-git/go-git/v5` (already a dep), `sync/atomic`, standard library.

## Global Constraints

- Go 1.26, no new third-party dependencies.
- `server.New`'s signature must NOT change — readiness composition happens at the call site in `runServe`.
- Catalog readiness means "initial load completed," NOT "has ≥1 entry": a zero-entry but synced catalog is ready (never brick a fresh install).
- Keep catalog sync running on every replica (warm standby) — do NOT make it leader-only.
- After each task: `go build ./... && go vet ./... && go test ./...` green. gofmt-clean (`gofmt -l .` empty) — CI runs golangci-lint with the gofmt formatter and fails on any unformatted file.

---

### Task 1: Catalog readiness flag

**Files:**
- Modify: `internal/catalog/catalog.go`
- Test: `internal/catalog/catalog_test.go`

**Interfaces:**
- Produces: `func (c *Catalog) Ready() bool` — false until the first successful `Reload`; `NewEmpty()` is not ready; `New(dir)` (which calls `Reload`) is ready.

- [ ] **Step 1: Write the failing tests**

Add to `internal/catalog/catalog_test.go`:

```go
func TestNewEmptyNotReady(t *testing.T) {
	if catalog.NewEmpty().Ready() {
		t.Fatal("a freshly-created empty catalog must not report ready before first sync")
	}
}

func TestReloadMarksReady(t *testing.T) {
	dir := t.TempDir() // zero entries on purpose: a synced-but-empty KB is still ready
	c := catalog.NewEmpty()
	if _, err := c.Reload(dir); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if !c.Ready() {
		t.Fatal("after a successful Reload the catalog must report ready (even with 0 entries)")
	}
}

func TestStaticCatalogReadyOnLoad(t *testing.T) {
	dir := t.TempDir()
	c, err := catalog.New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !c.Ready() {
		t.Fatal("a static-dir catalog must be ready immediately after New")
	}
}
```

Note: confirm whether `catalog_test.go` is package `catalog` or `catalog_test`. The existing tests reference symbols directly (e.g. `Load`, `New`) — check the file's `package` line and the existing call style, and match it (drop the `catalog.` qualifier if the tests are in-package). Use the same style the file already uses.

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/catalog/ -run 'TestNewEmptyNotReady|TestReloadMarksReady|TestStaticCatalogReadyOnLoad' -v`
Expected: FAIL — `Ready` undefined.

- [ ] **Step 3: Implement the flag**

In `internal/catalog/catalog.go`:

Add `"sync/atomic"` to the import block. Add a `ready` field to the struct:

```go
type Catalog struct {
	mu      sync.RWMutex
	index   bleve.Index
	entries []Entry
	ready   atomic.Bool // set on first successful Reload; gates readyz on catalog warmth
}
```

At the end of `Reload`, on the success path (just before `return skipped, nil`), add:

```go
	c.ready.Store(true)
```

Add the accessor (e.g. after `Len`):

```go
// Ready reports whether the catalog has completed at least one successful load.
// It stays false for a git-sync catalog (NewEmpty) until the first sync indexes,
// so readyz can keep the leader out of rotation until its KB is warm.
func (c *Catalog) Ready() bool {
	return c.ready.Load()
}
```

`NewEmpty()` needs no change — `atomic.Bool` zero-value is false.

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/catalog/ -run 'TestNewEmptyNotReady|TestReloadMarksReady|TestStaticCatalogReadyOnLoad' -v`
Expected: PASS.

- [ ] **Step 5: Run the full catalog package + gofmt check**

Run: `go test ./internal/catalog/ && gofmt -l internal/catalog/`
Expected: tests PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/catalog/catalog.go internal/catalog/catalog_test.go
git commit -m "feat(catalog): Ready() flag set on first successful Reload"
```

---

### Task 2: HEAD-change gating in the syncer

`Sync` reports whether the commit moved; `Run` re-indexes only on a real change. This changes `Sync`'s signature, so its callers are updated in the same task to keep the build green.

**Files:**
- Modify: `internal/catalog/sync.go`
- Modify: `cmd/lore/main.go` (the one-shot `runCatalogSync` caller at line 907)
- Test: `internal/catalog/sync_test.go`

**Interfaces:**
- Consumes: `go-git` `repo.Head()`, `plumbing.Hash`.
- Produces: `func (s *Syncer) Sync(ctx context.Context) (changed bool, err error)`; `Syncer.lastRev plumbing.Hash`. `Run` unchanged in signature; it now calls `onSync` only when `changed`.

- [ ] **Step 1: Write the failing tests**

The existing `internal/catalog/sync_test.go` has `TestSyncerCloneAndPull` calling `s.Sync(...)`; it will need its calls updated to the new signature (Step 4). First add new tests asserting the change semantics. Reuse the existing test's helper for creating a local upstream repo — read `sync_test.go` to find how it builds the source repo (it clones from a local path). Add:

```go
func TestSyncReportsChangeOnClone(t *testing.T) {
	src := initBareUpstream(t) // helper: see existing test's upstream setup; adapt name
	dir := t.TempDir()
	s := &Syncer{URL: src, Branch: "main", Dir: dir, Log: testLogger()}
	changed, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("Sync: %v", err)
	}
	if !changed {
		t.Fatal("the first sync (clone) must report changed=true")
	}
}

func TestSyncNoChangeOnRepeatedPull(t *testing.T) {
	src := initBareUpstream(t)
	dir := t.TempDir()
	s := &Syncer{URL: src, Branch: "main", Dir: dir, Log: testLogger()}
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	changed, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("second Sync: %v", err)
	}
	if changed {
		t.Fatal("a second sync with no upstream commit must report changed=false")
	}
}

func TestSyncReportsChangeAfterNewCommit(t *testing.T) {
	src := initBareUpstream(t)
	dir := t.TempDir()
	s := &Syncer{URL: src, Branch: "main", Dir: dir, Log: testLogger()}
	if _, err := s.Sync(context.Background()); err != nil {
		t.Fatalf("first Sync: %v", err)
	}
	commitToUpstream(t, src, "runbooks/new.md", "# new") // helper: add+commit+push a file to src
	changed, err := s.Sync(context.Background())
	if err != nil {
		t.Fatalf("third Sync: %v", err)
	}
	if !changed {
		t.Fatal("a sync after a new upstream commit must report changed=true")
	}
}
```

IMPORTANT: the helper names above (`initBareUpstream`, `commitToUpstream`, `testLogger`) are illustrative. Read `sync_test.go` first and REUSE whatever upstream-repo setup the existing `TestSyncerCloneAndPull` already uses (it must create a pushable upstream to test pull). If the existing test creates the upstream inline, extract a small helper or replicate the minimal setup. Do not invent a git mechanism the existing test doesn't already demonstrate.

- [ ] **Step 2: Run the new tests to verify they fail**

Run: `go test ./internal/catalog/ -run 'TestSyncReportsChange|TestSyncNoChange' -v`
Expected: FAIL — `Sync` returns one value (compile error: assignment mismatch), confirming the signature is not yet changed.

- [ ] **Step 3: Implement HEAD gating in `sync.go`**

Add a field to the `Syncer` struct:

```go
type Syncer struct {
	URL    string
	Branch string
	Dir    string
	Token  TokenFunc
	Log    *slog.Logger

	lastRev plumbing.Hash // last-synced HEAD; gates re-index on real change
}
```

Replace the `Sync` method with the change-reporting version (restructured so both the clone and pull paths fall through to a single HEAD read):

```go
// Sync clones the repo if the mirror is absent, otherwise fast-forwards it, and
// reports whether HEAD moved since the previous sync (true on the first sync).
func (s *Syncer) Sync(ctx context.Context) (bool, error) {
	auth, err := s.auth(ctx)
	if err != nil {
		return false, fmt.Errorf("auth: %w", err)
	}
	ref := plumbing.NewBranchReferenceName(s.branch())
	var repo *git.Repository
	if _, statErr := os.Stat(filepath.Join(s.Dir, ".git")); statErr != nil {
		repo, err = git.PlainCloneContext(ctx, s.Dir, false, &git.CloneOptions{
			URL:           s.URL,
			ReferenceName: ref,
			SingleBranch:  true,
			Auth:          auth,
		})
		if err != nil {
			return false, err
		}
	} else {
		repo, err = git.PlainOpen(s.Dir)
		if err != nil {
			return false, err
		}
		wt, werr := repo.Worktree()
		if werr != nil {
			return false, werr
		}
		perr := wt.PullContext(ctx, &git.PullOptions{
			ReferenceName: ref,
			SingleBranch:  true,
			Auth:          auth,
			Force:         true,
		})
		if perr != nil && !errors.Is(perr, git.NoErrAlreadyUpToDate) {
			return false, perr
		}
	}
	head, err := repo.Head()
	if err != nil {
		return false, err
	}
	rev := head.Hash()
	changed := rev != s.lastRev
	s.lastRev = rev
	return changed, nil
}
```

Update `Run`'s `do()` to gate `onSync` on change:

```go
	do := func() {
		changed, err := s.Sync(ctx)
		if err != nil {
			s.Log.Warn("catalog git sync failed", "url", s.URL, "err", err)
			return
		}
		if changed {
			onSync()
		}
	}
```

- [ ] **Step 4: Update the existing callers to the new signature**

In `internal/catalog/sync_test.go`, the existing `TestSyncerCloneAndPull` calls `s.Sync(...)` twice (~lines 51, 60). Change each `if err := s.Sync(ctx); err != nil {` to `if _, err := s.Sync(ctx); err != nil {`.

In `cmd/lore/main.go` line 907, change:
```go
	if err := syncer.Sync(context.Background()); err != nil {
```
to:
```go
	if _, err := syncer.Sync(context.Background()); err != nil {
```

- [ ] **Step 5: Run the catalog tests + build**

Run: `go build ./... && go test ./internal/catalog/ -v`
Expected: build PASS; all sync tests (new + existing `TestSyncerCloneAndPull`) PASS.

- [ ] **Step 6: gofmt check**

Run: `gofmt -l internal/catalog/ cmd/lore/`
Expected: prints nothing.

- [ ] **Step 7: Commit**

```bash
git add internal/catalog/sync.go internal/catalog/sync_test.go cmd/lore/main.go
git commit -m "feat(catalog): gate reload on real HEAD change (Sync reports changed)"
```

---

### Task 3: readyz gated on catalog warmth

Thread the catalog up to `runServe` and compose the readiness callback.

**Files:**
- Modify: `cmd/lore/main.go`
- Test: `cmd/lore/main_test.go` (create if absent)

**Interfaces:**
- Consumes: `Catalog.Ready()` (Task 1); `buildModelAndTools` already returns `*catalog.Catalog` (4th value).
- Produces: `func readyFunc(leader func() bool, cat *catalog.Catalog) func() bool`; `buildInvestigator` now returns `(investigate.Investigator, *catalog.Catalog)`.

- [ ] **Step 1: Write the failing test for `readyFunc`**

Determine the package of `cmd/lore` test files (likely `package main`). Create or append to `cmd/lore/main_test.go`:

```go
func TestReadyFunc(t *testing.T) {
	leaderTrue := func() bool { return true }
	leaderFalse := func() bool { return false }

	// No catalog configured → gate is pure leadership passthrough.
	if !readyFunc(leaderTrue, nil)() {
		t.Fatal("nil catalog + leader=true should be ready")
	}
	if readyFunc(leaderFalse, nil)() {
		t.Fatal("nil catalog + leader=false should not be ready")
	}

	// A not-yet-warm catalog blocks readiness even when leader.
	cold := catalog.NewEmpty()
	if readyFunc(leaderTrue, cold)() {
		t.Fatal("cold catalog must block readiness even when leader=true")
	}

	// A warm catalog is ready only when also leader.
	warm, err := catalog.New(t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !readyFunc(leaderTrue, warm)() {
		t.Fatal("warm catalog + leader=true should be ready")
	}
	if readyFunc(leaderFalse, warm)() {
		t.Fatal("warm catalog + leader=false should not be ready")
	}
}
```

Ensure the test file imports `"github.com/Smana/runlore/internal/catalog"` and `"testing"`.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./cmd/lore/ -run TestReadyFunc -v`
Expected: FAIL — `readyFunc` undefined.

- [ ] **Step 3: Implement `readyFunc` and thread the catalog**

Add `readyFunc` to `cmd/lore/main.go` (near the serve wiring):

```go
// readyFunc gates readiness on leadership AND a warm catalog. A nil catalog
// (none configured) imposes no catalog gate, so readiness is pure leadership.
func readyFunc(leader func() bool, cat *catalog.Catalog) func() bool {
	return func() bool {
		if cat != nil && !cat.Ready() {
			return false
		}
		return leader()
	}
}
```

Change `buildInvestigator` (line 1075) to return the catalog. Its body (line 1080) already has `model, tools, recall, cat := buildModelAndTools(...)`. Update the signature:

```go
func buildInvestigator(ctx context.Context, cfg *config.Config, gp providers.GitOpsProvider, approvals *action.Approvals, auto *action.Auto, metrics *telemetry.Metrics, ledger *outcome.Ledger, log *slog.Logger) (investigate.Investigator, *catalog.Catalog) {
```

Update every `return` in `buildInvestigator` to also return the catalog:
- The early `return investigate.LogInvestigator{Log: log}` (line ~1078, before `cat` exists) becomes `return investigate.LogInvestigator{Log: log}, nil`.
- The final return of the real investigator becomes `return <investigator>, cat`.

Read the full `buildInvestigator` body and update each return path accordingly (there may be more than two).

At the single call site `runServe` (line 221), capture the catalog:

```go
	inv, cat := buildInvestigator(ctx, cfg, gitops, approvals, auto, metrics, ledger, log)
```

Change the `server.New` call (line 302) from `leader.Load` to the composed callback:

```go
	srv := server.New(cfg, queue, readyFunc(leader.Load, cat), acts, metricsHandler, log)
```

- [ ] **Step 4: Run the test + full build**

Run: `go build ./... && go test ./cmd/lore/ -run TestReadyFunc -v`
Expected: build PASS; test PASS.

- [ ] **Step 5: Full suite + gofmt**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l .`
Expected: all PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add cmd/lore/main.go cmd/lore/main_test.go
git commit -m "feat(server): gate readyz on catalog warmth (leader AND loaded)"
```

---

### Task 4: Remove stray report artifact + gitignore

**Files:**
- Delete: `eval/reports/2026-06-23T17-49-33Z-replay.json`
- Modify: `.gitignore`

- [ ] **Step 1: Remove the tracked artifact and ignore the directory**

```bash
git rm eval/reports/2026-06-23T17-49-33Z-replay.json
```

Append to `.gitignore` (create the file if it does not exist):

```
# Generated eval campaign reports (written by `lore eval -report-dir`)
/eval/reports/
```

- [ ] **Step 2: Verify the directory is now ignored and the tree is clean**

Run: `git status --porcelain eval/reports/ ; git check-ignore eval/reports/anything.json`
Expected: no tracked changes pending in `eval/reports/` beyond the deletion/`.gitignore`; `check-ignore` prints the path (confirming it's ignored).

- [ ] **Step 3: Commit**

```bash
git add .gitignore
git commit -m "chore(eval): stop tracking generated eval reports; gitignore eval/reports/"
```

---

## Self-Review

**Spec coverage:**
- A. HEAD-gating (`Sync` returns changed; `Run` gates on change; callers updated) → Task 2. ✅
- B. readyz on catalog warmth (`Catalog.Ready` + `readyFunc` compose + thread cat) → Tasks 1, 3. ✅
- C. cleanup (rm stray report + gitignore) → Task 4. ✅
- Out-of-scope items (leader-only sync, persistence, investigation-block) → not added. ✅

**Placeholder scan:** test helper names in Task 2 are explicitly flagged as illustrative with instructions to reuse the existing `sync_test.go` upstream setup — not placeholders for production code. All production-code steps show complete code. ✅

**Type consistency:** `Ready()` (Task 1) used by `readyFunc` (Task 3); `Sync (bool,error)` (Task 2) callers all updated (sync.go Run, sync_test.go, main.go:907); `buildInvestigator` new return wired at its single caller (line 221). ✅
