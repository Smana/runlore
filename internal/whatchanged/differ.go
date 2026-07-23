// SPDX-License-Identifier: Apache-2.0

// Package whatchanged produces the "what changed" delta between two GitOps
// revisions: a path-scoped unified diff (the actual landed change). It is the
// differentiating core of RunLore's investigation spine.
package whatchanged

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"

	"github.com/Smana/runlore/internal/providers"
)

// providersFileDiff aliases the contract type for readable call sites/tests.
type providersFileDiff = providers.FileDiff

// Differ computes path-scoped diffs between Git revisions.
type Differ struct {
	// TokenSource mints a GitHub App installation token for HTTPS clone auth in
	// Remote/RemoteFromParent. It is called once per clone so a short-lived
	// (~1h) installation token stays fresh across a long-running agent. nil
	// disables auth (e.g. public or local repos).
	TokenSource func(context.Context) (string, error)
	// TokenHost, when non-empty, confines the TokenSource credential to clones of
	// that exact host — the GitHub App installation token is only valid for (and
	// must only be sent to) the GitHub instance the App is installed on. A clone
	// of any other host proceeds unauthenticated so a github.com token can never
	// leak to, say, gitlab.com. Empty (the default, used by what_changed on the
	// operator's own single-host GitOps repo) attaches the token to every clone,
	// preserving the original behavior. source_diff sets this because its clone
	// URLs are model-chosen across the whole allowlist.
	TokenHost string
	// Mirrors, when set, backs clones with a persistent per-repo bare mirror
	// (incremental fetch, shared across investigations). nil ⇒ full clone per
	// call, exactly as before. Mirror errors fall back to clone-per-call.
	Mirrors *MirrorCache
}

// Local diffs two revisions in an already-cloned repository at path. ctx bounds the
// (potentially expensive) patch computation so a caller deadline is honored.
func (d *Differ) Local(ctx context.Context, path, fromRev, toRev, scope string) (providers.Diff, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("open %s: %w", path, err)
	}
	return diffRevisions(ctx, repo, fromRev, toRev, scope)
}

// diffRevisions resolves two revisions and returns their path-scoped diff.
func diffRevisions(ctx context.Context, repo *git.Repository, fromRev, toRev, scope string) (providers.Diff, error) {
	from, err := resolveCommit(repo, fromRev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", fromRev, err)
	}
	to, err := resolveCommit(repo, toRev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", toRev, err)
	}
	return diffCommits(ctx, from, to, scope)
}

// diffCommits returns the path-scoped unified diff between two commits.
// scope is a path prefix matched on segment boundaries; "" includes every file.
// ctx cancels the diff computation (a large-tree Patch can be slow).
func diffCommits(ctx context.Context, from, to *object.Commit, scope string) (providers.Diff, error) {
	patch, err := from.PatchContext(ctx, to)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("patch: %w", err)
	}
	var out providers.Diff
	for _, fp := range patch.FilePatches() {
		path := filePatchPath(fp)
		if !underScope(path, scope) {
			continue
		}
		var buf bytes.Buffer
		if err := diff.NewUnifiedEncoder(&buf, diff.DefaultContextLines).Encode(singleFilePatch{fp}); err != nil {
			return providers.Diff{}, fmt.Errorf("encode %s: %w", path, err)
		}
		out.Files = append(out.Files, providers.FileDiff{Path: path, Patch: buf.String()})
	}
	return out, nil
}

// underScope reports whether path is within scope, matching on path-segment
// boundaries so "apps/harbor" does not also match a sibling like
// "apps/harbor-staging". Empty scope matches everything.
func underScope(path, scope string) bool {
	if scope == "" {
		return true
	}
	scope = strings.TrimSuffix(scope, "/")
	return path == scope || strings.HasPrefix(path, scope+"/")
}

func resolveCommit(repo *git.Repository, rev string) (*object.Commit, error) {
	h, err := repo.ResolveRevision(plumbing.Revision(rev))
	if err != nil {
		return nil, err
	}
	return repo.CommitObject(*h)
}

// filePatchPath returns the post-change path (or pre-change path for deletions).
func filePatchPath(fp diff.FilePatch) string {
	from, to := fp.Files()
	if to != nil {
		return to.Path()
	}
	if from != nil {
		return from.Path()
	}
	return ""
}

