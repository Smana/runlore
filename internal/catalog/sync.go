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

// Sync clones the repo if the mirror is absent, otherwise fast-forwards it.
func (s *Syncer) Sync(ctx context.Context) error {
	auth, err := s.auth(ctx)
	if err != nil {
		return fmt.Errorf("auth: %w", err)
	}
	ref := plumbing.NewBranchReferenceName(s.branch())
	if _, statErr := os.Stat(filepath.Join(s.Dir, ".git")); statErr != nil {
		_, cerr := git.PlainCloneContext(ctx, s.Dir, false, &git.CloneOptions{
			URL:           s.URL,
			ReferenceName: ref,
			SingleBranch:  true,
			Auth:          auth,
		})
		return cerr
	}
	repo, err := git.PlainOpen(s.Dir)
	if err != nil {
		return err
	}
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	perr := wt.PullContext(ctx, &git.PullOptions{
		ReferenceName: ref,
		SingleBranch:  true,
		Auth:          auth,
		Force:         true,
	})
	if perr != nil && !errors.Is(perr, git.NoErrAlreadyUpToDate) {
		return perr
	}
	return nil
}

// Run does an initial sync then re-syncs every interval, calling onSync after each
// success. It returns when ctx is done. interval <= 0 defaults to 5m.
func (s *Syncer) Run(ctx context.Context, interval time.Duration, onSync func()) {
	if interval <= 0 {
		interval = 5 * time.Minute
	}
	do := func() {
		if err := s.Sync(ctx); err != nil {
			s.Log.Warn("catalog git sync failed", "url", s.URL, "err", err)
			return
		}
		onSync()
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
