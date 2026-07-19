# Persistent Git Mirror for `whatchanged` Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the full-history clone-per-`what_changed`-call with a persistent per-repo bare mirror that fetches incrementally and is shared across investigations (roadmap N7; the code TODOs it at `differ.go:216`).

**Architecture:** A new `MirrorCache` in `internal/whatchanged` keeps one bare `Mirror:true` clone per remote URL under a configurable directory, keyed by URL hash. `Acquire` fetches under a per-URL write lock, then hands the repo back under a read lock so concurrent investigations can diff while a fetch on the *same* repo is excluded. `Differ` gains an optional `Mirrors` field: mirror errors fall back to the existing clone-per-call path (fail-safe), and the request-scoped `cloneCache` stays layered on top so a batch of diffs acquires the mirror once. History walks (`revisionsInWindow`, `lastPathChange`) keep working because a bare mirror holds full history — proven by test.

**Tech Stack:** Go 1.26 (toolchain go1.26.5), `github.com/go-git/go-git/v5` (already a direct dep — `PlainCloneContext(ctx, path, true, &CloneOptions{Mirror: true})` + `Repository.FetchContext`; verified against the module: `NoErrAlreadyUpToDate` is the no-change sentinel). No new deps.

## Global Constraints

- Quality gate before every commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — 0 issues, `gofmt -l` empty; plus `go test -race ./internal/whatchanged/...` (this plan adds real lock concurrency).
- SPDX `// SPDX-License-Identifier: Apache-2.0` as line 1 of every new `.go` file.
- Conventional Commits; NO co-author trailer; PR metadata in English.
- Fail-safe bias: any mirror error (clone, fetch, unwritable dir) silently falls back to clone-per-call — `what_changed` must never get *worse* because a mirror misbehaved.
- Concurrency rationale (locked in): go-git never garbage-collects packs, so the object store only grows and SHA-addressed reads are pack-safe; ref updates during a `Mirror` fetch are the torn-state risk, so fetch runs under the per-URL **write** lock and every diff/walk under a **read** lock. Do not weaken this to lock-free.

---

### Task 1: `MirrorCache` — bare mirror per URL, fetch-then-read-lock `Acquire`

**Files:**
- Create: `internal/whatchanged/mirror.go`
- Test: `internal/whatchanged/mirror_test.go`

**Interfaces:**
- Produces (consumed by Tasks 2–4): `NewMirrorCache(dir string, maxMirrors int) (*MirrorCache, error)`; `(*MirrorCache).Acquire(ctx context.Context, url string, auth transport.AuthMethod) (*git.Repository, func(), error)` — the `func()` releases the read lock and MUST be called exactly once.
- Consumes: nothing new; fixture helper `buildRepo` from `differ_test.go` (Task 5 generalizes it to `testing.TB`).

- [ ] **Step 1: Write the failing tests**