// singleFilePatch adapts one FilePatch to the diff.Patch interface so a single
// file can be rendered on its own.
type singleFilePatch struct{ fp diff.FilePatch }

func (p singleFilePatch) FilePatches() []diff.FilePatch { return []diff.FilePatch{p.fp} }
func (p singleFilePatch) Message() string               { return "" }

// auth builds the clone auth method for cloneURL from a freshly-minted
// installation token. Returns (nil, nil) when no token source is configured, it
// yields an empty token (public/local repos), or TokenHost is set and cloneURL's
// host does not match it — in which case the clone proceeds unauthenticated so
// the GitHub App token is never transmitted to a foreign host. A token-source
// error is surfaced so a private same-host clone fails loudly instead of
// silently attempting unauthenticated access.
func (d *Differ) auth(ctx context.Context, cloneURL string) (transport.AuthMethod, error) {
	if d.TokenSource == nil {
		return nil, nil
	}
	if d.TokenHost != "" && hostOf(cloneURL) != d.TokenHost {
		return nil, nil // token is scoped to TokenHost; do not leak it elsewhere
	}
	tok, err := d.TokenSource(ctx)
	if err != nil {
		return nil, fmt.Errorf("clone auth token: %w", err)
	}
	if tok == "" {
		return nil, nil
	}
	return &http.BasicAuth{Username: "x-access-token", Password: tok}, nil
}

// cloneToDisk clones url into a temporary on-disk repository and returns it with a
// cleanup func. Cloning to disk — NOT memory.NewStorage — bounds heap to the
// working set: an in-memory clone holds the entire object store in the heap, which
// for a large monorepo reached ~1.3GB and OOM-killed the agent (observed via the
// inuse_space heap profile: go-git MemoryObject.Write = 90% of heap). ctx aborts a
// hung remote (a stalled HTTPS clone would otherwise block the queue worker
// indefinitely); a cancelled clone returns a context error via %w.
//
// When ctx carries a clone cache (see WithCloneCache), a repo is cloned at most once
// per batch: a cache hit returns the shared clone with a no-op cleanup (the cache
// owns the temp dir and removes it on close). Without a cache, the caller owns the
// returned cleanup, as before.
func (d *Differ) cloneToDisk(ctx context.Context, url string) (*git.Repository, func(), error) {
	noop := func() {}
	cc := cacheFrom(ctx)
	if cc != nil {
		if repo, ok := cc.get(url); ok {
			return repo, noop, nil // reuse — the cache owns cleanup
		}
	}
	auth, err := d.auth(ctx, url)
	if err != nil {
		return nil, noop, err
	}
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
	dir, err := os.MkdirTemp("", "runlore-clone-")
	if err != nil {
		return nil, noop, fmt.Errorf("temp dir: %w", err)
	}
	repo, err := git.PlainCloneContext(ctx, dir, false, &git.CloneOptions{URL: url, Auth: auth, NoCheckout: true})
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, noop, fmt.Errorf("clone %s: %w", url, err)
	}
	if cc != nil {
		winner, kept := cc.put(url, repo, dir)
		if !kept {
			_ = os.RemoveAll(dir) // a concurrent diff cloned the same url first
		}
		return winner, noop, nil // cache owns cleanup
	}
	return repo, func() { _ = os.RemoveAll(dir) }, nil
}

// hostOf returns the lowercased host of an https/http/ssh clone URL, or "" for
// a local path or an unparseable/hostless URL. Used by auth to confine a
// host-scoped token; a "" host never equals a non-empty TokenHost, so a local
// clone is always treated as off-host (correct — local repos need no token).
func hostOf(cloneURL string) string {
	u, err := url.Parse(cloneURL)
	if err != nil {
		return ""
	}
	return strings.ToLower(u.Hostname())
}

// Remote clones url to disk (auth via the installation token when set) and diffs
// two revisions. The source may be a remote HTTPS URL or a local path. ctx bounds
// the clone + patch.
func (d *Differ) Remote(ctx context.Context, url, fromRev, toRev, scope string) (providers.Diff, error) {
	repo, cleanup, err := d.cloneToDisk(ctx, url)
	if err != nil {
		return providers.Diff{}, err
	}
	defer cleanup()
	return diffRevisions(ctx, repo, fromRev, toRev, scope)
}

