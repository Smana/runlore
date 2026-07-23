// SPDX-License-Identifier: Apache-2.0

package whatchanged

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/format/diff"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/Smana/runlore/internal/providers"
)

// SourceCommit is one commit in a source-repo range: enough for the model to
// spot the offending change (SHA + subject + committer time).
type SourceCommit struct {
	SHA     string
	Subject string
	When    time.Time
}

// SourceChanges is the source_diff payload: the ref spellings that actually
// resolved (after the v-prefix fallback), the commits reachable from ToRef
// down to FromRef (newest-first, capped), and the (bounded) unscoped diff
// between the two refs.
type SourceChanges struct {
	FromRef, ToRef string
	Commits        []SourceCommit
	CommitsCapped  bool // the walk hit maxCommits before reaching FromRef
	Diff           providers.Diff
	FilesOmitted   int // files dropped because the diff hit the file/byte cap (0 = whole diff materialized)
}

// nearTagLimit caps how many candidate tags a RefNotFoundError lists.
const nearTagLimit = 8

const (
	// sourceMaxDiffFiles / sourceMaxDiffBytes bound what Source materializes into
	// memory. Unlike what_changed (which scopes the diff to a workload path),
	// source_diff diffs the WHOLE repo, so a large monorepo release could render
	// hundreds of MB of unified-diff strings and OOM the agent (the failure mode
	// differ.go's clone-to-disk note records). These caps hold retention to a few
	// MB regardless of repo size; the render layer trims further. Files past the
	// cap are counted into FilesOmitted and their patches dropped — realistic
	// releases (well under 2000 changed files) never trip this.
	sourceMaxDiffFiles = 2000
	sourceMaxDiffBytes = 8 << 20
)

// RefNotFoundError reports an unresolvable ref plus nearby tag names so the
// model can self-correct (asked for "1.2.3", the repo tags "v1.2.3" — or the
// model guessed the wrong repo entirely, in which case no tag will look right).
type RefNotFoundError struct {
	Ref  string
	Tags []string
}

func (e *RefNotFoundError) Error() string {
	if len(e.Tags) == 0 {
		return fmt.Sprintf("ref %q not found (and the repo has no tags — wrong repo?)", e.Ref)
	}
	return fmt.Sprintf("ref %q not found; nearby tags: %s", e.Ref, strings.Join(e.Tags, ", "))
}

// Source diffs a source repo between two refs: resolve each (tags or SHAs,
// with a v-prefix fallback — image tag "1.2.3" vs git tag "v1.2.3"), walk the
// commit range newest-first capped at maxCommits, and compute the full
// unscoped diff. One clone serves all three (mirror-backed when configured).
func (d *Differ) Source(ctx context.Context, url, fromRef, toRef string, maxCommits int) (SourceChanges, error) {
	repo, cleanup, err := d.cloneToDisk(ctx, url)
	if err != nil {
		return SourceChanges{}, err
	}
	defer cleanup()
	from, fromRes, err := resolveWithVFallback(repo, fromRef)
	if err != nil {
		return SourceChanges{}, err
	}
	to, toRes, err := resolveWithVFallback(repo, toRef)
	if err != nil {
		return SourceChanges{}, err
	}
	out := SourceChanges{FromRef: fromRes, ToRef: toRes}
	if out.Commits, out.CommitsCapped, err = commitRange(repo, to, from.Hash, maxCommits); err != nil {
		return SourceChanges{}, err
	}
	if out.Diff, out.FilesOmitted, err = cappedDiff(ctx, from, to); err != nil {
		return SourceChanges{}, err
	}
	return out, nil
}

