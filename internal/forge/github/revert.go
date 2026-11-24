// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

var _ providers.RevertForge = (*Client)(nil)

func splitGitHubRepo(repo string) (owner, name string, err error) {
	parts := strings.Split(strings.Trim(strings.TrimSpace(repo), "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return "", "", fmt.Errorf("invalid github repo %q (want owner/repo)", repo)
	}
	return parts[0], parts[1], nil
}

func encodeGitHubContentPath(p string) string {
	segs := strings.Split(strings.TrimPrefix(p, "/"), "/")
	for i, s := range segs {
		segs[i] = url.PathEscape(s)
	}
	return strings.Join(segs, "/")
}

// DefaultBranch resolves the repository's default branch name and its current HEAD SHA.
func (c *Client) DefaultBranch(ctx context.Context, repo string) (branch, headSHA string, err error) {
	owner, name, err := splitGitHubRepo(repo)
	if err != nil {
		return "", "", err
	}
	var meta struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s", owner, name), nil, &meta); err != nil {
		return "", "", err
	}
	if meta.DefaultBranch == "" {
		return "", "", fmt.Errorf("github repo %s returned empty default_branch", repo)
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", owner, name, url.PathEscape(meta.DefaultBranch)), nil, &ref); err != nil {
		return "", "", err
	}
	return meta.DefaultBranch, ref.Object.SHA, nil
}