// diffAgainstFirstParent returns the path-scoped diff of the change introduced by
// commit to (to against its first parent). A root commit (no parent) yields an
// empty diff.
func diffAgainstFirstParent(ctx context.Context, to *object.Commit, scope string) (providers.Diff, error) {
	if to.NumParents() == 0 {
		return providers.Diff{}, nil // root commit: nothing to diff against
	}
	from, err := to.Parent(0)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("parent of %s: %w", to.Hash, err)
	}
	return diffCommits(ctx, from, to, scope)
}

// RemoteFromParent clones url and returns the path-scoped diff of the change
// introduced by rev (rev against its first parent). A root commit (no parent)
// yields an empty diff. ctx bounds the clone + patch.
func (d *Differ) RemoteFromParent(ctx context.Context, url, rev, scope string) (providers.Diff, error) {
	repo, cleanup, err := d.cloneToDisk(ctx, url)
	if err != nil {
		return providers.Diff{}, err
	}
	defer cleanup()
	to, err := resolveCommit(repo, rev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", rev, err)
	}
	return diffAgainstFirstParent(ctx, to, scope)
}

// ForChange resolves the diff for a detected Change by cloning its source repo
// and scoping to the workload's path. With both revisions it diffs FromRev..ToRev;
// with only ToRev it diffs the change introduced by ToRev (against its parent).
// ctx bounds the clone + patch so a caller deadline (per-investigation timeout)
// aborts a hung remote.
//
// When the forward range holds no change to the resource's path it falls back to
// the newest commit that actually touched the path. This matters on a Flux
// health-check failure: Flux applies the manifest (advancing lastAppliedRevision to
// the breaking commit) and only then fails the health gate, so the change is at/behind
// the applied revision — diffing forward from it misses it entirely (RunLore #239).
func (d *Differ) ForChange(ctx context.Context, c providers.Change) (providers.Diff, error) {
	if c.ToRev == "" {
		return providers.Diff{}, fmt.Errorf("change %s/%s: missing to revision", c.Workload.Namespace, c.Workload.Name)
	}
	var (
		diff providers.Diff
		err  error
	)
	if c.FromRev == "" {
		diff, err = d.RemoteFromParent(ctx, c.Source.RepoURL, c.ToRev, c.Source.Path)
	} else {
		diff, err = d.Remote(ctx, c.Source.RepoURL, c.FromRev, c.ToRev, c.Source.Path)
	}
	if err != nil {
		return diff, err
	}
	// RemoteLastPathChange is a no-op for an empty path, so no guard needed here.
	if len(diff.Files) == 0 {
		if fb, ferr := d.RemoteLastPathChange(ctx, c.Source.RepoURL, c.ToRev, c.Source.Path); ferr == nil && len(fb.Files) > 0 {
			return fb, nil
		}
	}
	return diff, nil
}

// CommitTime clones url (via the batch clone cache when present) and returns the
// committer timestamp of rev. This is the time the change actually landed in Git —
// the anchor for aligning a GitOps Change against kube_events / pod-log timestamps
// (RunLore B1: Change.When was never populated for Flux). A zero time + error is
// returned when rev can't be resolved; callers fall back to a status timestamp.
func (d *Differ) CommitTime(ctx context.Context, url, rev string) (time.Time, error) {
	if rev == "" {
		return time.Time{}, errors.New("commit time: empty revision")
	}
	repo, cleanup, err := d.cloneToDisk(ctx, url)
	if err != nil {
		return time.Time{}, err
	}
	defer cleanup()
	c, err := resolveCommit(repo, rev)
	if err != nil {
		return time.Time{}, fmt.Errorf("resolve %q: %w", rev, err)
	}
	return c.Committer.When, nil
}

// RemoteLastPathChange clones url and returns the path-scoped diff of the newest
// commit reachable from atRev that actually modified scope, against its first parent.
// It is ForChange's fallback when the forward range holds no change to scope (see
// RunLore #239). Returns an empty diff when scope is empty, no ancestor of atRev
// touches scope, or the touching commit is a root commit (no parent).
func (d *Differ) RemoteLastPathChange(ctx context.Context, url, atRev, scope string) (providers.Diff, error) {
	if scope == "" {
		return providers.Diff{}, nil
	}
	repo, cleanup, err := d.cloneToDisk(ctx, url)
	if err != nil {
		return providers.Diff{}, err
	}
	defer cleanup()
	return lastPathChange(ctx, repo, atRev, scope)
}

