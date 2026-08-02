// SPDX-License-Identifier: Apache-2.0

package gitlab

// This file is OKF bundle maintenance for GitLab: keep the reserved index.md /
// log.md files in step with the entry a merge request adds, so a GitLab-hosted
// catalog stays as self-describing as a GitHub-hosted one (progressive-disclosure
// index, chronological change log) and the reviewer sees the whole change in one
// diff. Without it a GitLab KB silently stops maintaining both files the moment
// the team switches forges — the entries keep landing, the map to them rots.
//
// The markdown surgery is okf.UpdateIndex / okf.UpdateLog, shared with
// internal/forge/github so the bundle's shape cannot depend on which forge hosts
// it. Only the transport differs, and this is where GitLab is genuinely better:
// GitHub needs four extra calls (a read and a PUT per file, on the PR branch it
// has already created), while GitLab's Commits API takes a LIST of actions, so
// the index/log writes ride along in the very same commit that creates the entry
// — one atomic commit, no half-maintained branch if a later call fails.

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

// bundleActions returns the extra Commits-API actions that maintain index.md and
// log.md for the entry being filed at entryPath. Both files are read from ref
// (the MR's target branch — the branch the commit will be started from, since it
// does not exist yet).
//
// Best-effort by contract, exactly like github.Client.maintainBundle: bundle
// upkeep must never cost the entry. A read failure returns whatever actions were
// already gathered, so OpenPR still commits the entry and opens the merge
// request. Note the asymmetry with GitHub: because these actions share ONE commit
// with the entry, a bad action would fail the entry too — which is why a file is
// only ever touched after a successful read told us it is there ("update") or
// definitively is not ("create").
func (c *Client) bundleActions(ctx context.Context, e providers.KBEntry, entryPath, ref string) []map[string]any {
	var out []map[string]any

	// index.md is updated ONLY when the bundle already has one: an index's
	// structure is the owner's choice, so RunLore never imposes one.
	idx, found, err := c.getRawFile(ctx, "index.md", ref)
	if err != nil {
		return out
	}
	if found {
		out = append(out, map[string]any{
			"action":    "update",
			"file_path": "index.md",
			"content":   string(okf.UpdateIndex(idx, e, entryPath)),
		})
	}

	// log.md's shape is fully specified by OKF §7, so it is created when absent.
	logMD, found, err := c.getRawFile(ctx, "log.md", ref)
	if err != nil {
		return out
	}
	action := "create"
	if found {
		action = "update"
	}
	return append(out, map[string]any{
		"action":    action,
		"file_path": "log.md",
		"content":   string(okf.UpdateLog(logMD, e, entryPath, time.Now().UTC().Format("2006-01-02"))),
	})
}

// getRawFile reads a file's raw bytes at ref via the repository-files endpoint.
// A 404 is not an error: found=false says "the bundle doesn't have this file".
//
// The file path is URL-encoded as a single path segment, the same rule (and the
// same 404-that-looks-like-a-permissions-problem trap) as the project path —
// GitLab wants "docs%2Findex.md", not "docs/index.md". The /raw suffix is what
// makes this cheap: the non-raw variant returns JSON with the content
// base64-wrapped, which would mean a decode step for no gain.
func (c *Client) getRawFile(ctx context.Context, path, ref string) ([]byte, bool, error) {
	data, _, err := c.request(ctx, http.MethodGet,
		fmt.Sprintf("/projects/%s/repository/files/%s/raw?ref=%s",
			c.projectSeg(), url.PathEscape(path), url.QueryEscape(ref)), nil)
	if isNotFound(err) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	return data, true, nil
}
