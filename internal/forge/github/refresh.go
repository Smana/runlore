// SPDX-License-Identifier: Apache-2.0

package github

// This file rebuilds a conflicting entry PR on the current base (KP19). Every entry PR
// inserts at the same place in log.md and often index.md, and nothing rebases them, so
// merging one makes the next conflict.
//
// It is RunLore's one force-push, so its limits are stated here and checked here rather
// than trusted to the caller: an open PR, whose head is in this repository, on an entry
// branch, still at the head the caller examined. The caller (curate.RefreshEntries)
// decides whose commits those are from EntryPRState, and passing that state's HeadSHA
// here is what ties the decision to the commits it was made about.

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

// kbBranchPattern is what OpenPR makes: the prefix, a slug and a timestamp. A rebuild moves nothing
// else. A branch name goes unescaped into the read's query and the PATCH's path, so a # or ? in it
// would send both to another ref, one nobody examined.
var kbBranchPattern = regexp.MustCompile(`^` + regexp.QuoteMeta(kbBranchPrefix) + `[A-Za-z0-9._-]+$`)

// webFlowLogin is the committer GitHub reports on the commits it writes and signs itself, as it does
// for a Contents API write with the App's token.
const webFlowLogin = "web-flow"

// mergeableRetry is how long EntryPRState waits before its one re-read when GitHub answers
// mergeable: null, which it does while computing the answer. A variable so tests set it to 0.
var mergeableRetry = 2 * time.Second

// EntryPRState reads what deciding on a rebuild takes: the head, whether GitHub calls it
// mergeable, whose commits it is and what it changes. Mergeable stays nil if GitHub is
// still computing after one re-read.
//
// A commit's author login counts only when GitHub wrote and signed the commit (committer
// web-flow, verification verified), and is empty otherwise. An amend, a rebase or a spoofed
// email keeps the bot as author and loses both, and only the signature cannot be set from a
// laptop. Every commit RunLore makes qualifies: OpenPR, EditEntryOnPR and RebuildEntryPR all
// commit through the Contents API (putFile). A human's web edit keeps that human's login.
//
// The head is read FIRST: a push landing during these reads makes the commits newer than
// HeadSHA, and RebuildEntryPR then refuses that HeadSHA.
func (c *Client) EntryPRState(ctx context.Context, number int) (providers.EntryPRState, error) {
	pr, err := c.readPR(ctx, number)
	if err != nil {
		return providers.EntryPRState{}, err
	}
	if pr.mergeable == nil {
		select {
		case <-ctx.Done():
			return providers.EntryPRState{}, ctx.Err()
		case <-time.After(mergeableRetry):
		}
		if pr, err = c.readPR(ctx, number); err != nil {
			return providers.EntryPRState{}, err
		}
	}
	st := providers.EntryPRState{Branch: pr.branch, HeadSHA: pr.headSHA, Mergeable: pr.mergeable, Fork: !c.ownHead(pr)}

	var commits []struct {
		Author    *gitHubAccount `json:"author"`    // null when the commit's email links to no account
		Committer *gitHubAccount `json:"committer"` // likewise
		Commit    struct {
			Verification struct {
				Verified bool `json:"verified"`
			} `json:"verification"`
		} `json:"commit"`
	}
	if err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/commits?per_page=%d", c.owner, c.repo, number, prFilesPerPage), nil, &commits); err != nil {
		return providers.EntryPRState{}, err
	}
	for _, cm := range commits {
		login := ""
		if cm.Author != nil && cm.Committer != nil && cm.Committer.Login == webFlowLogin && cm.Commit.Verification.Verified {
			login = cm.Author.Login
		}
		st.CommitAuthors = append(st.CommitAuthors, login)
	}
	var files []struct {
		Filename string `json:"filename"`
	}
	if err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=%d", c.owner, c.repo, number, prFilesPerPage), nil, &files); err != nil {
		return providers.EntryPRState{}, err
	}
	for _, f := range files {
		st.Files = append(st.Files, f.Filename)
	}
	// A full page says nothing about the commits or paths past it, so it vouches for nothing.
	st.Truncated = len(commits) >= prFilesPerPage || len(files) >= prFilesPerPage
	return st, nil
}

type gitHubAccount struct {
	Login string `json:"login"`
}

// ownHead reports whether the PR's head branch is in this repository. A deleted fork
// reports no repository at all, which is not this one either.
func (c *Client) ownHead(pr prHead) bool {
	return strings.EqualFold(pr.repo, c.owner+"/"+c.repo)
}

// checkRebuildable is the check made on the first read of the PR and again just before the push.
func (c *Client) checkRebuildable(number int, pr prHead, head string) error {
	if pr.state != "open" {
		return fmt.Errorf("github PR %d is %q: %w", number, pr.state, providers.ErrPRNotOpen)
	}
	// head.ref is a bare branch name while the PATCH addresses this repository, so a fork's
	// branch name would force-update a branch of ours.
	if !c.ownHead(pr) {
		return fmt.Errorf("github PR %d: head branch is in %q, not %s/%s (fork); refusing to rebuild", number, pr.repo, c.owner, c.repo)
	}
	if !kbBranchPattern.MatchString(pr.branch) {
		return fmt.Errorf("github PR %d (branch %q): %w", number, pr.branch, providers.ErrNotRunLorePR)
	}
	if pr.headSHA != head {
		return fmt.Errorf("github PR %d: head is %s, not %s: %w", number, pr.headSHA, head, providers.ErrHeadMoved)
	}
	return nil
}

