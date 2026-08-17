// SPDX-License-Identifier: Apache-2.0

package gitlab

// This file mirrors internal/forge/github/noteappend.go: one further commit
// onto a merge request RunLore itself opened, appending an operator note to the
// KB entry that merge request carries — because a comment lives in the forge's
// conversation, which the catalog never indexes, and only the entry becomes
// knowledge when the request merges.
//
// "Mirrors" is a claim about the SAFETY PROPERTIES, not only about the shape.
// Every guard the GitHub sibling has, this has; where GitLab spells one
// differently the difference is stated at the guard rather than left for a
// reader to infer from its absence — see last_commit_id below, which an earlier
// version of this file omitted while still claiming the mirror, and so shipped
// a read-modify-write that silently reverted whoever it raced.

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

// AppendToEntryOnPR appends body to the KB entry file carried by the merge
// request with this iid, as one further commit on that merge request's OWN
// source branch. It never touches the target branch and never merges.
//
// key identifies this note for idempotency (see okf.NoteMarker); an entry
// already carrying it makes this a no-op reporting success.
//
// Three calls where GitHub needs four, and the saving is real rather than paid
// for in safety: `/changes` returns the merge request AND its changed paths
// together, and the non-raw repository-files endpoint returns the entry's
// content AND the last_commit_id the write needs, so neither fact costs its own
// round trip.
//
// `/changes` is GitLab's older single-MR-changes endpoint, deprecated in 15.7 in
// favour of `/diffs` and still served (removal is scheduled for API v5, which
// does not exist). It is used deliberately: `/diffs` returns the diffs ALONE, so
// it would need a second call for source_branch and state. If v5 ever lands,
// this becomes four calls.
//
// Failures are returned rather than swallowed, and providers.ErrPRNotOpen is
// distinguished from the rest for the reason the GitHub sibling gives: it is the
// one failure on which the caller's comment fallback would be lost too.
func (c *Client) AppendToEntryOnPR(ctx context.Context, number int, body, key string) error {
	var mr struct {
		State           string `json:"state"`
		SourceBranch    string `json:"source_branch"`
		SourceProjectID int64  `json:"source_project_id"`
		TargetProjectID int64  `json:"target_project_id"`
		Changes         []struct {
			NewPath string `json:"new_path"`
		} `json:"changes"`
	}
	if err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/projects/%s/merge_requests/%d/changes", c.projectSeg(), number), nil, &mr); err != nil {
		return err
	}
	// Re-checked out of a response this call already makes — see the GitHub
	// sibling for why the caller's check one round trip ago is not enough. GitLab
	// spells the open state "opened"; "closed", "merged" and "locked" all mean
	// finished.
	if mr.State != "opened" {
		return fmt.Errorf("gitlab MR %d is %q: %w", number, mr.State, providers.ErrPRNotOpen)
	}
	if mr.SourceBranch == "" {
		return fmt.Errorf("gitlab MR %d: no source branch to commit onto", number)
	}
	// The GitLab spelling of GitHub's fork check. source_branch is a bare branch
	// name while every write below addresses c.projectPath, so for a merge
	// request opened from a FORK the branch named lives in another project —
	// commonly "main" — and this would commit onto the configured project's own
	// main. Comparing the two project ids needs no extra lookup and no knowledge
	// of either id: on a same-project merge request they are equal by
	// construction.
	if mr.SourceProjectID != mr.TargetProjectID {
		return fmt.Errorf("gitlab MR %d: source branch is in project %d, not the target project %d (fork); refusing to commit",
			number, mr.SourceProjectID, mr.TargetProjectID)
	}
	changed := make([]string, 0, len(mr.Changes))
	for _, ch := range mr.Changes {
		changed = append(changed, ch.NewPath)
	}
	entry, err := okf.EntryFile(changed)
	if err != nil {
		return fmt.Errorf("gitlab MR %d: %w", number, err)
	}

	// Read the entry on the merge request's OWN branch: it does not exist on the
	// target branch until the request merges.
	raw, lastCommit, found, err := c.getEntryFile(ctx, entry, mr.SourceBranch)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("gitlab MR %d: entry %s is not on branch %s", number, entry, mr.SourceBranch)
	}
	// Never write blind — okf.AppendBlock returns the block ALONE when what it is
	// appending to is empty, so a read that yields nothing while reporting success
	// would replace the entry with one note. See okf.HasFrontmatter.
	if !okf.HasFrontmatter(raw) {
		return fmt.Errorf("gitlab MR %d: %s on branch %s does not read as a catalog entry; refusing to rewrite it", number, entry, mr.SourceBranch)
	}
	if okf.HasNoteMarker(raw, key) {
		// Already recorded: a replayed chat delivery, or a retry of a write whose
		// response was lost. Reporting success is honest — the note is in the entry
		// — and appending it again would duplicate it permanently.
		return nil
	}

	// neutralizeImages for the same reason renderEntry applies it to a first
	// draft — see github.Client.AppendToEntryOnPR for the full argument; the
	// appended block is untrusted text landing on a page a reviewer opens.
	block := neutralizeImages(body)
	if key != "" {
		block += "\n\n" + okf.NoteMarker(key)
	}
	if err := c.commitEntry(ctx, entry, mr.SourceBranch, lastCommit, string(okf.AppendBlock(raw, block))); err != nil {
		return c.appendLanded(ctx, entry, mr.SourceBranch, key, err)
	}
	return nil
}

