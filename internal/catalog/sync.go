// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	git "github.com/go-git/go-git/v5"
	gitcfg "github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	githttp "github.com/go-git/go-git/v5/plumbing/transport/http"
)

// tagRefPrefix is the fully-qualified namespace of a tag. Together with a bare
// object id it spells the two IMMUTABLE revisions a Syncer accepts — see Syncer.Branch.
const tagRefPrefix = "refs/tags/"

// pinRefSpecs is what a pin fetch asks for: every branch and every tag. A pin may
// name any commit in the repository, so narrowing this would make "bump the pin"
// work for some revisions and silently not for others.
var pinRefSpecs = []gitcfg.RefSpec{
	"+refs/heads/*:refs/remotes/origin/*",
	"+refs/tags/*:refs/tags/*",
}

// isObjectID reports whether s is a full-length git object id (SHA-1 40 hex chars,
// SHA-256 64). Abbreviations are deliberately NOT accepted: they are ambiguous, and
// a 7-character hex string is as likely to be a tag name as a commit.
//
// internal/config carries the same rule for the operator-facing side of the same
// contract (config.isObjectID). Keep the two in step: config decides what may be
// written, this decides what the resulting string means.
func isObjectID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		switch {
		case r >= '0' && r <= '9', r >= 'a' && r <= 'f', r >= 'A' && r <= 'F':
		default:
			return false
		}
	}
	return true
}

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
	URL string
	// Branch is the revision the mirror follows. A bare name is a BRANCH and is
	// tracked: every poll pulls whatever upstream moved it to. Two spellings instead
	// PIN the mirror to an immutable revision that no upstream push can change:
	//
	//	refs/tags/<name>       a tag
	//	<40- or 64-char hex>   a commit id
	//
	// Anything unqualified stays a branch, so every configuration written before
	// pinning existed resolves down the identical path, byte for byte.
	//
	// It is one field rather than two because a syncer follows exactly one revision;
	// the operator-facing surface (catalog.commons.branch vs .ref) is where the two
	// intentions are kept apart, and config.ApplyDefaults normalises a `ref` into the
	// spellings above. Same contract, read from its two ends.
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

// pin returns the immutable revision this syncer is frozen at, and whether it is
// pinned at all. An unpinned syncer tracks branch().
func (s *Syncer) pin() (plumbing.Revision, bool) {
	if strings.HasPrefix(s.Branch, tagRefPrefix) || isObjectID(s.Branch) {
		return plumbing.Revision(s.Branch), true
	}
	return "", false
}

// cloneOptions selects what the initial clone fetches. A branch and a tag are both
// refs, so both stay the cheap single-ref clone this syncer has always done. A bare
// commit id is not a ref and cannot be a clone target at all, so that case clones
// the repository's refs and checkoutPin moves HEAD onto the commit afterwards.
func (s *Syncer) cloneOptions(auth *githttp.BasicAuth) *git.CloneOptions {
	o := &git.CloneOptions{URL: s.URL, Auth: auth}
	rev, pinned := s.pin()
	switch {
	case !pinned:
		o.ReferenceName = plumbing.NewBranchReferenceName(s.branch())
		o.SingleBranch = true
	case strings.HasPrefix(string(rev), tagRefPrefix):
		o.ReferenceName = plumbing.ReferenceName(rev)
		o.SingleBranch = true
	}
	return o
}

