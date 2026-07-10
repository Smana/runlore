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
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

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

// Sync clones the repo if the mirror is absent, otherwise fast-forwards it, and
// reports whether HEAD moved since the previous sync (true on the first sync).
func (s *Syncer) Sync(ctx context.Context) (bool, error) {
	auth, err := s.auth(ctx)
	if err != nil {
		return false, fmt.Errorf("auth: %w", err)
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
			return false, err
		}
	} else if repo, err = git.PlainOpen(s.Dir); err != nil {
		// A present-but-unreadable mirror (e.g. an earlier clone killed mid-write)
		// would otherwise error on every Pull forever — discard it and re-clone.
		s.Log.Warn("catalog mirror unreadable; re-cloning", "dir", s.Dir, "err", err)
		if rmErr := os.RemoveAll(s.Dir); rmErr != nil {
			return false, fmt.Errorf("remove corrupt mirror: %w", rmErr)
		}
		if repo, err = clone(); err != nil {
			return false, err
		}
	} else {
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

// Run does an initial sync then re-syncs every interval, calling onSync after each
// success. It returns when ctx is done. interval <= 0 defaults to 5m.
func (s *Syncer) Run(ctx context.Context, interval time.Duration, onSync func() error) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	do := func() {
		prev := s.lastRev
		changed, err := s.Sync(ctx)
		if err != nil {
			s.Log.Warn("catalog git sync failed", "url", s.URL, "err", err)
			return
		}
		if !changed {
			return
		}
		if err := onSync(); err != nil {
			// Re-index failed: roll back to the previous synced revision so the next
			// tick retries it, instead of sticking the catalog on a stale/empty index
			// until upstream HEAD next moves. (Sync already advanced lastRev.)
			s.lastRev = prev
			s.Log.Warn("catalog re-index failed; will retry next sync", "url", s.URL, "err", err)
		}
	}
	do()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			do()
		}
	}
}