Create `internal/whatchanged/mirror_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package whatchanged

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestMirrorAcquireClonesOnce: first Acquire clones a bare mirror; a second
// Acquire reuses the same on-disk dir (no re-clone) and still resolves commits.
func TestMirrorAcquireClonesOnce(t *testing.T) {
	src, v1, _ := buildRepo(t)
	mc, err := NewMirrorCache(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	repo, release, err := mc.Acquire(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	if _, err := resolveCommit(repo, v1.String()); err != nil {
		t.Fatalf("resolve v1 in mirror: %v", err)
	}
	release()
	entries, _ := os.ReadDir(mc.dir)
	if len(entries) != 1 {
		t.Fatalf("want 1 mirror dir, got %d", len(entries))
	}
	repo2, release2, err := mc.Acquire(context.Background(), src, nil)
	if err != nil {
		t.Fatalf("second Acquire: %v", err)
	}
	defer release2()
	if _, err := resolveCommit(repo2, v1.String()); err != nil {
		t.Fatalf("resolve v1 on reuse: %v", err)
	}
}

// TestMirrorAcquireFetchesNewCommits: a commit pushed to the source AFTER the
// mirror was created is visible on the next Acquire (incremental fetch).
func TestMirrorAcquireFetchesNewCommits(t *testing.T) {
	src, _, _ := buildRepo(t)
	mc, err := NewMirrorCache(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	_, release, err := mc.Acquire(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	v3 := addCommit(t, src, "apps/harbor/values.yaml", "version: 1.16.0\n", "v3", 3000)
	repo, release2, err := mc.Acquire(context.Background(), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer release2()
	if _, err := resolveCommit(repo, v3.String()); err != nil {
		t.Fatalf("v3 not fetched into mirror: %v", err)
	}
}

// TestMirrorAcquireConcurrent: N concurrent Acquire/release cycles on the same
// URL race-cleanly (run with -race). Each goroutine must resolve a known SHA.
func TestMirrorAcquireConcurrent(t *testing.T) {
	src, v1, _ := buildRepo(t)
	mc, err := NewMirrorCache(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			repo, release, err := mc.Acquire(context.Background(), src, nil)
			if err != nil {
				t.Error(err)
				return
			}
			defer release()
			if _, err := resolveCommit(repo, v1.String()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
}

// TestMirrorAcquireBadURL: an unclonable URL returns an error and leaves no
// half-created mirror dir behind.
func TestMirrorAcquireBadURL(t *testing.T) {
	mc, err := NewMirrorCache(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := mc.Acquire(context.Background(), filepath.Join(t.TempDir(), "nope"), nil); err == nil {
		t.Fatal("want error for unclonable URL")
	}
	entries, _ := os.ReadDir(mc.dir)
	if len(entries) != 0 {
		t.Fatalf("want no leftover dirs, got %d", len(entries))
	}
}
```

Add the `addCommit` helper to `mirror_test.go` (imports: `time`, `git "github.com/go-git/go-git/v5"`, `"github.com/go-git/go-git/v5/plumbing"`, `"github.com/go-git/go-git/v5/plumbing/object"`):

```go
// addCommit writes one file into the fixture repo at dir and commits it with a
// fixed timestamp, returning the new commit hash.
func addCommit(t testing.TB, dir, rel, content, msg string, sec int64) plumbing.Hash {
	t.Helper()
	repo, err := git.PlainOpen(dir)
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	full := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(rel); err != nil {
		t.Fatal(err)
	}
	h, err := wt.Commit(msg, &git.CommitOptions{
		Author: &object.Signature{Name: "t", Email: "t@x", When: time.Unix(sec, 0)},
	})
	if err != nil {
		t.Fatal(err)
	}
	return h
}
```

