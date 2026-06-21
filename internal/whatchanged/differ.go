// Package whatchanged produces the "what changed" delta between two GitOps
// revisions: a path-scoped unified diff (the actual landed change). It is the
// differentiating core of RunLore's investigation spine.
package whatchanged

import (
	"bytes"
	"fmt"
	"os"
	"strings"

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
	// Token is a GitHub App installation token used for HTTPS clone auth in
	// Remote. Empty disables auth (e.g. public or local repos).
	Token string
}

// Local diffs two revisions in an already-cloned repository at path.
func (d *Differ) Local(path, fromRev, toRev, scope string) (providers.Diff, error) {
	repo, err := git.PlainOpen(path)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("open %s: %w", path, err)
	}
	return diffRevisions(repo, fromRev, toRev, scope)
}

// diffRevisions resolves two revisions and returns their path-scoped diff.
func diffRevisions(repo *git.Repository, fromRev, toRev, scope string) (providers.Diff, error) {
	from, err := resolveCommit(repo, fromRev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", fromRev, err)
	}
	to, err := resolveCommit(repo, toRev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", toRev, err)
	}
	return diffCommits(from, to, scope)
}

// diffCommits returns the path-scoped unified diff between two commits.
// scope is a path prefix matched on segment boundaries; "" includes every file.
func diffCommits(from, to *object.Commit, scope string) (providers.Diff, error) {
	patch, err := from.Patch(to)
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

// auth builds the clone auth method from the installation token.
func (d *Differ) auth() transport.AuthMethod {
	if d.Token == "" {
		return nil
	}
	return &http.BasicAuth{Username: "x-access-token", Password: d.Token}
}

// cloneToDisk clones url into a temporary on-disk repository and returns it with a
// cleanup func. Cloning to disk — NOT memory.NewStorage — bounds heap to the
// working set: an in-memory clone holds the entire object store in the heap, which
// for a large monorepo reached ~1.3GB and OOM-killed the agent (observed via the
// inuse_space heap profile: go-git MemoryObject.Write = 90% of heap).
func (d *Differ) cloneToDisk(url string) (*git.Repository, func(), error) {
	dir, err := os.MkdirTemp("", "runlore-clone-")
	if err != nil {
		return nil, func() {}, fmt.Errorf("temp dir: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	repo, err := git.PlainClone(dir, false, &git.CloneOptions{URL: url, Auth: d.auth()})
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("clone %s: %w", url, err)
	}
	return repo, cleanup, nil
}

// Remote clones url to disk (auth via the installation token when set) and diffs
// two revisions. The source may be a remote HTTPS URL or a local path.
func (d *Differ) Remote(url, fromRev, toRev, scope string) (providers.Diff, error) {
	repo, cleanup, err := d.cloneToDisk(url)
	if err != nil {
		return providers.Diff{}, err
	}
	defer cleanup()
	return diffRevisions(repo, fromRev, toRev, scope)
}

// RemoteFromParent clones url and returns the path-scoped diff of the change
// introduced by rev (rev against its first parent). A root commit (no parent)
// yields an empty diff.
//
// NOTE (perf): does a full (disk) clone per call. When the GitOpsProvider drives
// this across many changes, add a per-repo clone cache here (see docs/plans note).
func (d *Differ) RemoteFromParent(url, rev, scope string) (providers.Diff, error) {
	repo, cleanup, err := d.cloneToDisk(url)
	if err != nil {
		return providers.Diff{}, err
	}
	defer cleanup()
	to, err := resolveCommit(repo, rev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", rev, err)
	}
	if to.NumParents() == 0 {
		return providers.Diff{}, nil // root commit: nothing to diff against
	}
	from, err := to.Parent(0)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("parent of %q: %w", rev, err)
	}
	return diffCommits(from, to, scope)
}

// ForChange resolves the diff for a detected Change by cloning its source repo
// and scoping to the workload's path. With both revisions it diffs FromRev..ToRev;
// with only ToRev it diffs the change introduced by ToRev (against its parent).
func (d *Differ) ForChange(c providers.Change) (providers.Diff, error) {
	if c.ToRev == "" {
		return providers.Diff{}, fmt.Errorf("change %s/%s: missing to revision", c.Workload.Namespace, c.Workload.Name)
	}
	if c.FromRev == "" {
		return d.RemoteFromParent(c.Source.RepoURL, c.ToRev, c.Source.Path)
	}
	return d.Remote(c.Source.RepoURL, c.FromRev, c.ToRev, c.Source.Path)
}