// CommitDetail fetches a single commit's parents, message, and touched files.
func (c *Client) CommitDetail(ctx context.Context, repo, sha string) (providers.CommitDetail, error) {
	owner, name, err := splitGitHubRepo(repo)
	if err != nil {
		return providers.CommitDetail{}, err
	}
	var raw struct {
		SHA     string `json:"sha"`
		Parents []struct {
			SHA string `json:"sha"`
		} `json:"parents"`
		Commit struct {
			Message string `json:"message"`
		} `json:"commit"`
		Files []struct {
			Filename         string `json:"filename"`
			Status           string `json:"status"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/commits/%s", owner, name, url.PathEscape(sha)), nil, &raw); err != nil {
		return providers.CommitDetail{}, err
	}
	parents := make([]string, 0, len(raw.Parents))
	for _, p := range raw.Parents {
		parents = append(parents, p.SHA)
	}
	files := make([]providers.CommitFile, 0, len(raw.Files))
	for _, f := range raw.Files {
		files = append(files, providers.CommitFile{
			Path:         f.Filename,
			Status:       f.Status,
			PreviousPath: f.PreviousFilename,
		})
	}
	return providers.CommitDetail{
		SHA:     raw.SHA,
		Parents: parents,
		Message: raw.Commit.Message,
		Files:   files,
	}, nil
}

// CompareChangedFiles returns all file paths modified between baseSHA and headSHA.
func (c *Client) CompareChangedFiles(ctx context.Context, repo, baseSHA, headSHA string) ([]string, error) {
	owner, name, err := splitGitHubRepo(repo)
	if err != nil {
		return nil, err
	}
	var raw struct {
		Files []struct {
			Filename         string `json:"filename"`
			PreviousFilename string `json:"previous_filename"`
		} `json:"files"`
	}
	path := fmt.Sprintf("/repos/%s/%s/compare/%s...%s", owner, name, url.PathEscape(baseSHA), url.PathEscape(headSHA))
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(raw.Files))
	for _, f := range raw.Files {
		if f.Filename != "" && !seen[f.Filename] {
			seen[f.Filename] = true
			out = append(out, f.Filename)
		}
		if f.PreviousFilename != "" && !seen[f.PreviousFilename] {
			seen[f.PreviousFilename] = true
			out = append(out, f.PreviousFilename)
		}
	}
	return out, nil
}

// FileBlob returns the exact bytes of filePath at ref, or (nil, false, nil) if absent.
func (c *Client) FileBlob(ctx context.Context, repo, ref, filePath string) ([]byte, bool, error) {
	owner, name, err := splitGitHubRepo(repo)
	if err != nil {
		return nil, false, err
	}
	var raw struct {
		SHA      string `json:"sha"`
		Encoding string `json:"encoding"`
		Content  string `json:"content"`
	}
	p := fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", owner, name, encodeGitHubContentPath(filePath), url.QueryEscape(ref))
	if err := c.do(ctx, http.MethodGet, p, nil, &raw); err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	// Files >1 MiB omit inline content on the contents API; fall back to git/blobs.
	if raw.Content == "" && raw.SHA != "" {
		var blob struct {
			Encoding string `json:"encoding"`
			Content  string `json:"content"`
		}
		if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/git/blobs/%s", owner, name, url.PathEscape(raw.SHA)), nil, &blob); err != nil {
			return nil, false, err
		}
		raw.Encoding = blob.Encoding
		raw.Content = blob.Content
	}
	if raw.Encoding == "base64" {
		cleaned := strings.ReplaceAll(raw.Content, "\n", "")
		data, err := base64.StdEncoding.DecodeString(cleaned)
		if err != nil {
			return nil, false, fmt.Errorf("decode base64 blob %s: %w", filePath, err)
		}
		return data, true, nil
	}
	return []byte(raw.Content), true, nil
}

// FindOpenPRByHead returns an existing open PR for headBranch if one exists.
func (c *Client) FindOpenPRByHead(ctx context.Context, repo, headBranch string) (prURL string, number int, found bool, err error) {
	owner, name, err := splitGitHubRepo(repo)
	if err != nil {
		return "", 0, false, err
	}
	var prs []struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	p := fmt.Sprintf("/repos/%s/%s/pulls?state=open&head=%s&per_page=10", owner, name, url.QueryEscape(owner+":"+headBranch))
	if err := c.do(ctx, http.MethodGet, p, nil, &prs); err != nil {
		return "", 0, false, err
	}
	if len(prs) == 0 {
		return "", 0, false, nil
	}
	return prs[0].HTMLURL, prs[0].Number, true, nil
}

// CreateRevertDraftPR atomically builds a single inverse commit via the GitHub
// Git Data API (blobs -> tree -> commit -> ref) and opens a Draft pull request.
func (c *Client) CreateRevertDraftPR(ctx context.Context, req providers.RevertPRRequest) (string, int, error) {
	owner, name, err := splitGitHubRepo(req.Repo)
	if err != nil {
		return "", 0, err
	}
	// 1. Fetch base commit's tree SHA.
	var baseCommit struct {
		Tree struct {
			SHA string `json:"sha"`
		} `json:"tree"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/git/commits/%s", owner, name, url.PathEscape(req.BaseSHA)), nil, &baseCommit); err != nil {
		return "", 0, err
	}
	// 2. Create blobs for restored files and assemble tree entries (sha: null deletes).
	type treeEntry struct {
		Path string  `json:"path"`
		Mode string  `json:"mode"`
		Type string  `json:"type"`
		SHA  *string `json:"sha"`
	}
	entries := make([]treeEntry, 0, len(req.Files))
	for _, f := range req.Files {
		if f.Delete {
			entries = append(entries, treeEntry{
				Path: f.Path,
				Mode: "100644",
				Type: "blob",
				SHA:  nil,
			})
			continue
		}
		var blobOut struct {
			SHA string `json:"sha"`
		}
		err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/blobs", owner, name), map[string]any{
			"content":  base64.StdEncoding.EncodeToString(f.Content),
			"encoding": "base64",
		}, &blobOut)
		if err != nil {
			return "", 0, err
		}
		sha := blobOut.SHA
		entries = append(entries, treeEntry{
			Path: f.Path,
			Mode: "100644",
			Type: "blob",
			SHA:  &sha,
		})
	}
	// 3. Create new tree atop base tree.
	var newTree struct {
		SHA string `json:"sha"`
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/trees", owner, name), map[string]any{
		"base_tree": baseCommit.Tree.SHA,
		"tree":      entries,
	}, &newTree); err != nil {
		return "", 0, err
	}
	// 4. Create atomic revert commit.
	var newCommit struct {
		SHA string `json:"sha"`
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/commits", owner, name), map[string]any{
		"message": req.CommitMessage,
		"tree":    newTree.SHA,
		"parents": []string{req.BaseSHA},
	}, &newCommit); err != nil {
		return "", 0, err
	}
	// 5. Create branch ref pointing to the new commit.
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/refs", owner, name), map[string]any{
		"ref": "refs/heads/" + req.HeadBranch,
		"sha": newCommit.SHA,
	}, nil); err != nil {
		return "", 0, err
	}
	// 6. Open Draft PR (draft: true is hardcoded; never auto-ready).
	var pr struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", owner, name), map[string]any{
		"title": req.Title,
		"head":  req.HeadBranch,
		"base":  req.BaseBranch,
		"body":  req.Body,
		"draft": true,
	}, &pr); err != nil {
		return "", 0, err
	}
	return pr.HTMLURL, pr.Number, nil
}