// cappedDiff computes the whole-repo unified diff between two commits, bounded
// by sourceMaxDiffFiles and sourceMaxDiffBytes so a pathological monorepo diff
// can't exhaust memory. Files rendered before a cap trips carry full patches;
// the rest are counted and returned as omitted (their patches dropped). It
// mirrors diffCommits' per-file encoding but adds the caps — kept separate so
// what_changed's path-scoped diffs stay uncapped.
func cappedDiff(ctx context.Context, from, to *object.Commit) (providers.Diff, int, error) {
	patch, err := from.PatchContext(ctx, to)
	if err != nil {
		return providers.Diff{}, 0, fmt.Errorf("patch: %w", err)
	}
	var (
		out     providers.Diff
		total   int
		omitted int
	)
	for _, fp := range patch.FilePatches() {
		if len(out.Files) >= sourceMaxDiffFiles || total >= sourceMaxDiffBytes {
			omitted++
			continue
		}
		var buf bytes.Buffer
		if err := diff.NewUnifiedEncoder(&buf, diff.DefaultContextLines).Encode(singleFilePatch{fp}); err != nil {
			return providers.Diff{}, 0, fmt.Errorf("encode %s: %w", filePatchPath(fp), err)
		}
		out.Files = append(out.Files, providers.FileDiff{Path: filePatchPath(fp), Patch: buf.String()})
		total += buf.Len()
	}
	return out, omitted, nil
}

// resolveWithVFallback resolves ref, then "v"+ref (the image-tag/git-tag
// mismatch), returning the commit and the spelling that worked. Failure is a
// *RefNotFoundError carrying nearby tags for model self-correction.
func resolveWithVFallback(repo *git.Repository, ref string) (*object.Commit, string, error) {
	if c, err := resolveCommit(repo, ref); err == nil {
		return c, ref, nil
	}
	if !strings.HasPrefix(ref, "v") {
		if c, err := resolveCommit(repo, "v"+ref); err == nil {
			return c, "v" + ref, nil
		}
	}
	return nil, "", &RefNotFoundError{Ref: ref, Tags: nearbyTags(repo, ref)}
}

// commitRange walks history from to (newest-first, committer-time order) and
// collects commits until stop is reached (exclusive) or maxCommits is hit.
// On non-linear history this is "commits reachable from to down to stop", a
// superset of `git log stop..to` on merged side branches — acceptable for the
// model's purpose (spotting the offending commit) and always capped.
func commitRange(repo *git.Repository, to *object.Commit, stop plumbing.Hash, maxCommits int) ([]SourceCommit, bool, error) {
	iter, err := repo.Log(&git.LogOptions{From: to.Hash, Order: git.LogOrderCommitterTime})
	if err != nil {
		return nil, false, fmt.Errorf("log: %w", err)
	}
	defer iter.Close()
	var out []SourceCommit
	for {
		c, err := iter.Next()
		if errors.Is(err, io.EOF) {
			return out, false, nil
		}
		if err != nil {
			return out, false, fmt.Errorf("log: %w", err)
		}
		if c.Hash == stop {
			return out, false, nil
		}
		if len(out) == maxCommits {
			return out, true, nil
		}
		subject := c.Message
		if i := strings.IndexByte(subject, '\n'); i >= 0 {
			subject = subject[:i]
		}
		out = append(out, SourceCommit{SHA: c.Hash.String(), Subject: strings.TrimSpace(subject), When: c.Committer.When})
	}
}

// nearbyTags returns up to nearTagLimit tag names related to ref (substring
// match on the version, "v" stripped), falling back to the lexically-last
// tags (usually the newest semver) when nothing matches.
func nearbyTags(repo *git.Repository, ref string) []string {
	iter, err := repo.Tags()
	if err != nil {
		return nil
	}
	defer iter.Close()
	var all []string
	_ = iter.ForEach(func(r *plumbing.Reference) error {
		all = append(all, r.Name().Short())
		return nil
	})
	sort.Strings(all)
	needle := strings.ToLower(strings.TrimPrefix(ref, "v"))
	var near []string
	if needle != "" {
		for _, tag := range all {
			if strings.Contains(strings.ToLower(tag), needle) {
				near = append(near, tag)
			}
		}
	}
	if len(near) == 0 {
		if len(all) > nearTagLimit {
			all = all[len(all)-nearTagLimit:]
		}
		return all
	}
	if len(near) > nearTagLimit {
		near = near[:nearTagLimit]
	}
	return near
}