// RebuildEntryPR makes the PR's branch the current base plus the entry file, unchanged,
// plus its index.md and log.md lines: the shape OpenPR makes. head is the PR head the
// caller examined; any other head returns providers.ErrHeadMoved and moves nothing. An
// entry the base already has returns providers.ErrEntryOnBase.
//
// The work happens on a temporary branch, removed on every return, and the PR's ref moves
// only once that branch is whole. The ref is never pointed at the base itself on the way:
// a head with no diff can close the PR.
//
// One window remains, between the second read of the PR and the PATCH: GitHub's REST API
// has no compare-and-swap on a ref, so a push landing in that one round trip would be
// overwritten.
func (c *Client) RebuildEntryPR(ctx context.Context, number int, head string) error {
	pr, err := c.readPR(ctx, number)
	if err != nil {
		return err
	}
	if err := c.checkRebuildable(number, pr, head); err != nil {
		return err
	}
	path, err := c.prEntryPath(ctx, number)
	if err != nil {
		return err
	}
	raw, _, found, err := c.getFile(ctx, path, pr.branch)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("github PR %d: entry %s is not on branch %s", number, path, pr.branch)
	}
	if !okf.HasFrontmatter(raw) {
		return fmt.Errorf("github PR %d: %s on branch %s does not read as a catalog entry; refusing to rebuild it", number, path, pr.branch)
	}
	entry, err := bundleFields(raw)
	if err != nil {
		return fmt.Errorf("github PR %d: %s: %w", number, path, err)
	}
	_, _, onBase, err := c.getFile(ctx, path, c.baseBranch)
	if err != nil {
		return err
	}
	if onBase {
		return fmt.Errorf("github PR %d: %s: %w", number, path, providers.ErrEntryOnBase)
	}

	baseSHA, err := c.branchHead(ctx, c.baseBranch)
	if err != nil {
		return err
	}
	tmp := fmt.Sprintf("runlore/rebuild-%d-%d", number, time.Now().Unix())
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/refs", c.owner, c.repo),
		map[string]any{"ref": "refs/heads/" + tmp, "sha": baseSHA}, nil); err != nil {
		return err
	}
	defer c.deleteBranch(ctx, tmp)

	if err := c.putFile(ctx, path, tmp, "", "runlore: draft KB entry "+entry.Title, raw); err != nil {
		return err
	}
	// Returned, unlike in OpenPR: there it must not lose a new entry, here the PR already
	// exists, and a missing companion would change the shape the merge gate admits.
	if err := c.maintainBundle(ctx, entry, path, tmp); err != nil {
		return err
	}
	rebuilt, err := c.branchHead(ctx, tmp)
	if err != nil {
		return err
	}
	if pr, err = c.readPR(ctx, number); err != nil {
		return err
	}
	if err := c.checkRebuildable(number, pr, head); err != nil {
		return err
	}
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", c.owner, c.repo, pr.branch),
		map[string]any{"sha": rebuilt, "force": true}, nil)
}

// bundleFields reads from an entry's frontmatter what its index.md and log.md lines hold.
func bundleFields(raw []byte) (providers.KBEntry, error) {
	fm, _ := catalog.SplitFrontmatter(raw)
	var meta struct {
		Type        string `yaml:"type"`
		Title       string `yaml:"title"`
		Description string `yaml:"description"`
	}
	if err := yaml.Unmarshal(fm, &meta); err != nil {
		return providers.KBEntry{}, fmt.Errorf("frontmatter: %w", err)
	}
	if meta.Type == "" || meta.Title == "" {
		return providers.KBEntry{}, fmt.Errorf("frontmatter has no type or title for the index and log lines")
	}
	return providers.KBEntry{Type: meta.Type, Title: meta.Title, Description: meta.Description}, nil
}

// branchHead reads a branch's head commit.
func (c *Client) branchHead(ctx context.Context, branch string) (string, error) {
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", c.owner, c.repo, branch), nil, &ref); err != nil {
		return "", err
	}
	return ref.Object.SHA, nil
}

// deleteBranch removes a temporary branch, best-effort: the rebuild's outcome does not depend on
// it. Nothing sweeps these branches, so a failure names the branch for someone to delete. It
// outlives a cancelled ctx, which is one of the ways a rebuild ends early.
func (c *Client) deleteBranch(ctx context.Context, branch string) {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	if err := c.do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/%s/git/refs/heads/%s", c.owner, c.repo, branch), nil, nil); err != nil {
		slog.Warn("github: a temporary rebuild branch was left behind; delete it by hand", "branch", branch, "err", err)
	}
}