// checkoutPin puts the mirror on the pinned revision, fetching ONLY when the mirror
// has never seen it — a fresh commit-id pin, or an operator who reviewed upstream,
// bumped the pin and restarted onto the checkout they already had.
//
// A pin cannot move, so the steady state (the mirror is already there) costs one
// local resolve and no network at all. That is what keeps a periodic poll on a
// pinned corpus free rather than re-cloning on every tick; the poll is kept, not
// skipped, because it is also what retries a failed re-index and re-materialises a
// checkout that went missing.
//
// A pin the repository does not contain is an error, never a silent fallback: the
// operator named a revision and must not end up quietly indexing a different one.
func (s *Syncer) checkoutPin(ctx context.Context, repo *git.Repository, auth *githttp.BasicAuth, rev plumbing.Revision) error {
	hash, err := repo.ResolveRevision(rev)
	if err != nil {
		ferr := repo.FetchContext(ctx, &git.FetchOptions{
			Auth:     auth,
			RefSpecs: pinRefSpecs,
			Tags:     git.AllTags,
			Force:    true,
		})
		if ferr != nil && !errors.Is(ferr, git.NoErrAlreadyUpToDate) {
			return fmt.Errorf("fetch for pinned revision %s: %w", rev, ferr)
		}
		if hash, err = repo.ResolveRevision(rev); err != nil {
			return fmt.Errorf("pinned revision %s not found in %s: %w", rev, s.URL, err)
		}
	}
	// ResolveRevision (rather than a raw ref lookup) peels an annotated tag, so hash
	// is a COMMIT and comparable to HEAD, which always is one. Checkout would peel a
	// tag object on its own; what the peel buys is that the comparison below can
	// actually short-circuit, instead of re-checking out the same tree every poll.
	if head, herr := repo.Head(); herr == nil && head.Hash() == *hash {
		return nil
	}
	wt, err := repo.Worktree()
	if err != nil {
		return err
	}
	if err := wt.Checkout(&git.CheckoutOptions{Hash: *hash, Force: true}); err != nil {
		return fmt.Errorf("checkout pinned revision %s: %w", rev, err)
	}
	return nil
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

// Sync clones the repo if the mirror is absent, otherwise brings it to the wanted
// revision, and reports whether HEAD moved since the previous sync (true on the
// first sync). The returned *SyncDelta names the changed/removed paths between the
// previous and new revision; it is nil ("unknown") on the first sync or any diff
// error, which the caller must treat as a full reload.
//
// A pinned Syncer (see Branch) differs in exactly one place — how an existing
// mirror is advanced. Clone, corrupt-mirror recovery and the HEAD-gated change
// detection below are shared, deliberately: pinning changes which revision the
// mirror lands on, not how the syncer decides something moved. A pin therefore
// reports changed=true once, then changed=false forever, and Run's re-index gate
// does the rest with no special case of its own.
func (s *Syncer) Sync(ctx context.Context) (bool, *SyncDelta, error) {
	auth, err := s.auth(ctx)
	if err != nil {
		return false, nil, fmt.Errorf("auth: %w", err)
	}
	pinRev, pinned := s.pin()
	// advance brings an EXISTING mirror to the wanted revision: a pull for a tracked
	// branch, a (normally network-free) checkout for a pin.
	advance := func(repo *git.Repository) error {
		if pinned {
			return s.checkoutPin(ctx, repo, auth, pinRev)
		}
		wt, werr := repo.Worktree()
		if werr != nil {
			return werr
		}
		perr := wt.PullContext(ctx, &git.PullOptions{
			ReferenceName: plumbing.NewBranchReferenceName(s.branch()),
			SingleBranch:  true,
			Auth:          auth,
			Force:         true,
		})
		if perr != nil && !errors.Is(perr, git.NoErrAlreadyUpToDate) {
			return perr
		}
		return nil
	}
	clone := func() (*git.Repository, error) {
		repo, cerr := git.PlainCloneContext(ctx, s.Dir, false, s.cloneOptions(auth))
		if cerr == nil && pinned {
			// A tag clone already landed on the pin and this is a no-op; a commit-id
			// clone landed on the default branch and still has to move.
			cerr = s.checkoutPin(ctx, repo, auth, pinRev)
		}
		if cerr != nil {
			// Drop the partial checkout so an interrupted/failed clone can't leave a
			// half-written .git that wedges every future sync — the next tick re-clones.
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
	} else if err = advance(repo); err != nil {
		return false, nil, err
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
