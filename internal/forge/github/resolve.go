// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Smana/runlore/internal/providers"
)

// resolveLabels mark an entry-edit PR that records a verified resolution on a
// merged entry: "runlore" for the shared namespace, "runlore-resolve" for the
// idempotency listing and for the guard that keeps such a PR out of the queue.
var resolveLabels = []string{"runlore", "runlore-resolve"}

// OpenResolvePR opens a human-reviewed PR that applies edit to a merged entry:
// the way a resolution reaches an entry whose own PR is gone. edit reports whether
// it changed anything; when it did not, no PR opens and providers.ErrAlreadyResolved
// is returned. body carries the caller's hidden marker.
func (c *Client) OpenResolvePR(ctx context.Context, path string, edit func([]byte) ([]byte, bool, error), body string) (providers.Ref, error) {
	return c.openEntryEditPR(ctx, entryEdit{
		path: path,
		stamp: func(raw []byte) ([]byte, error) {
			out, changed, err := edit(raw)
			if err != nil {
				return nil, err
			}
			if !changed {
				return nil, providers.ErrAlreadyResolved
			}
			return out, nil
		},
		verb:        "resolve",
		titlePrefix: "KB resolve: ",
		labels:      resolveLabels,
		body:        body,
	})
}

// PullSummary reads what a pull request in repo ("owner/name", which need not be
// the knowledge base) changed: its URL, merge commit and line counts from
// GET /pulls/{n}, and the first five changed files from its /files. A 404 is
// providers.ErrPullNotFound; any other failure is returned as it is, so a caller
// can tell a gone PR from a blip.
func (c *Client) PullSummary(ctx context.Context, repo string, number int) (providers.PullSummary, error) {
	var pr struct {
		HTMLURL        string `json:"html_url"`
		MergeCommitSHA string `json:"merge_commit_sha"`
		Additions      int    `json:"additions"`
		Deletions      int    `json:"deletions"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d", repo, number), nil, &pr); err != nil {
		if isNotFound(err) {
			return providers.PullSummary{}, fmt.Errorf("%s#%d: %w", repo, number, providers.ErrPullNotFound)
		}
		return providers.PullSummary{}, err
	}
	var files []struct {
		Filename string `json:"filename"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/pulls/%d/files?per_page=5", repo, number), nil, &files); err != nil {
		if isNotFound(err) {
			return providers.PullSummary{}, fmt.Errorf("%s#%d: %w", repo, number, providers.ErrPullNotFound)
		}
		return providers.PullSummary{}, err
	}
	out := providers.PullSummary{URL: pr.HTMLURL, MergeSHA: pr.MergeCommitSHA, Additions: pr.Additions, Deletions: pr.Deletions}
	for _, f := range files {
		out.Files = append(out.Files, f.Filename)
	}
	return out, nil
}
