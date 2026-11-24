// SPDX-License-Identifier: Apache-2.0

package gitlab

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

var _ providers.RevertForge = (*Client)(nil)

func repoSeg(repo string) (string, error) {
	trimmed := strings.Trim(strings.TrimSpace(repo), "/")
	if trimmed == "" || !strings.Contains(trimmed, "/") {
		return "", fmt.Errorf("invalid gitlab project path %q (want group/project)", repo)
	}
	return url.PathEscape(trimmed), nil
}

// DefaultBranch resolves the project's default branch name and its current HEAD SHA.
func (c *Client) DefaultBranch(ctx context.Context, repo string) (branch, headSHA string, err error) {
	seg, err := repoSeg(repo)
	if err != nil {
		return "", "", err
	}
	var proj struct {
		DefaultBranch string `json:"default_branch"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/projects/%s", seg), nil, &proj); err != nil {
		return "", "", err
	}
	if proj.DefaultBranch == "" {
		return "", "", fmt.Errorf("gitlab project %s returned empty default_branch", repo)
	}
	var br struct {
		Commit struct {
			ID string `json:"id"`
		} `json:"commit"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/projects/%s/repository/branches/%s", seg, url.PathEscape(proj.DefaultBranch)), nil, &br); err != nil {
		return "", "", err
	}
	return proj.DefaultBranch, br.Commit.ID, nil
}

// CommitDetail fetches a single commit's parents, message, and touched files.
func (c *Client) CommitDetail(ctx context.Context, repo, sha string) (providers.CommitDetail, error) {
	seg, err := repoSeg(repo)
	if err != nil {
		return providers.CommitDetail{}, err
	}
	var meta struct {
		ID        string   `json:"id"`
		ParentIDs []string `json:"parent_ids"`
		Message   string   `json:"message"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/projects/%s/repository/commits/%s", seg, url.PathEscape(sha)), nil, &meta); err != nil {
		return providers.CommitDetail{}, err
	}
	var diffs []struct {
		OldPath     string `json:"old_path"`
		NewPath     string `json:"new_path"`
		NewFile     bool   `json:"new_file"`
		RenamedFile bool   `json:"renamed_file"`
		DeletedFile bool   `json:"deleted_file"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/projects/%s/repository/commits/%s/diff?per_page=100", seg, url.PathEscape(sha)), nil, &diffs); err != nil {
		return providers.CommitDetail{}, err
	}
	files := make([]providers.CommitFile, 0, len(diffs))
	for _, d := range diffs {
		switch {
		case d.NewFile:
			files = append(files, providers.CommitFile{Path: d.NewPath, Status: "added"})
		case d.DeletedFile:
			files = append(files, providers.CommitFile{Path: d.OldPath, Status: "removed"})
		case d.RenamedFile:
			files = append(files, providers.CommitFile{Path: d.NewPath, Status: "renamed", PreviousPath: d.OldPath})
		default:
			files = append(files, providers.CommitFile{Path: d.NewPath, Status: "modified"})
		}
	}
	return providers.CommitDetail{
		SHA:     meta.ID,
		Parents: meta.ParentIDs,
		Message: meta.Message,
		Files:   files,
	}, nil
}

// CompareChangedFiles returns all file paths modified between baseSHA and headSHA.
func (c *Client) CompareChangedFiles(ctx context.Context, repo, baseSHA, headSHA string) ([]string, error) {
	seg, err := repoSeg(repo)
	if err != nil {
		return nil, err
	}
	var cmp struct {
		Diffs []struct {
			OldPath string `json:"old_path"`
			NewPath string `json:"new_path"`
		} `json:"diffs"`
	}
	p := fmt.Sprintf("/projects/%s/repository/compare?from=%s&to=%s", seg, url.QueryEscape(baseSHA), url.QueryEscape(headSHA))
	if err := c.do(ctx, http.MethodGet, p, nil, &cmp); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	out := make([]string, 0, len(cmp.Diffs))
	for _, d := range cmp.Diffs {
		if d.NewPath != "" && !seen[d.NewPath] {
			seen[d.NewPath] = true
			out = append(out, d.NewPath)
		}
		if d.OldPath != "" && !seen[d.OldPath] {
			seen[d.OldPath] = true
			out = append(out, d.OldPath)
		}
	}
	return out, nil
}