(The `TestMirrorAcquireFetchesNewCommits` call site passes `(t, src, "apps/harbor/values.yaml", "version: 1.16.0\n", "v3", 3000)` — adjust the earlier snippet's argument order to match this signature.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/whatchanged/ -run TestMirror -v`
Expected: FAIL — `undefined: NewMirrorCache`

- [ ] **Step 3: Implement `mirror.go`**

```go
// SPDX-License-Identifier: Apache-2.0

package whatchanged

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/transport"
)

// MirrorCache keeps one persistent bare mirror per remote URL so repeated
// what_changed calls fetch incrementally instead of re-cloning full history
// (the differ.go clone-per-call NOTE). Safe for concurrent use: fetch runs
// under a per-URL write lock, callers hold a read lock while diffing —
// go-git never GCs packs, so the only torn state a concurrent fetch could
// expose is a mid-update ref, which the lock excludes.
type MirrorCache struct {
	dir string
	max int
	mu  sync.Mutex // guards entries
	entries map[string]*mirrorEntry
}

type mirrorEntry struct {
	lock sync.RWMutex
	path string
}

// NewMirrorCache creates the cache rooted at dir ("" ⇒ a runlore-mirrors dir
// under os.TempDir()), keeping at most maxMirrors mirrors (<=0 ⇒ 10).
func NewMirrorCache(dir string, maxMirrors int) (*MirrorCache, error) {
	if dir == "" {
		dir = filepath.Join(os.TempDir(), "runlore-mirrors")
	}
	if maxMirrors <= 0 {
		maxMirrors = 10
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("mirror dir: %w", err)
	}
	return &MirrorCache{dir: dir, max: maxMirrors, entries: map[string]*mirrorEntry{}}, nil
}

// Acquire returns a repo backed by the persistent bare mirror for url, cloning
// it on first use and fetching otherwise (so the triggering revision is
// present). The returned release func MUST be called exactly once; the repo
// must not be used after release. Errors are safe to fall back from.
func (m *MirrorCache) Acquire(ctx context.Context, url string, auth transport.AuthMethod) (*git.Repository, func(), error) {
	m.mu.Lock()
	e, ok := m.entries[url]
	if !ok {
		e = &mirrorEntry{path: filepath.Join(m.dir, mirrorKey(url))}
		m.entries[url] = e
	}
	m.mu.Unlock()

	e.lock.Lock()
	repo, err := m.syncLocked(ctx, e, url, auth)
	if err != nil {
		e.lock.Unlock()
		return nil, nil, err
	}
	// Downgrade to a read lock for the caller's diffs. The gap between Unlock
	// and RLock only lets another Acquire fetch first — harmless repetition.
	e.lock.Unlock()
	e.lock.RLock()
	return repo, e.lock.RUnlock, nil
}

// syncLocked clones (first use) or fetches (steady state) under e.lock held
// for writing. A failed first clone removes the half-created dir.
func (m *MirrorCache) syncLocked(ctx context.Context, e *mirrorEntry, url string, auth transport.AuthMethod) (*git.Repository, error) {
	if _, statErr := os.Stat(e.path); statErr != nil {
		m.evictLocked(e.path)
		repo, err := git.PlainCloneContext(ctx, e.path, true, &git.CloneOptions{URL: url, Auth: auth, Mirror: true})
		if err != nil {
			_ = os.RemoveAll(e.path)
			return nil, fmt.Errorf("mirror clone %s: %w", url, err)
		}
		return repo, nil
	}
	repo, err := git.PlainOpen(e.path)
	if err != nil {
		return nil, fmt.Errorf("mirror open: %w", err)
	}
	// Mirror clones configure a +refs/*:refs/* refspec, so a plain fetch
	// force-updates every ref (rewritten branches included).
	if err := repo.FetchContext(ctx, &git.FetchOptions{Auth: auth}); err != nil && !errors.Is(err, git.NoErrAlreadyUpToDate) {
		return nil, fmt.Errorf("mirror fetch %s: %w", url, err)
	}
	return repo, nil
}

// mirrorKey derives a filesystem-safe dir name from a remote URL.
func mirrorKey(url string) string {
	h := sha256.Sum256([]byte(url))
	return hex.EncodeToString(h[:8])
}
```

Add a stub so Task 1 compiles before Task 2 implements eviction:

```go
// evictLocked makes room for one more mirror; implemented in the eviction task.
func (m *MirrorCache) evictLocked(keep string) {}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/whatchanged/ -run TestMirror -v`
Expected: PASS (all four)

- [ ] **Step 5: Commit**

```bash
git add internal/whatchanged/mirror.go internal/whatchanged/mirror_test.go
git commit -m "feat(whatchanged): persistent per-repo bare mirror cache"
```

---

### Task 2: Eviction — cap mirror count, oldest-by-mtime first

**Files:**
- Modify: `internal/whatchanged/mirror.go` (replace the `evictLocked` stub)
- Test: `internal/whatchanged/mirror_test.go`

**Interfaces:**
- Produces: `evictLocked(keep string)` — called with `m.mu` NOT held, only from `syncLocked` (which holds the target entry's write lock); skips dirs whose entry lock is held.

- [ ] **Step 1: Write the failing test**

```go
// TestMirrorEviction: with max=2, acquiring a 3rd distinct repo evicts the
// oldest-mtime mirror; the two newest survive.
func TestMirrorEviction(t *testing.T) {
	srcA, _, _ := buildRepo(t)
	srcB, _, _ := buildRepo(t)
	srcC, _, _ := buildRepo(t)
	mc, err := NewMirrorCache(t.TempDir(), 2)
	if err != nil {
		t.Fatal(err)
	}
	for _, src := range []string{srcA, srcB} {
		_, release, err := mc.Acquire(context.Background(), src, nil)
		if err != nil {
			t.Fatal(err)
		}
		release()
	}
	// Age A so it is the eviction victim regardless of clone timing.
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(filepath.Join(mc.dir, mirrorKey(srcA)), old, old); err != nil {
		t.Fatal(err)
	}
	_, release, err := mc.Acquire(context.Background(), srcC, nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	if _, err := os.Stat(filepath.Join(mc.dir, mirrorKey(srcA))); !os.IsNotExist(err) {
		t.Fatal("oldest mirror (A) should have been evicted")
	}
	for _, src := range []string{srcB, srcC} {
		if _, err := os.Stat(filepath.Join(mc.dir, mirrorKey(src))); err != nil {
			t.Fatalf("mirror for %s should survive: %v", src, err)
		}
	}
}
```

Add `"time"` to the test imports.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/whatchanged/ -run TestMirrorEviction -v`
Expected: FAIL — dir for srcA still exists (stub evicts nothing)

- [ ] **Step 3: Implement eviction**

Replace the stub:

```go
// evictLocked deletes oldest-mtime mirrors until fewer than max remain,
// making room for the mirror about to be cloned at keep. A dir whose entry
// read/write lock is currently held (an in-flight Acquire) is skipped —
// never delete a mirror out from under a reader. Touches disk state only;
// best-effort by design (an undeletable dir is skipped, not fatal).
func (m *MirrorCache) evictLocked(keep string) {
	entries, err := os.ReadDir(m.dir)
	if err != nil || len(entries) < m.max {
		return
	}
	type victim struct {
		path string
		mod  int64
	}
	var victims []victim
	m.mu.Lock()
	locked := map[string]bool{}
	for _, e := range m.entries {
		if e.path == keep {
			continue
		}
		if e.lock.TryLock() {
			e.lock.Unlock()
		} else {
			locked[e.path] = true
		}
	}
	m.mu.Unlock()
	for _, de := range entries {
		p := filepath.Join(m.dir, de.Name())
		if p == keep || locked[p] {
			continue
		}
		info, err := de.Info()
		if err != nil {
			continue
		}
		victims = append(victims, victim{path: p, mod: info.ModTime().UnixNano()})
	}
	sort.Slice(victims, func(i, j int) bool { return victims[i].mod < victims[j].mod })
	for i := 0; len(victims)-i > m.max-1 && i < len(victims); i++ {
		_ = os.RemoveAll(victims[i].path)
	}
}
```

Add `"sort"` to the imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test -race ./internal/whatchanged/ -run TestMirror -v`
Expected: PASS (all five)

- [ ] **Step 5: Commit**

```bash
git add internal/whatchanged/mirror.go internal/whatchanged/mirror_test.go
git commit -m "feat(whatchanged): cap mirror count with oldest-mtime eviction"
```

---

### Task 3: `Differ` integration — mirror first, clone-per-call fallback, cloneCache layering

**Files:**
- Modify: `internal/whatchanged/differ.go:154-183` (`cloneToDisk`), `internal/whatchanged/differ.go:32-38` (`Differ` struct), delete the stale NOTE at `differ.go:215-216`
- Modify: `internal/whatchanged/clonecache.go` (shared-entry support)
- Test: `internal/whatchanged/differ_test.go`, `internal/whatchanged/clonecache_test.go`

**Interfaces:**
- Produces: `Differ.Mirrors *MirrorCache` (nil ⇒ exactly today's behavior); `(*cloneCache).putShared(url string, repo *git.Repository, release func()) (*git.Repository, bool)` — like `put` but the cache calls `release` on close instead of removing a dir.
- Consumes: `MirrorCache.Acquire` (Task 1).

- [ ] **Step 1: Write the failing tests**

Append to `differ_test.go`:

```go
// TestRemoteWithMirrorHistoryWalks: with Mirrors set, Remote,
// RemoteLastPathChange and RevisionsInWindow all work off the bare mirror —
// full history is preserved (the reason a shallow clone was rejected).
func TestRemoteWithMirrorHistoryWalks(t *testing.T) {
	src, v1, v2 := buildRepo(t)
	mc, err := NewMirrorCache(t.TempDir(), 10)
	if err != nil {
		t.Fatal(err)
	}
	d := &Differ{Mirrors: mc}
	diff, err := d.Remote(context.Background(), src, v1.String(), v2.String(), "apps/harbor")
	if err != nil {
		t.Fatalf("Remote via mirror: %v", err)
	}
	if len(diff.Files) != 1 || diff.Files[0].Path != "apps/harbor/values.yaml" {
		t.Fatalf("unexpected diff via mirror: %v", paths(diff.Files))
	}
	fb, err := d.RemoteLastPathChange(context.Background(), src, v2.String(), "apps/harbor")
	if err != nil || len(fb.Files) == 0 {
		t.Fatalf("lastPathChange via mirror: files=%d err=%v", len(fb.Files), err)
	}
	revs, err := d.RevisionsInWindow(context.Background(), src, v2.String(), "",
		providers.TimeWindow{Start: time.Unix(0, 0), End: time.Unix(3000, 0)}, 10)
	if err != nil || len(revs) != 2 {
		t.Fatalf("revisionsInWindow via mirror: revs=%d err=%v", len(revs), err)
	}
	// The mirror, not a temp clone, must have been used: exactly one mirror dir.
	entries, _ := os.ReadDir(mc.dir)
	if len(entries) != 1 {
		t.Fatalf("want 1 mirror dir, got %d", len(entries))
	}
}

// TestMirrorFallbackToClone: a broken mirror cache (unwritable dir) must not
// break Remote — it silently falls back to clone-per-call.
func TestMirrorFallbackToClone(t *testing.T) {
	src, v1, v2 := buildRepo(t)
	roDir := filepath.Join(t.TempDir(), "ro")
	if err := os.MkdirAll(roDir, 0o500); err != nil {
		t.Fatal(err)
	}
	mc := &MirrorCache{dir: roDir, max: 10, entries: map[string]*mirrorEntry{}}
	d := &Differ{Mirrors: mc}
	diff, err := d.Remote(context.Background(), src, v1.String(), v2.String(), "apps/harbor")
	if err != nil {
		t.Fatalf("Remote should fall back to clone-per-call: %v", err)
	}
	if len(diff.Files) != 1 {
		t.Fatalf("fallback diff wrong: %v", paths(diff.Files))
	}
}
```

Append to `clonecache_test.go`:

```go
// TestPutSharedReleasesOnClose: a shared entry's release func runs on close,
// and close removes no dir for it.
func TestPutSharedReleasesOnClose(t *testing.T) {
	ctx, done := WithCloneCache(context.Background())
	cc := cacheFrom(ctx)
	released := false
	if _, kept := cc.putShared("u", &git.Repository{}, func() { released = true }); !kept {
		t.Fatal("first putShared must keep")
	}
	if _, ok := cc.get("u"); !ok {
		t.Fatal("shared entry must be gettable")
	}
	done()
	if !released {
		t.Fatal("release must run on close")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/whatchanged/ -run 'TestRemoteWithMirror|TestMirrorFallback|TestPutShared' -v`
Expected: FAIL — `unknown field Mirrors` / `undefined: putShared`

- [ ] **Step 3: Implement**

`clonecache.go` — add a closers list:

```go
type cloneCache struct {
	mu      sync.Mutex
	clones  map[string]*git.Repository
	dirs    []string
	closers []func() // release funcs for shared (mirror-backed) entries
}

// putShared stores a repo the cache does NOT own on disk (a mirror-backed
// clone); close calls release instead of removing anything. Same race
// contract as put: if another entry won, kept=false and the caller must
// release its own handle.
func (c *cloneCache) putShared(url string, repo *git.Repository, release func()) (*git.Repository, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.clones[url]; ok {
		return existing, false
	}
	c.clones[url] = repo
	c.closers = append(c.closers, release)
	return repo, true
}
```

In `close()`, before removing dirs: `for _, f := range c.closers { f() }` and reset `c.closers = nil`.

`differ.go` — add the field and the mirror path in `cloneToDisk` (after the cache-hit check, before the temp-dir clone; `auth` is already computed above the clone — move the `d.auth(ctx)` call up so both paths share it):

```go
type Differ struct {
	TokenSource func(context.Context) (string, error)
	// Mirrors, when set, backs clones with a persistent per-repo bare mirror
	// (incremental fetch, shared across investigations). nil ⇒ full clone per
	// call, exactly as before. Mirror errors fall back to clone-per-call.
	Mirrors *MirrorCache
}
```

```go
	if d.Mirrors != nil {
		if repo, release, merr := d.Mirrors.Acquire(ctx, url, auth); merr == nil {
			if cc != nil {
				winner, kept := cc.putShared(url, repo, release)
				if !kept {
					release() // another goroutine won the race; drop our lock
				}
				return winner, noop, nil
			}
			return repo, release, nil
		}
		// fall through: a broken mirror must never break what_changed
	}
```

Delete the now-stale `NOTE (perf)` comment block above `RemoteFromParent`.

- [ ] **Step 4: Run the package suite**

Run: `go test -race ./internal/whatchanged/ -v`
Expected: PASS — every pre-existing test unchanged and green (nil `Mirrors` preserves old behavior)

- [ ] **Step 5: Commit**

```bash
git add internal/whatchanged/differ.go internal/whatchanged/clonecache.go internal/whatchanged/differ_test.go internal/whatchanged/clonecache_test.go
git commit -m "feat(whatchanged): route clones through the mirror cache with clone-per-call fallback"
```

---

### Task 4: Config + wiring

**Files:**
- Modify: `internal/config/config.go:572-574` (`GitOps` struct), `internal/config/load.go` (defaults), `internal/config/config.go` `Validate` (bounds)
- Modify: `internal/app/gitops.go:23` (wire the cache into the `Differ`)
- Modify: `docs/configuration.md` (document the block)
- Test: `internal/config/config_test.go` (or the package's existing validate test file), `internal/app` build only

**Interfaces:**
- Produces: `config.GitOps.Mirror config.GitOpsMirror` with `Enabled *bool` (nil/true ⇒ on — the repo's `Rerank *bool` idiom), `Dir string` ("" ⇒ MirrorCache's TempDir default), `Max int` (0 ⇒ 10 via load defaults); `(GitOpsMirror).IsEnabled() bool`.

- [ ] **Step 1: Write the failing config test**

In the config package's validate tests:

```go
func TestGitOpsMirrorConfig(t *testing.T) {
	var m GitOpsMirror
	if !m.IsEnabled() {
		t.Fatal("zero-value mirror config must default to enabled")
	}
	off := false
	m.Enabled = &off
	if m.IsEnabled() {
		t.Fatal("enabled:false must disable")
	}
	c := validServeConfig() // reuse the package's existing valid-config helper; adapt name to what the test file uses
	c.GitOps.Mirror.Max = -1
	if err := c.Validate(); err == nil {
		t.Fatal("negative gitops.mirror.max must fail validation")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestGitOpsMirror -v`
Expected: FAIL — `undefined: GitOpsMirror`

- [ ] **Step 3: Implement**

`config.go`:

```go
type GitOps struct {
	Engine string       `yaml:"engine"` // "flux" (default) | "argocd"
	Mirror GitOpsMirror `yaml:"mirror"` // persistent clone mirror for what_changed
}

// GitOpsMirror configures the persistent per-repo bare mirror backing
// what_changed clones. Enabled by default (nil/true): a mirror only ever
// falls back to the previous clone-per-call behavior on error.
type GitOpsMirror struct {
	Enabled *bool  `yaml:"enabled"` // nil/true ⇒ on; false ⇒ clone per call (legacy)
	Dir     string `yaml:"dir"`     // mirror root; "" ⇒ <tmp>/runlore-mirrors (ephemeral; point at a PV to persist across restarts)
	Max     int    `yaml:"max"`     // max mirrors kept (LRU by mtime); 0 ⇒ 10
}

// IsEnabled reports whether the mirror cache is on (nil ⇒ default on).
func (m GitOpsMirror) IsEnabled() bool { return m.Enabled == nil || *m.Enabled }
```

In `Validate` (near the other bounds checks): `if c.GitOps.Mirror.Max < 0 { return fmt.Errorf("gitops.mirror.max must be >= 0") }`. In `load.go` `applyDefaults`: `if c.GitOps.Mirror.Max == 0 { c.GitOps.Mirror.Max = 10 }`.

`internal/app/gitops.go`:

```go
	differ := &whatchanged.Differ{TokenSource: BuildForgeTokenSource(cfg, log)}
	if cfg.GitOps.Mirror.IsEnabled() {
		if mc, err := whatchanged.NewMirrorCache(cfg.GitOps.Mirror.Dir, cfg.GitOps.Mirror.Max); err != nil {
			log.Warn("gitops: mirror cache unavailable; falling back to clone-per-call", "err", err)
		} else {
			differ.Mirrors = mc
		}
	}
```

`docs/configuration.md`: add the `gitops.mirror` block next to the existing `gitops.engine` doc — keys, defaults, the PV note, and `enabled: false` as the escape hatch.

- [ ] **Step 4: Run the gate**

Run: `go build ./... && go vet ./... && go test ./internal/config/ ./internal/app/ ./internal/whatchanged/ && gofmt -l . && golangci-lint run ./...`
Expected: build/vet/tests pass, no gofmt output, `0 issues.`

- [ ] **Step 5: Commit**

```bash
git add internal/config/ internal/app/gitops.go docs/configuration.md
git commit -m "feat(config): gitops.mirror block wiring the what_changed mirror cache"
```

---

### Task 5: Benchmark — clone-per-call vs mirror (the repo's first `testing.B`)

**Files:**
- Modify: `internal/whatchanged/differ_test.go:23` (`buildRepo(t *testing.T)` → `buildRepo(t testing.TB)`; `t.Helper()`, `t.TempDir()`, `t.Fatal` all exist on `testing.TB` — no body changes)
- Create: `internal/whatchanged/differ_bench_test.go`

- [ ] **Step 1: Generalize the fixture**

Change `buildRepo`'s signature to `testing.TB`; run `go test ./internal/whatchanged/` — Expected: PASS (pure widening).

- [ ] **Step 2: Write the benchmarks**

```go
// SPDX-License-Identifier: Apache-2.0

package whatchanged

import (
	"context"
	"testing"
)

// Hermetic benchmarks (local fixture repo, no network) contrasting the two
// clone strategies behind what_changed. Run with:
//   go test ./internal/whatchanged/ -bench BenchmarkRemote -benchtime 5x -run '^$'
func BenchmarkRemoteClonePerCall(b *testing.B) {
	src, v1, v2 := buildRepo(b)
	d := &Differ{}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Remote(context.Background(), src, v1.String(), v2.String(), "apps/harbor"); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkRemoteMirror(b *testing.B) {
	src, v1, v2 := buildRepo(b)
	mc, err := NewMirrorCache(b.TempDir(), 10)
	if err != nil {
		b.Fatal(err)
	}
	d := &Differ{Mirrors: mc}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := d.Remote(context.Background(), src, v1.String(), v2.String(), "apps/harbor"); err != nil {
			b.Fatal(err)
		}
	}
}
```

- [ ] **Step 3: Run and record**

Run: `go test ./internal/whatchanged/ -bench BenchmarkRemote -benchtime 5x -run '^$'`
Expected: both benchmarks complete; mirror ns/op below clone-per-call after the first iteration (record the numbers in the PR description — the fixture is tiny, so the gap understates real-repo wins).

- [ ] **Step 4: Full gate + race**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./... && go test -race ./internal/whatchanged/...`
Expected: all green, `0 issues.`

- [ ] **Step 5: Commit**

```bash
git add internal/whatchanged/differ_test.go internal/whatchanged/differ_bench_test.go
git commit -m "test(whatchanged): benchmark clone-per-call vs mirror fetch"
```
