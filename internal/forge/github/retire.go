// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// ErrAlreadyRetired signals the entry already carries status:retired on the base
// branch, so no retirement PR is needed. The curate pass treats it as done-skip.
var ErrAlreadyRetired = errors.New("entry already retired on base branch")

// setStatusRetired stamps `status: retired` into an OKF entry's YAML frontmatter,
// editing ONLY the status line — human formatting, key order and comments are
// preserved (this file is a human-authored artifact under review; a re-marshal
// would produce an unreadable retirement diff). Scanning is fence-bounded so a
// "status:" string in the markdown body is never touched. already=true means the
// entry is retired on the base branch and no PR is needed. A file without a
// frontmatter block errors: retirement must never write blind.
func setStatusRetired(content []byte) (out []byte, already bool, err error) {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") {
		return nil, false, fmt.Errorf("entry has no YAML frontmatter block")
	}
	rest := s[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, false, fmt.Errorf("entry frontmatter block is unterminated")
	}
	fm, body := rest[:end], rest[end:]
	lines := strings.Split(fm, "\n")
	for i, ln := range lines {
		if key, val, ok := strings.Cut(ln, ":"); ok && strings.TrimSpace(key) == "status" {
			if strings.TrimSpace(val) == "retired" {
				return content, true, nil
			}
			lines[i] = "status: retired"
			return []byte("---\n" + strings.Join(lines, "\n") + body), false, nil
		}
	}
	return []byte("---\nstatus: retired\n" + fm + body), false, nil
}

// retireLabels mark a PR proposed by the curate retirement pass — "runlore" for
// the shared forge namespace, "runlore-retire" for the pass's idempotency and
// human-veto listings.
var retireLabels = []string{"runlore", "runlore-retire"}

// OpenRetirePR opens a human-reviewed PR that stamps status:retired into an
// existing catalog entry's frontmatter. It never merges and never deletes — a
// human is the load-bearing gate. body carries the reviewer-facing track record
// and the hidden idempotency marker (authored by the caller). Returns
// ErrAlreadyRetired when the entry is already retired on the base branch (no PR
// opened); a 404 on the entry file surfaces as an error (entry deleted → the pass
// logs and skips it).
func (c *Client) OpenRetirePR(ctx context.Context, entryPath, body string) (providers.Ref, error) {
	// 1. fetch the entry on the base branch: its content and blob sha.
	var file struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	if err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", c.owner, c.repo, entryPath, c.baseBranch), nil, &file); err != nil {
		return providers.Ref{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return providers.Ref{}, fmt.Errorf("decode %s: %w", entryPath, err)
	}
	stamped, already, err := setStatusRetired(raw)
	if err != nil {
		return providers.Ref{}, fmt.Errorf("%s: %w", entryPath, err)
	}
	if already {
		return providers.Ref{}, ErrAlreadyRetired
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
	// 3. create the retire branch.
	branch := fmt.Sprintf("runlore/retire-%s-%d", slugify(entryPath), time.Now().Unix())
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/refs", c.owner, c.repo),
		map[string]any{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA}, nil); err != nil {
		return providers.Ref{}, err
	}
	// 4. update the entry file in place — the file sha makes this an update, not a create.
	if err := c.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/%s/contents/%s", c.owner, c.repo, entryPath),
		map[string]any{
			"message": "runlore: retire " + entryPath,
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
		map[string]any{"title": "KB retire: " + entryPath, "head": branch, "base": c.baseBranch, "body": body}, &out); err != nil {
		return providers.Ref{}, err
	}
	// 6. label the PR. Best-effort: a labelling failure must not lose the PR (same
	// contract as OpenPR), so the error is intentionally ignored.
	if out.Number != 0 {
		_ = c.addLabels(ctx, out.Number, retireLabels)
	}
	return providers.Ref{URL: out.HTMLURL}, nil
}
