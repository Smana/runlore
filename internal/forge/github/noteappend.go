// SPDX-License-Identifier: Apache-2.0

package github

// This file is the "add to the entry, not beside it" write: one further commit
// onto a pull request RunLore itself opened, appending an operator note to the
// KB entry that pull request carries.
//
// It exists because a comment is not knowledge. A note filed as a PR comment
// lives in the forge's conversation, which the catalog never indexes — so a
// thread whose second, third and fourth notes were comments merged as an entry
// holding only the first, and kb_search never saw the rest.

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

// prFilesPerPage asks for every changed path in one page. A RunLore curation PR
// changes at most three files (the entry plus the reserved index.md / log.md),
// so this is a ceiling far above the real case rather than a pagination scheme.
//
// It is NOT okf.EntryFile that catches a truncated listing — a >100-file pull
// request whose first page happens to hold exactly one non-reserved .md resolves
// cleanly to the wrong file. A full page is refused explicitly below instead.
const prFilesPerPage = 100

// AppendToEntryOnPR appends body to the KB entry file carried by the pull
// request numbered `number`, as one further commit on that pull request's OWN
// head branch. It never touches the base branch and never merges: the pull
// request stays the proposal and a human stays the gate, exactly as when the
// entry was first drafted.
//
// Four calls: read the PR (its head branch), list its changed paths, read the
// entry on that branch, PUT it back. The entry is located from the pull request
// itself rather than passed in, so no caller has to persist a path it wrote
// weeks ago and no stale path can send a commit somewhere unintended — see
// okf.EntryFile, which refuses to guess when the changed paths do not name
// exactly one entry.
//
// Every failure is returned rather than swallowed. The thread responder's
// fallback for one is a comment (thread.Responder.addToPR), which keeps the
// human's words even when this route cannot run — but that choice is the
// caller's to make with the error in hand, not this function's to make by
// pretending it succeeded.
func (c *Client) AppendToEntryOnPR(ctx context.Context, number int, body, key string) error {
	var pr struct {
		State string `json:"state"`
		Head  struct {
			Ref  string `json:"ref"`
			Repo struct {
				FullName string `json:"full_name"`
			} `json:"repo"`
		} `json:"head"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", c.owner, c.repo, number), nil, &pr); err != nil {
		return err
	}
	// Re-checked here, out of a response this call already makes, even though the
	// caller checked one round trip ago: four calls run between the two, and a PR
	// that merged in the window either 404s on a deleted branch — sending the
	// caller to comment on a MERGED pull request, the silent-loss case its
	// IsPROpen check exists to prevent — or takes a commit onto a branch that will
	// never reach base while this reports success. Zero extra calls.
	if pr.State != "open" {
		return fmt.Errorf("github PR %d is %q: %w", number, pr.State, providers.ErrPRNotOpen)
	}
	if pr.Head.Ref == "" {
		return fmt.Errorf("github PR %d: no head branch to commit onto", number)
	}
	// head.ref is a bare BRANCH NAME while every write below addresses
	// c.owner/c.repo, so for a pull request opened from a FORK the branch named
	// lives in another repository — commonly "main" — and this would commit onto
	// the configured repository's own main. Not reachable today (the append route
	// requires a PR RunLore itself opened, recorded from OpenPR's own return), but
	// it is the single check standing between "commit onto a note branch" and
	// "commit onto main", so it does not depend on that staying true.
	if want := c.owner + "/" + c.repo; !strings.EqualFold(pr.Head.Repo.FullName, want) {
		return fmt.Errorf("github PR %d: head branch is in %q, not %q (fork); refusing to commit", number, pr.Head.Repo.FullName, want)
	}

	var files []struct {
		Filename string `json:"filename"`
	}
	if err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=%d", c.owner, c.repo, number, prFilesPerPage), nil, &files); err != nil {
		return err
	}
	// A FULL page means one of two things, and neither permits a write: either
	// this is not a RunLore curation pull request (they change at most three
	// files — the entry plus the reserved index.md and log.md), or the listing was
	// truncated and okf.EntryFile is about to choose among a fraction of the
	// changed paths with no way to know it did. "The one .md on page 1" is not a
	// fact about the pull request, so it is not a file to rewrite.
	if len(files) >= prFilesPerPage {
		return fmt.Errorf("github PR %d: %d changed files in one page; the listing may be truncated, refusing to pick an entry from it", number, len(files))
	}
	changed := make([]string, 0, len(files))
	for _, f := range files {
		changed = append(changed, f.Filename)
	}
	entry, err := okf.EntryFile(changed)
	if err != nil {
		return fmt.Errorf("github PR %d: %w", number, err)
	}

	// Read the entry on the PR's OWN branch, not the base branch: the entry does
	// not exist on base until the PR merges, and reading base would either 404 or
	// — for a PR editing a merged entry — append onto the wrong revision.
	raw, sha, found, err := c.getFile(ctx, entry, pr.Head.Ref)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("github PR %d: entry %s is not on branch %s", number, entry, pr.Head.Ref)
	}
	// Never write blind. okf.AppendBlock returns the block ALONE when what it is
	// appending to is empty — correct behaviour, it is how a first draft is
	// built — so a read that yields nothing while reporting success would replace
	// the entry with one note, frontmatter and every earlier note gone, and the
	// PUT would succeed because it carries a valid blob sha. getFile now refuses a
	// non-base64 body, which is the known way to get here; this is the guard that
	// does not depend on knowing the next one.
	if !okf.HasFrontmatter(raw) {
		return fmt.Errorf("github PR %d: %s on branch %s does not read as a catalog entry; refusing to rewrite it", number, entry, pr.Head.Ref)
	}
	if okf.HasNoteMarker(raw, key) {
		// Already recorded: a replayed chat delivery, or a retry of a write whose
		// response was lost. Reporting success is honest — the note IS in the entry —
		// and appending again would duplicate it permanently in catalog content that
		// kb_search then indexes twice.
		return nil
	}

	// The blob sha makes this an UPDATE of the revision just read: GitHub rejects
	// the PUT if anything else committed to that file in between, so a racing
	// writer loses the call rather than silently overwriting the note it did not
	// see.
	//
	// neutralizeImages for the same reason renderEntry applies it to a first
	// draft — the appended block is untrusted text, and an entry file rendered on
	// a review page must not be able to fire a request from the reviewer's
	// browser. It is applied HERE, on the whole block, rather than trusted to the
	// caller: thread.NoteBody neutralises the note TEXT but interpolates identity
	// fields (author, thread title) around it, and on the first-draft path those
	// are covered by renderEntry, not by NoteBody.
	block := neutralizeImages(body)
	if key != "" {
		block += "\n\n" + okf.NoteMarker(key)
	}
	if err := c.putFile(ctx, entry, pr.Head.Ref, sha, "runlore: append operator note to "+entry,
		okf.AppendBlock(raw, block)); err != nil {
		return c.appendLanded(ctx, entry, pr.Head.Ref, key, err)
	}
	return nil
}

// appendLanded turns "the write landed but the response did not" into success.
//
// c.do returns an error for any transport failure, including one AFTER the server
// processed the request — a response timeout, a reset connection, a cancelled
// context. The commit is on the branch; the caller sees an error, falls back to a
// comment, and the note ends up BOTH in the entry and duplicated in the
// conversation, reported to the human with the comment's wording and counted
// under the comment route. That metric exists precisely to separate a note that
// became knowledge from one discarded at merge, so it would be wrong on the one
// case where it matters.
//
// Re-reading is cheap and unambiguous: the marker is either there or it is not.
// With no key there is nothing to look for, so the original error stands.
func (c *Client) appendLanded(ctx context.Context, entry, branch, key string, putErr error) error {
	if key == "" {
		return putErr
	}
	raw, _, found, err := c.getFile(ctx, entry, branch)
	if err != nil || !found || !okf.HasNoteMarker(raw, key) {
		return putErr
	}
	return nil
}
