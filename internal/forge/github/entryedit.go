// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// entryEdit describes one surgical, human-reviewed frontmatter edit to a MERGED
// catalog entry: fetch the entry on the base branch, run stamp over its bytes,
// and open a PR carrying the result. Retirement (`status: retired`) and
// revalidation (`last_validated: <date>`) are the two instances; they differ
// only in the stamp and the PR's naming, so the six-call GitHub dance lives here
// once rather than being copy-pasted per pass — the two can never drift apart on
// branch naming, sha handling, or best-effort labelling.
type entryEdit struct {
	path        string                       // entry path on the base branch
	stamp       func([]byte) ([]byte, error) // the frontmatter edit; a sentinel error means "nothing to do"
	verb        string                       // "retire" | "revalidate": names the branch AND the commit
	titlePrefix string                       // PR title prefix
	labels      []string                     // labels applied best-effort after the PR opens
	body        string                       // reviewer-facing body (carries the caller's hidden marker)
}

// openEntryEditPR performs the fetch → stamp → branch → commit → PR → label
// sequence for one entryEdit. It never merges and never deletes: the PR is the
// proposal and a human is the load-bearing gate. A stamp sentinel (already
// retired, recently validated, inactive entry) is wrapped with the entry path and
// returned, so `errors.Is` still identifies it and the caller's pass can treat it
// as a done-skip; a 404 on the entry file surfaces as an error (entry deleted →
// the pass logs and skips it).
func (c *Client) openEntryEditPR(ctx context.Context, e entryEdit) (providers.Ref, error) {
	// 1. fetch the entry on the base branch: its content and blob sha.
	var file struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	if err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", c.owner, c.repo, e.path, c.baseBranch), nil, &file); err != nil {
		return providers.Ref{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return providers.Ref{}, fmt.Errorf("decode %s: %w", e.path, err)
	}
	stamped, err := e.stamp(raw)
	if err != nil {
		return providers.Ref{}, fmt.Errorf("%s: %w", e.path, err)
	}

	// 2. base ref SHA.
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", c.owner, c.repo, c.baseBranch), nil, &ref); err != nil {
		return providers.Ref{}, err
	}
	// 3. create the edit branch.
	branch := fmt.Sprintf("runlore/%s-%s-%d", e.verb, slugify(e.path), time.Now().Unix())
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/refs", c.owner, c.repo),
		map[string]any{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA}, nil); err != nil {
		return providers.Ref{}, err
	}
	// 4. update the entry file in place — the file sha makes this an update, not a create.
	if err := c.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/%s/contents/%s", c.owner, c.repo, e.path),
		map[string]any{
			"message": "runlore: " + e.verb + " " + e.path,
			"content": base64.StdEncoding.EncodeToString(stamped),
			"branch":  branch,
			"sha":     file.SHA,
		}, nil); err != nil {
		return providers.Ref{}, err
	}
	// 5. open the PR.
	var out struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", c.owner, c.repo),
		map[string]any{"title": e.titlePrefix + e.path, "head": branch, "base": c.baseBranch, "body": e.body}, &out); err != nil {
		return providers.Ref{}, err
	}
	// 6. label the PR. Best-effort: a labelling failure must not lose the PR (same
	// contract as OpenPR), so the error is intentionally ignored.
	if out.Number != 0 {
		_ = c.addLabels(ctx, out.Number, e.labels)
	}
	return providers.Ref{URL: out.HTMLURL}, nil
}

// frontmatterBlock splits an OKF entry into its frontmatter lines and the
// remainder (everything from the closing fence on, returned verbatim so a
// surgical edit can rejoin the file byte-for-byte). A file without a
// frontmatter block errors: a frontmatter stamp must never write blind.
func frontmatterBlock(content []byte) (lines []string, rest string, err error) {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") {
		return nil, "", fmt.Errorf("entry has no YAML frontmatter block")
	}
	body := s[len("---\n"):]
	end := strings.Index(body, "\n---")
	if end < 0 {
		return nil, "", fmt.Errorf("entry frontmatter block is unterminated")
	}
	return strings.Split(body[:end], "\n"), body[end:], nil
}