// FileBlob returns the exact bytes of filePath at ref, or (nil, false, nil) if absent.
func (c *Client) FileBlob(ctx context.Context, repo, ref, filePath string) ([]byte, bool, error) {
	seg, err := repoSeg(repo)
	if err != nil {
		return nil, false, err
	}
	p := fmt.Sprintf("/projects/%s/repository/files/%s/raw?ref=%s", seg, url.PathEscape(filePath), url.QueryEscape(ref))
	raw, _, err := c.request(ctx, http.MethodGet, p, nil)
	if err != nil {
		if isNotFound(err) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return raw, true, nil
}

// FindOpenPRByHead returns an existing open merge request for headBranch if one exists.
func (c *Client) FindOpenPRByHead(ctx context.Context, repo, headBranch string) (mrURL string, number int, found bool, err error) {
	seg, err := repoSeg(repo)
	if err != nil {
		return "", 0, false, err
	}
	var mrs []struct {
		WebURL string `json:"web_url"`
		IID    int    `json:"iid"`
	}
	p := fmt.Sprintf("/projects/%s/merge_requests?state=opened&source_branch=%s&per_page=10", seg, url.QueryEscape(headBranch))
	if err := c.do(ctx, http.MethodGet, p, nil, &mrs); err != nil {
		return "", 0, false, err
	}
	if len(mrs) == 0 {
		return "", 0, false, nil
	}
	return mrs[0].WebURL, mrs[0].IID, true, nil
}

// CreateRevertDraftPR atomically creates the revert branch + inverse commit via
// the GitLab Repository Commits API and opens a Draft merge request.
func (c *Client) CreateRevertDraftPR(ctx context.Context, req providers.RevertPRRequest) (string, int, error) {
	seg, err := repoSeg(req.Repo)
	if err != nil {
		return "", 0, err
	}
	actions := make([]map[string]any, 0, len(req.Files))
	for _, f := range req.Files {
		if f.Delete {
			actions = append(actions, map[string]any{
				"action":    "delete",
				"file_path": f.Path,
			})
			continue
		}
		// Determine whether the file currently exists on BaseSHA to pick update vs create.
		ref := req.BaseSHA
		if ref == "" {
			ref = req.BaseBranch
		}
		_, exists, err := c.FileBlob(ctx, req.Repo, ref, f.Path)
		if err != nil {
			return "", 0, err
		}
		act := "create"
		if exists {
			act = "update"
		}
		actions = append(actions, map[string]any{
			"action":    act,
			"file_path": f.Path,
			"content":   base64.StdEncoding.EncodeToString(f.Content),
			"encoding":  "base64",
		})
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/repository/commits", seg), map[string]any{
		"branch":         req.HeadBranch,
		"start_branch":   req.BaseBranch,
		"commit_message": req.CommitMessage,
		"actions":        actions,
	}, nil); err != nil {
		return "", 0, err
	}
	title := req.Title
	if !strings.HasPrefix(title, "Draft: ") {
		title = "Draft: " + title
	}
	raw, _, err := c.request(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/merge_requests", seg), map[string]any{
		"title":                title,
		"source_branch":        req.HeadBranch,
		"target_branch":        req.BaseBranch,
		"description":          req.Body,
		"remove_source_branch": true,
	})
	if err != nil {
		return "", 0, err
	}
	var mr struct {
		WebURL string `json:"web_url"`
		IID    int    `json:"iid"`
	}
	if err := json.Unmarshal(raw, &mr); err != nil {
		return "", 0, err
	}
	return mr.WebURL, mr.IID, nil
}
