// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// CreateIssue opens an issue and returns its number and web URL. The Knowledge queue is the
// one issue RunLore files, once per KB repository.
func (c *Client) CreateIssue(ctx context.Context, title, body string, labels []string) (int, providers.Ref, error) {
	var out struct {
		Number  int    `json:"number"`
		HTMLURL string `json:"html_url"`
	}
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues", c.owner, c.repo),
		map[string]any{"title": title, "body": body, "labels": labels}, &out)
	if err != nil {
		return 0, providers.Ref{}, err
	}
	return out.Number, providers.Ref{URL: out.HTMLURL}, nil
}

// EditIssue replaces an issue's body.
func (c *Client) EditIssue(ctx context.Context, number int, body string) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/%s/issues/%d", c.owner, c.repo, number),
		map[string]any{"body": body}, nil)
}

// ListMergedPRsByLabel returns the PRs carrying the label that merged on or after since (a
// zero since lists every merge). It uses the Search API, which filters server-side with day
// granularity, then drops the PRs merged before the exact instant.
func (c *Client) ListMergedPRsByLabel(ctx context.Context, label string, since time.Time) ([]providers.CuratedIssue, error) {
	query := fmt.Sprintf("repo:%s/%s is:pr is:merged label:%s", c.owner, c.repo, label)
	if !since.IsZero() {
		query += " merged:>=" + since.UTC().Format(time.DateOnly)
	}
	raw, err := c.searchIssues(ctx, query)
	if err != nil {
		return nil, err
	}
	var out []providers.CuratedIssue
	for _, ri := range raw {
		if ri.PullRequest == nil {
			continue // a plain issue, not a PR
		}
		ci := ri.curated()
		if ri.PullRequest.MergedAt != nil {
			ci.MergedAt = *ri.PullRequest.MergedAt
			if !since.IsZero() && ci.MergedAt.Before(since) {
				continue
			}
		}
		out = append(out, ci)
	}
	return out, nil
}