// Revision is one in-window commit on a source repo: its SHA and committer time.
// It is the unit RevisionsInWindow returns, letting a GitOpsProvider emit one Change
// per in-window revision instead of only the current applied/synced one (RunLore G3).
type Revision struct {
	SHA  string
	When time.Time
}

// MaxWindowRevisions caps how many in-window revisions a single GitOps object
// (Flux Kustomization / Argo CD Application) emits (G3). It bounds the "what changed"
// output so a wide window on a busy monorepo can't flood the model with hundreds of
// Changes. Shared by the flux and argocd providers, which pass it to RevisionsInWindow.
const MaxWindowRevisions = 10

// RevisionsInWindow clones url and returns the commits reachable from atRev whose
// committer time falls within w, optionally scoped to a path, newest-first and
// capped at max. It is what lets Changes honor a TimeWindow: instead of surfacing
// only the current applied/synced revision, a provider can enumerate every revision
// that landed in the window and emit a Change per one.
//
// Bounded on purpose (G3 is SAFETY MEDIUM — it changes what "what changed" surfaces
// to the model): the walk stops once max revisions are collected OR once history
// predates w.Start, so a wide window on a busy monorepo can't explode the output.
// A zero-valued window (w.Start.IsZero() && w.End.IsZero()) returns nil so callers
// fall back to their single-revision behavior. max<=0 also returns nil.
func (d *Differ) RevisionsInWindow(ctx context.Context, url, atRev, scope string, w providers.TimeWindow, maxRevs int) ([]Revision, error) {
	if maxRevs <= 0 || (w.Start.IsZero() && w.End.IsZero()) {
		return nil, nil
	}
	repo, cleanup, err := d.cloneToDisk(ctx, url)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	return revisionsInWindow(repo, atRev, scope, w, maxRevs)
}

// revisionsInWindow walks history from atRev newest-first (committer-time order),
// keeping commits within [w.Start, w.End] until max are collected or history predates
// the window. Because the walk is committer-time ordered, once a commit is older than
// w.Start no later one can be in-window, so the walk short-circuits — keeping it cheap.
func revisionsInWindow(repo *git.Repository, atRev, scope string, w providers.TimeWindow, maxRevs int) ([]Revision, error) {
	from, err := resolveCommit(repo, atRev)
	if err != nil {
		return nil, fmt.Errorf("resolve %q: %w", atRev, err)
	}
	opts := &git.LogOptions{From: from.Hash, Order: git.LogOrderCommitterTime}
	if scope != "" {
		opts.PathFilter = func(p string) bool { return underScope(p, scope) }
	}
	iter, err := repo.Log(opts)
	if err != nil {
		return nil, fmt.Errorf("log %s: %w", scope, err)
	}
	defer iter.Close()
	var out []Revision
	for len(out) < maxRevs {
		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return out, fmt.Errorf("log %s: %w", scope, err)
		}
		t := c.Committer.When
		if !w.Start.IsZero() && t.Before(w.Start) {
			break // committer-time ordered: everything older is also out of window
		}
		if !w.End.IsZero() && t.After(w.End) {
			continue // newer than the window (rare with clock skew) — skip, keep walking
		}
		out = append(out, Revision{SHA: c.Hash.String(), When: t})
	}
	return out, nil
}

// lastPathChange walks history from atRev and returns the path-scoped diff of the
// newest commit that modified scope, against its first parent.
func lastPathChange(ctx context.Context, repo *git.Repository, atRev, scope string) (providers.Diff, error) {
	from, err := resolveCommit(repo, atRev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", atRev, err)
	}
	iter, err := repo.Log(&git.LogOptions{
		From:       from.Hash,
		Order:      git.LogOrderCommitterTime,
		PathFilter: func(p string) bool { return underScope(p, scope) },
	})
	if err != nil {
		return providers.Diff{}, fmt.Errorf("log %s: %w", scope, err)
	}
	defer iter.Close()
	to, err := iter.Next()
	if errors.Is(err, io.EOF) {
		return providers.Diff{}, nil // nothing in history touched scope
	}
	if err != nil {
		return providers.Diff{}, fmt.Errorf("log %s: %w", scope, err)
	}
	return diffAgainstFirstParent(ctx, to, scope)
}
