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

	"github.com/Smana/runlore/internal/okf"
)

// prFilesPerPage asks for every changed path in one page. A RunLore curation PR
// changes at most three files (the entry plus the reserved index.md / log.md),
// so this is a ceiling far above the real case rather than a pagination scheme:
// okf.EntryFile refuses anything other than exactly one entry, which is what
// would catch a truncated listing rather than let it pick the wrong file.
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
func (c *Client) AppendToEntryOnPR(ctx context.Context, number int, body string) error {
	var pr struct {
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", c.owner, c.repo, number), nil, &pr); err != nil {
		return err
	}
	if pr.Head.Ref == "" {
		return fmt.Errorf("github PR %d: no head branch to commit onto", number)
	}

	var files []struct {
		Filename string `json:"filename"`
	}
	if err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/pulls/%d/files?per_page=%d", c.owner, c.repo, number, prFilesPerPage), nil, &files); err != nil {
		return err
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
	return c.putFile(ctx, entry, pr.Head.Ref, sha, "runlore: append operator note to "+entry,
		okf.AppendBlock(raw, neutralizeImages(body)))
}
