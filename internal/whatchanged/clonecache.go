package whatchanged

import (
	"context"
	"os"
	"sync"

	git "github.com/go-git/go-git/v5"
)

// cloneCache reuses a repo clone across the several diffs of one what_changed call.
// Without it, K changes on the same (mono)repo each trigger a full disk clone; with
// it, each source repo is cloned at most once per call. It is request-scoped via the
// context and OWNS the clones' lifetime — close() removes every temp dir it made.
type cloneCache struct {
	mu     sync.Mutex
	clones map[string]*git.Repository // url -> reusable clone
	dirs   []string                   // temp dirs to remove on close
}

type cloneCacheKey struct{}

// WithCloneCache returns a derived context carrying a clone cache, plus a cleanup
// func that removes every clone the cache created. Wrap a batch of diffs that may
// hit the same repo (the what_changed tool's change loop) so each source repo is
// cloned at most once; call the returned func when the batch is done.
func WithCloneCache(ctx context.Context) (context.Context, func()) {
	cc := &cloneCache{clones: map[string]*git.Repository{}}
	return context.WithValue(ctx, cloneCacheKey{}, cc), cc.close
}

func cacheFrom(ctx context.Context) *cloneCache {
	cc, _ := ctx.Value(cloneCacheKey{}).(*cloneCache)
	return cc
}

func (c *cloneCache) get(url string) (*git.Repository, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	repo, ok := c.clones[url]
	return repo, ok
}

// put stores a freshly-cloned repo (at dir) for url, taking ownership of dir's
// cleanup. If another clone for the same url raced in first, it keeps that one and
// returns kept=false so the caller can discard its now-redundant dir.
func (c *cloneCache) put(url string, repo *git.Repository, dir string) (winner *git.Repository, kept bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if existing, ok := c.clones[url]; ok {
		return existing, false
	}
	c.clones[url] = repo
	c.dirs = append(c.dirs, dir)
	return repo, true
}

func (c *cloneCache) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, d := range c.dirs {
		_ = os.RemoveAll(d)
	}
	c.clones, c.dirs = nil, nil
}
