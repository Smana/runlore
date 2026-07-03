package github

// This file is OKF bundle maintenance: keep the reserved index.md / log.md files
// in step with the entry a PR adds, so the bundle stays self-describing for every
// OKF consumer (progressive-disclosure index, chronological change log) and the
// reviewer sees the whole change in one diff.

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

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
			"runlore: index "+e.Title, updateIndex(idx, e, entryPath)); err != nil {
			return err
		}
	}

	logMD, logSHA, _, err := c.getFile(ctx, "log.md", branch)
	if err != nil {
		return fmt.Errorf("read log.md: %w", err)
	}
	return c.putFile(ctx, "log.md", branch, logSHA,
		"runlore: log "+e.Title, updateLog(logMD, e, entryPath, date))
}

// updateIndex appends the entry's link line to its "## <Type>s" section of an
// OKF index (creating the section at the end when absent). Links are relative to
// the bundle root, matching the seed index style.
func updateIndex(existing []byte, e providers.KBEntry, entryPath string) []byte {
	section := "## " + e.Type + "s"
	line := fmt.Sprintf("- [%s](%s) — %s", e.Title, entryPath, e.Description)

	lines := strings.Split(strings.TrimRight(string(existing), "\n"), "\n")
	start := -1
	for i, l := range lines {
		if strings.TrimSpace(l) == section {
			start = i
			break
		}
	}
	if start == -1 {
		lines = append(lines, "", section, "", line)
		return []byte(strings.Join(lines, "\n") + "\n")
	}
	// Insert at the section's end: just before the next heading, or at EOF.
	end := len(lines)
	for i := start + 1; i < len(lines); i++ {
		if strings.HasPrefix(strings.TrimSpace(lines[i]), "#") {
			end = i
			break
		}
	}
	// Trim the section's trailing blank lines so the new line joins the list.
	for end > start+1 && strings.TrimSpace(lines[end-1]) == "" {
		end--
	}
	out := append(append(append([]string{}, lines[:end]...), line), lines[end:]...)
	return []byte(strings.Join(out, "\n") + "\n")
}

// updateLog records the entry in an OKF log: flat date-grouped entries, newest
// first, bold action word (§7). A nil/empty existing log gets the standard shape.
func updateLog(existing []byte, e providers.KBEntry, entryPath, date string) []byte {
	heading := "## " + date
	line := fmt.Sprintf("* **Creation**: Added [%s](%s).", e.Title, entryPath)

	cur := strings.TrimRight(string(existing), "\n")
	if strings.TrimSpace(cur) == "" {
		return []byte("# Knowledge catalog update log\n\n" + heading + "\n\n" + line + "\n")
	}
	lines := strings.Split(cur, "\n")
	for i, l := range lines {
		if strings.TrimSpace(l) != heading {
			continue
		}
		// Today's heading exists (it is the newest — logs are newest-first):
		// slot the line right under it.
		out := append(append(append([]string{}, lines[:i+1]...), "", line), lines[i+1:]...)
		return []byte(strings.Join(out, "\n") + "\n")
	}
	// New (newest) date: insert the heading after the H1 title, before older dates.
	at := len(lines)
	for i, l := range lines {
		if strings.HasPrefix(strings.TrimSpace(l), "## ") {
			at = i
			break
		}
	}
	out := append(append(append([]string{}, lines[:at]...), heading, "", line, ""), lines[at:]...)
	return []byte(strings.Join(out, "\n") + "\n")
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
		SHA     string `json:"sha"`
		Content string `json:"content"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, "", false, err
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
