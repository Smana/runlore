// SPDX-License-Identifier: Apache-2.0

package gitlab

// This file mirrors internal/forge/github/noteappend.go: one further commit
// onto a merge request RunLore itself opened, appending an operator note to the
// KB entry that merge request carries — because a comment lives in the forge's
// conversation, which the catalog never indexes, and only the entry becomes
// knowledge when the request merges.

import (
	"context"
	"fmt"
	"net/http"

	"github.com/Smana/runlore/internal/okf"
)

// AppendToEntryOnPR appends body to the KB entry file carried by the merge
// request with this iid, as one further commit on that merge request's OWN
// source branch. It never touches the target branch and never merges.
//
// Three calls where GitHub needs four, the same one-call-shorter shape the rest
// of this client has: `/changes` returns the merge request AND its changed paths
// together, so the source branch and the entry come from one round trip, and the
// Commits API writes the file back without a separate blob-sha read.
//
// `/changes` is GitLab's older single-MR-changes endpoint, deprecated in 15.7 in
// favour of `/diffs` and still served (removal is scheduled for API v5, which
// does not exist). It is used deliberately: `/diffs` returns the diffs ALONE, so
// it would need a second call for `source_branch` — trading a live endpoint for
// an extra round trip on every note. If v5 ever lands, this becomes two calls.
//
// Every failure is returned rather than swallowed; the thread responder decides
// what to degrade to (see thread.Responder.addToPR).
func (c *Client) AppendToEntryOnPR(ctx context.Context, number int, body string) error {
	var mr struct {
		SourceBranch string `json:"source_branch"`
		Changes      []struct {
			NewPath string `json:"new_path"`
		} `json:"changes"`
	}
	if err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/projects/%s/merge_requests/%d/changes", c.projectSeg(), number), nil, &mr); err != nil {
		return err
	}
	if mr.SourceBranch == "" {
		return fmt.Errorf("gitlab MR %d: no source branch to commit onto", number)
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
	raw, found, err := c.getRawFile(ctx, entry, mr.SourceBranch)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("gitlab MR %d: entry %s is not on branch %s", number, entry, mr.SourceBranch)
	}
	// neutralizeImages for the same reason renderEntry applies it to a first
	// draft — see github.Client.AppendToEntryOnPR for the full argument; the
	// appended block is untrusted text landing on a page a reviewer opens.
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/repository/commits", c.projectSeg()), map[string]any{
		"branch":         mr.SourceBranch,
		"commit_message": "runlore: append operator note to " + entry,
		"actions": []map[string]any{{
			"action":    "update",
			"file_path": entry,
			"content":   string(okf.AppendBlock(raw, neutralizeImages(body))),
		}},
	}, nil)
}
