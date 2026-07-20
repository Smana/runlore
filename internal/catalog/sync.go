// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	git "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// SyncDelta names the repo-relative paths that changed between two synced
// revisions. A nil *SyncDelta means "unknown" — the caller must do a full
// reload. Renames contribute the old name to Removed and the new to Changed.
type SyncDelta struct {
	Changed []string // added or modified
	Removed []string // deleted (or rename sources)
}

// TokenFunc returns a git credential (e.g. a GitHub App installation token or a
// read-scoped PAT). Used as the basic-auth password with username x-access-token.
type TokenFunc func(ctx context.Context) (string, error)

// Syncer keeps a local mirror of an OKF catalog Git repo up to date, calling
// onSync after each successful sync so the reader can re-index. This closes the
// read/write loop: the curator's merged PRs flow back into what the agent searches.
type Syncer struct {
	URL    string
	Branch string
	Dir    string
	Token  TokenFunc // nil / empty => anonymous (public repo)
	Log    *slog.Logger

	lastRev plumbing.Hash // last-synced HEAD; gates re-index on real change

	// tick, when non-nil, supplies Run's poll ticks instead of a real time.Ticker —
	// the seam that lets a test drive the loop cycle by cycle rather than sleeping and
	// hoping a git clone finished in time (mirrors the incidentDebouncer's clock).
	// Always nil in production.
	tick <-chan time.Time
}

func (s *Syncer) auth(ctx context.Context) (*githttp.BasicAuth, error) {
	if s.Token == nil {
		return nil, nil
	}
	tok, err := s.Token(ctx)
	if err != nil {
		return nil, err
	}
	if tok == "" {
		return nil, nil
	}
	return &githttp.BasicAuth{Username: "x-access-token", Password: tok}, nil
}

func (s *Syncer) branch() string {
	if s.Branch == "" {
		return "main"
	}
	return s.Branch
}

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

// Sync clones the repo if the mirror is absent, otherwise fast-forwards it, and
// reports whether HEAD moved since the previous sync (true on the first sync).
// The returned *SyncDelta names the changed/removed paths between the previous
// and new revision; it is nil ("unknown") on the first sync or any diff error,
// which the caller must treat as a full reload.
func (s *Syncer) Sync(ctx context.Context) (bool, *SyncDelta, error) {
	auth, err := s.auth(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("auth: %w", err)
	}
	ref := plumbing.NewBranchReferenceName(s.branch())
	clone := func() (*git.Repository, error) {
		repo, cerr := git.PlainCloneContext(ctx, s.Dir, false, &git.CloneOptions{
			URL:           s.URL,
			ReferenceName: ref,
			SingleBranch:  true,
			Auth:          auth,
		})
		if cerr != nil {
			// Drop the partial checkout so an interrupted/failed clone can't leave a
			// half-written .git that wedges every future Pull — the next tick re-clones.
			_ = os.RemoveAll(s.Dir)
			return nil, cerr
		}
		return repo, nil
	}
	var repo *git.Repository
	if _, statErr := os.Stat(filepath.Join(s.Dir, ".git")); statErr != nil {
		if repo, err = clone(); err != nil {
			return false, nil, err
		}
	} else if repo, err = git.PlainOpen(s.Dir); err != nil {
		// A present-but-unreadable mirror (e.g. an earlier clone killed mid-write)
		// would otherwise error on every Pull forever — discard it and re-clone.
		s.Log.Warn("catalog mirror unreadable; re-cloning", "dir", s.Dir, "err", err)
		if rmErr := os.RemoveAll(s.Dir); rmErr != nil {
			return false, nil, fmt.Errorf("remove corrupt mirror: %w", rmErr)
		}
		if repo, err = clone(); err != nil {
			return false, nil, err
		}
	} else {
		wt, werr := repo.Worktree()
		if werr != nil {
			return false, nil, werr
		}
		perr := wt.PullContext(ctx, &git.PullOptions{
			ReferenceName: ref,
			SingleBranch:  true,
			Auth:          auth,
			Force:         true,
		})
		if perr != nil && !errors.Is(perr, git.NoErrAlreadyUpToDate) {
			return false, nil, perr
		}
	}
	head, err := repo.Head()
	if err != nil {
		return false, nil, err
	}
	rev := head.Hash()
	changed := rev != s.lastRev
	var delta *SyncDelta
	if changed && s.lastRev != (plumbing.Hash{}) {
		delta = s.diffPaths(repo, s.lastRev, rev)
	}
	s.lastRev = rev
	return changed, delta, nil
}

// Run does an initial sync then re-syncs every interval, calling onSync after each
// success. It returns when ctx is done. interval <= 0 defaults to 5m.
func (s *Syncer) Run(ctx context.Context, interval time.Duration, onSync func(*SyncDelta) error) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	do := func() {
		prev := s.lastRev
		changed, delta, err := s.Sync(ctx)
		if err != nil {
			s.Log.Warn("catalog git sync failed", "url", s.URL, "err", err)
			return
		}
		if !changed {
			return
		}
		if err := onSync(delta); err != nil {
			// Re-index failed: roll back to the previous synced revision so the next
			// tick retries it, instead of sticking the catalog on a stale/empty index
			// until upstream HEAD next moves. (Sync already advanced lastRev.)
			s.lastRev = prev
			s.Log.Warn("catalog re-index failed; will retry next sync", "url", s.URL, "err", err)
		}
	}
	do()
	// interval is ignored when tick is set (tests only); the sender paces the loop.
	ticks := s.tick
	if ticks == nil {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		ticks = ticker.C
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticks:
			do()
		}
	}
}