// getEntryFile reads a file's bytes AND the id of the last commit that touched
// it, at ref, via the non-raw repository-files endpoint.
//
// It exists rather than reusing getRawFile because of that second return value.
// The Commits API overwrites a file UNCONDITIONALLY unless the update action
// carries last_commit_id, and this whole call is a read-modify-write: without it
// a reviewer fixing a typo in the web IDE has their commit silently reverted by
// the next note in the thread, with nothing anywhere reporting that it happened.
// GitHub's sibling gets that property from the blob sha its contents API hands
// back; this file claims to mirror GitHub's guarantees, so it has to hold the
// property rather than resemble it.
//
// The cost over /raw is one base64 decode, and it buys the concurrency control
// AND the encoding check in the same call rather than a second round trip.
// A 404 is not an error: found=false says the file is absent.
func (c *Client) getEntryFile(ctx context.Context, path, ref string) (data []byte, lastCommit string, found bool, err error) {
	var out struct {
		Content      string `json:"content"`
		Encoding     string `json:"encoding"`
		LastCommitID string `json:"last_commit_id"`
	}
	err = c.do(ctx, http.MethodGet,
		fmt.Sprintf("/projects/%s/repository/files/%s?ref=%s", c.projectSeg(), url.PathEscape(path), url.QueryEscape(ref)), nil, &out)
	if isNotFound(err) {
		return nil, "", false, nil
	}
	if err != nil {
		return nil, "", false, err
	}
	// Checked, not assumed, for the reason github.getFile states at length: a
	// read that reports success while handing back nothing turns an append into a
	// replacement.
	if out.Encoding != "base64" {
		return nil, "", false, fmt.Errorf("gitlab GET %s: unsupported content encoding %q", path, out.Encoding)
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return nil, "", false, err
	}
	return raw, out.LastCommitID, true, nil
}

// commitEntry writes content back to path on branch as one update action,
// refusing if anything committed to that file since lastCommit.
//
// GitLab answers a stale last_commit_id with 400 ("You are attempting to update
// a file that has changed since you started editing it"), which is exactly the
// outcome wanted: the racing writer keeps their commit, and this note goes down
// the caller's comment fallback instead of overwriting them.
func (c *Client) commitEntry(ctx context.Context, path, branch, lastCommit, content string) error {
	action := map[string]any{
		"action":    "update",
		"file_path": path,
		"content":   content,
	}
	if lastCommit != "" {
		action["last_commit_id"] = lastCommit
	}
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/repository/commits", c.projectSeg()), map[string]any{
		"branch":         branch,
		"commit_message": "runlore: append operator note to " + path,
		"actions":        []map[string]any{action},
	}, nil)
}

// appendLanded turns "the commit reported an error" into "the commit reported an
// error AND the note is not there", by looking. Same defect and same reasoning
// as github.Client.appendLanded: a lost RESPONSE to a commit GitLab already
// applied would otherwise be filed a second time as a comment and reported on
// the wrong route. A failure to re-read returns the original error.
func (c *Client) appendLanded(ctx context.Context, entry, branch, key string, commitErr error) error {
	if key == "" {
		return commitErr
	}
	raw, _, found, err := c.getEntryFile(ctx, entry, branch)
	if err != nil || !found || !okf.HasNoteMarker(raw, key) {
		return commitErr
	}
	return nil
}
