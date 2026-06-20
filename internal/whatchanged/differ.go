// Package whatchanged produces the "what changed" delta between two GitOps
// revisions: a path-scoped unified diff (the actual landed change). It is the
// differentiating core of RunLore's investigation spine.
package whatchanged

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/transport"
	"github.com/go-git/go-git/v5/plumbing/transport/http"
	"github.com/go-git/go-git/v5/storage/memory"

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

// diffRevisions returns the path-scoped unified diff between two revisions.
// scope is a path prefix; "" includes every changed file.
func diffRevisions(repo *git.Repository, fromRev, toRev, scope string) (providers.Diff, error) {
	from, err := resolveCommit(repo, fromRev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", fromRev, err)
	}
	to, err := resolveCommit(repo, toRev)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("resolve %q: %w", toRev, err)
	}
	patch, err := from.Patch(to)
	if err != nil {
		return providers.Diff{}, fmt.Errorf("patch: %w", err)
	}

	var out providers.Diff
	for _, fp := range patch.FilePatches() {
		path := filePatchPath(fp)
		if scope != "" && !strings.HasPrefix(path, scope) {
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

// Remote clones url into memory (auth via the installation token when set) and
// diffs two revisions. The source may be a remote HTTPS URL or a local path.
func (d *Differ) Remote(url, fromRev, toRev, scope string) (providers.Diff, error) {
	repo, err := git.Clone(memory.NewStorage(), nil, &git.CloneOptions{
		URL:  url,
		Auth: d.auth(),
	})
	if err != nil {
		return providers.Diff{}, fmt.Errorf("clone %s: %w", url, err)
	}
	return diffRevisions(repo, fromRev, toRev, scope)
}

// ForChange resolves the diff for a detected Change, cloning its source repo and
// scoping to the workload's path. This is the integration point a GitOpsProvider
// uses to fill in a Change's diff.
func (d *Differ) ForChange(c providers.Change) (providers.Diff, error) {
	if c.FromRev == "" || c.ToRev == "" {
		return providers.Diff{}, fmt.Errorf("change %s/%s: missing from/to revision", c.Workload.Namespace, c.Workload.Name)
	}
	return d.Remote(c.Source.RepoURL, c.FromRev, c.ToRev, c.Source.Path)
}
