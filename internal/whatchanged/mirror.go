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
	"sort"
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
	dir     string
	max     int
	mu      sync.Mutex // guards entries
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

// evictLocked deletes oldest-mtime mirrors until fewer than max remain,
// making room for the mirror about to be cloned at keep. A dir whose entry
// read/write lock is currently held (an in-flight Acquire) is skipped —
// never delete a mirror out from under a reader. Only entries whose names
// are mirror keys (isMirrorName) are considered — foreign content such as the
// source_diff "source/" subdir or operator files on a mirror PV is never touched.
// Touches disk state only; best-effort by design (an undeletable dir is skipped,
// not fatal).
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
		if !isMirrorName(de.Name()) {
			continue
		}
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

// isMirrorName reports whether a directory entry was created by mirrorKey
// (16 hex chars). Eviction must only ever consider its own mirrors: the
// root may also hold foreign content — the source_diff cache lives in a
// "source" subdir, and an operator PV can carry stray files.
func isMirrorName(name string) bool {
	if len(name) != 16 {
		return false
	}
	for _, c := range name {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
