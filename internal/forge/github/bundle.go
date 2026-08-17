// SPDX-License-Identifier: Apache-2.0

package github

// This file is OKF bundle maintenance: keep the reserved index.md / log.md files
// in step with the entry a PR adds, so the bundle stays self-describing for every
// OKF consumer (progressive-disclosure index, chronological change log) and the
// reviewer sees the whole change in one diff.
//
// Only the GitHub-specific transport lives here (a read + a PUT per file, four
// calls in all). The markdown surgery itself is okf.UpdateIndex / okf.UpdateLog,
// shared with internal/forge/gitlab so the bundle's shape can't depend on which
// forge hosts the KB.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

// maintainBundle updates index.md (only when the bundle already has one — its
// structure is the owner's choice) and creates/appends log.md (whose shape OKF §7
// fully specifies) on the PR branch. Best-effort by contract: a failure here must
// never lose the entry PR, so the caller ignores the returned error beyond logging.
func (c *Client) maintainBundle(ctx context.Context, e providers.KBEntry, entryPath, branch string) error {
	date := time.Now().UTC().Format("2006-01-02")

	idx, idxSHA, found, err := c.getFile(ctx, "index.md", branch)
	if err != nil {
		return fmt.Errorf("read index.md: %w", err)
	}
	if found {
		if err := c.putFile(ctx, "index.md", branch, idxSHA,
			"runlore: index "+e.Title, okf.UpdateIndex(idx, e, entryPath)); err != nil {
			return err
		}
	}

	logMD, logSHA, _, err := c.getFile(ctx, "log.md", branch)
	if err != nil {
		return fmt.Errorf("read log.md: %w", err)
	}
	return c.putFile(ctx, "log.md", branch, logSHA,
		"runlore: log "+e.Title, okf.UpdateLog(logMD, e, entryPath, date))
}

// getFile reads a file's content + blob SHA at ref via the contents API.
// A 404 is not an error: found=false says "the bundle doesn't have this file".
func (c *Client) getFile(ctx context.Context, path, ref string) (data []byte, sha string, found bool, err error) {
	tok, err := c.token(ctx)
	if err != nil {
		return nil, "", false, fmt.Errorf("token: %w", err)
	}
	url := fmt.Sprintf("%s/repos/%s/%s/contents/%s?ref=%s", c.baseURL, c.owner, c.repo, path, ref)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, "", false, err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, "", false, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, "", false, nil
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", false, fmt.Errorf("github GET %s: status %d", path, resp.StatusCode)
	}
	var out struct {
		SHA      string `json:"sha"`
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", false, err
	}
	// Refuse anything that is not base64. For a blob over 1 MB the contents API
	// answers 200 with an EMPTY content field and encoding "none" — which
	// base64-decodes without error, so a caller that trusts (raw, found) is handed
	// zero bytes from a "successful" read. On the read-modify-write append path
	// that becomes replacing a whole entry with the newest note, since
	// okf.AppendBlock returns the block ALONE when what it appends to is empty.
	// Failing here degrades to the caller's comment fallback; reading nothing
	// silently does not.
	if out.Encoding != "base64" {
		return nil, "", false, fmt.Errorf("github GET %s: content encoding %q, want base64 (blob too large for the contents API?)", path, out.Encoding)
	}
	// The contents API base64-wraps with newlines; the std decoder wants them gone.
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(out.Content, "\n", ""))
	if err != nil {
		return nil, "", false, err
	}
	return raw, out.SHA, true, nil
}

// putFile creates/updates a file on branch. sha is the current blob SHA when
// updating ("" when creating).
func (c *Client) putFile(ctx context.Context, path, branch, sha, message string, content []byte) error {
	body := map[string]any{
		"message": message,
		"content": base64.StdEncoding.EncodeToString(content),
		"branch":  branch,
	}
	if sha != "" {
		body["sha"] = sha
	}
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/%s/contents/%s", c.owner, c.repo, path), body, nil)
}
