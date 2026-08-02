// SPDX-License-Identifier: Apache-2.0

// Package gitlab is RunLore's GitLab forge client (curation + re-investigation)
// over the GitLab REST API v4, authenticated with a project or group access
// token sent as the PRIVATE-TOKEN header. It satisfies providers.CurationForge
// and providers.ReinvestForge — the same contract internal/forge/github meets —
// so a self-hosted GitLab team gets the same Learn-loop curation a GitHub team
// gets, with no forge-specific code above this package.
//
// GitLab concepts map onto GitHub's: merge request → pull request, note →
// comment, the Commits API (with actions + start_branch) → branch + commit.
// Structure deliberately mirrors internal/forge/github/github.go; see the
// package-level doc comments below for where GitLab's shape genuinely differs
// and why (project-path URL-encoding, one-call branch+commit, direct label
// application on MR create, and the merge-request/issue notes split).
package gitlab

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

// TokenFunc returns a valid PRIVATE-TOKEN value (a project or group access
// token). Unlike GitHub's TokenFunc, which mints and caches a short-lived App
// installation token, GitLab has no App-equivalent bot identity — the token is
// a static credential read from an env var, so TokenFunc is typically just a
// closure over os.Getenv's result. The type stays a func (not a bare string)
// for parity with github.TokenFunc and so a future rotating-token source can
// swap in without changing the Client's shape.
type TokenFunc func(ctx context.Context) (string, error)

// Client is a GitLab forge client scoped to one project.
type Client struct {
	baseURL     string // instance root + "/api/v4", e.g. https://gitlab.com/api/v4
	projectPath string // "group/project" or a nested "group/subgroup/project"
	baseBranch  string
	token       TokenFunc
	http        *http.Client
}

// DefaultBaseURL is the public gitlab.com instance root. Override for a
// self-managed instance (e.g. https://gitlab.example.com) or tests; New
// appends the "/api/v4" API suffix itself, so config's base_url is the plain
// instance root exactly as a browser URL reads.
const DefaultBaseURL = "https://gitlab.com"

const apiSuffix = "/api/v4"

// lifecycleLabels are the labels applied to a freshly curated artifact,
// matching github.Client's "runlore" + "triggered" (see providers.KBEntry /
// the OKF lifecycle: triggered → investigating → solved).
var lifecycleLabels = []string{"runlore", "triggered"}

// New builds a client. baseURL is the GitLab INSTANCE root (empty defaults to
// gitlab.com); projectPath is the project's namespace path ("group/project",
// or a nested "group/subgroup/project") — NOT URL-encoded here, encoding
// happens per-request (see projectSeg); baseBranch is the MR target branch
// (e.g. "main").
func New(baseURL, projectPath, baseBranch string, token TokenFunc) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL:     strings.TrimRight(baseURL, "/") + apiSuffix,
		projectPath: projectPath,
		baseBranch:  baseBranch,
		token:       token,
		http:        httpx.SecureClient(30 * time.Second),
	}
}

var _ providers.CurationForge = (*Client)(nil)
var _ providers.ReinvestForge = (*Client)(nil)

// projectSeg is the URL-encoded project-path path segment GitLab's v4 API
// requires: every "/" in the path must become "%2F" (url.PathEscape does this
// for a path SEGMENT — unlike url.QueryEscape, which would encode "/" as "+"
// and still be wrong). This is THE most common GitLab-client bug: get it
// wrong and every call 404s in a way that looks like a permissions problem,
// not an encoding one. A nested group path ("group/sub/project") has more
// than one "/" to encode; PathEscape handles all of them.
func (c *Client) projectSeg() string {
	return url.PathEscape(c.projectPath)
}

// statusError carries the HTTP status code alongside the formatted message so
// callers can branch on it (errors.As) without parsing strings — used by
// Comment's merge-request/issue fallback and DoWithRetry's own status check.
type statusError struct {
	method, path string
	status       int
	body         string
}

func (e *statusError) Error() string {
	return fmt.Sprintf("gitlab %s %s: status %d: %s", e.method, e.path, e.status, e.body)
}

// isNotFound reports whether err is a statusError carrying a 404 — the same
// "a number that doesn't exist as THIS resource type" signal github.Client's
// IsPROpen relies on, reused here to disambiguate an MR iid from an issue iid
// in Comment (see its doc comment).
func isNotFound(err error) bool {
	var se *statusError
	return errors.As(err, &se) && se.status == http.StatusNotFound
}

// do performs an authenticated JSON request and decodes the response into out
// (if non-nil). It retries on a network error, 429, or 5xx via httpx.DoWithRetry
// (already used by internal/mcp and internal/embed) — a self-hosted GitLab
// instance under load, or gitlab.com's own rate limiting, is a realistic
// operational case, so retry/backoff is built in here from the start rather
// than left for a later pass. A 404 (or any other 4xx) is never retried — see
// isNotFound, which some callers rely on for fast, deliberate fallback.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	tok, err := c.token(ctx)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	var raw []byte
	if body != nil {
		raw, err = json.Marshal(body)
		if err != nil {
			return err
		}
	}
	resp, err := httpx.DoWithRetry(ctx, c.http, 3, func() (*http.Request, error) {
		var rdr io.Reader
		if raw != nil {
			rdr = bytes.NewReader(raw)
		}
		req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
		if err != nil {
			return nil, err
		}
		req.Header.Set("PRIVATE-TOKEN", tok)
		req.Header.Set("Accept", "application/json")
		if raw != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		return req, nil
	})
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return &statusError{method: method, path: path, status: resp.StatusCode, body: string(data[:min(len(data), 512)])}
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// OpenPR drafts the KB entry on a new branch and opens a merge request.
//
// This is the biggest structural divergence from github.Client.OpenPR, which
// needs THREE calls before the PR (get the base ref SHA, create the branch,
// PUT the file) plus a fourth AFTER (apply labels — the create-PR call can't
// carry them). GitLab folds the first three into ONE call: the Commits API
// creates `branch` from `start_branch` when `branch` doesn't already exist, so
// there is no separate branch-creation round trip. And GitLab's create-MR call
// accepts `labels` directly, so there is no follow-up label call either — the
// whole lifecycle is two requests instead of five.
func (c *Client) OpenPR(ctx context.Context, e providers.KBEntry) (providers.Ref, error) {
	slug := slugify(e.Title)
	now := time.Now().Unix()
	branch := fmt.Sprintf("runlore/kb-%s-%d", slug, now)
	path := entryPath(e, slug, now)

	commitBody := map[string]any{
		"branch":         branch,
		"start_branch":   c.baseBranch,
		"commit_message": "runlore: draft KB entry " + e.Title,
		"actions": []map[string]any{
			{"action": "create", "file_path": path, "content": renderEntry(e)},
		},
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/repository/commits", c.projectSeg()), commitBody, nil); err != nil {
		return providers.Ref{}, err
	}

	var out struct {
		WebURL string `json:"web_url"`
		IID    int    `json:"iid"`
	}
	mrBody := map[string]any{
		"source_branch": branch,
		"target_branch": c.baseBranch,
		"title":         "KB: " + e.Title,
		"description":   c.mrBody(e),
		"labels":        strings.Join(lifecycleLabels, ","),
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/merge_requests", c.projectSeg()), mrBody, &out); err != nil {
		return providers.Ref{}, err
	}
	return providers.Ref{URL: out.WebURL}, nil
}

// rawItem is the common shape of a GitLab merge-request or issue list item —
// both carry iid/title/description/labels/updated_at, and (unlike GitHub,
// whose one issues endpoint returns both kinds so a PullRequest field is
// needed to tell them apart) MRs and issues come from entirely separate
// endpoints here, so no such discriminator is needed.
type rawItem struct {
	IID         int       `json:"iid"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	UpdatedAt   time.Time `json:"updated_at"`
	Labels      []string  `json:"labels"`
}

func (r rawItem) curated() providers.CuratedIssue {
	return providers.CuratedIssue{Number: r.IID, Title: r.Title, Body: r.Description, Labels: r.Labels, UpdatedAt: r.UpdatedAt}
}

// listPaged fetches ALL pages of resource ("merge_requests" | "issues") in the
// given state carrying label. GitLab caps a page at 100 like GitHub; without
// this loop a label search past the first 100 would be silently truncated.
func (c *Client) listPaged(ctx context.Context, resource, state, label string) ([]providers.CuratedIssue, error) {
	var all []providers.CuratedIssue
	for page := 1; ; page++ {
		var raw []rawItem
		path := fmt.Sprintf("/projects/%s/%s?state=%s&labels=%s&per_page=100&page=%d",
			c.projectSeg(), resource, state, url.QueryEscape(label), page)
		if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			all = append(all, r.curated())
		}
		if len(raw) < 100 {
			break
		}
	}
	return all, nil
}

// ListPRsByLabel returns all open merge requests carrying the given label.
func (c *Client) ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	return c.listPaged(ctx, "merge_requests", "opened", label)
}

// ListIssuesByLabel returns all open issues carrying the given label.
func (c *Client) ListIssuesByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	return c.listPaged(ctx, "issues", "opened", label)
}

// Comment posts a note on a merge request OR an issue, identified only by its
// project-scoped iid.
//
// This is the sharpest divergence from GitHub: GitHub gives issues and pull
// requests ONE shared number space and ONE shared comments endpoint, so
// Comment(number) is never ambiguous. GitLab does not — merge requests and
// issues are separate resources, each with its OWN iid sequence starting at 1
// and its OWN notes endpoint. The providers.CurationForge / ReinvestForge
// interfaces pass only a bare int (no "this is an MR" flag), because that
// contract was shaped by GitHub's model. So Comment tries the merge-request
// notes endpoint first (the far more common caller: curation's duplicate-PR
// coalesce) and falls back to the issue notes endpoint on a 404 (the
// reinvestigate path, which always names an issue) — the same 404-as-type-
// discriminator idiom github.Client.IsPROpen already relies on.
func (c *Client) Comment(ctx context.Context, number int, body string) error {
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/merge_requests/%d/notes", c.projectSeg(), number),
		map[string]any{"body": body}, nil)
	if isNotFound(err) {
		return c.do(ctx, http.MethodPost, fmt.Sprintf("/projects/%s/issues/%d/notes", c.projectSeg(), number),
			map[string]any{"body": body}, nil)
	}
	return err
}

// ReplaceLabel removes one label and adds another on an issue (the only
// caller, investigate.Reinvestigator, always names an issue iid). Unlike
// GitHub — which needs a DELETE call for the removed label plus a POST for the
// added one — GitLab's issue-edit endpoint takes add_labels/remove_labels
// directly, so this is a single PUT.
func (c *Client) ReplaceLabel(ctx context.Context, number int, remove, add string) error {
	body := map[string]any{}
	if remove != "" {
		body["remove_labels"] = remove
	}
	if add != "" {
		body["add_labels"] = add
	}
	if len(body) == 0 {
		return nil
	}
	return c.do(ctx, http.MethodPut, fmt.Sprintf("/projects/%s/issues/%d", c.projectSeg(), number), body, nil)
}

// mrBody is the GitLab MR description: mirrors github.Client.prBody exactly
// (same one-line why-keep summary, same Related-knowledge reviewer section,
// same trailing hidden fingerprint marker for dedup) — only the field name
// (GitLab: description, GitHub: body) and the blob URL shape differ.
func (c *Client) mrBody(e providers.KBEntry) string {
	desc := e.Description
	if desc == "" {
		desc = e.Title
	}
	body := fmt.Sprintf("Drafted by RunLore — %s\n\nReview the decision card + OKF entry in the changed file.", desc)
	if s := c.relatedSection(e); s != "" {
		body += "\n\n" + s
	}
	if m := providers.FingerprintMarker(e.Fingerprint); m != "" {
		body += "\n\n" + m
	}
	return neutralizeImages(body)
}

// relatedSection renders the reviewer context: the draft-time BM25 neighborhood
// (linked, scored) and the trigger's recurrence line. Empty when there is
// nothing to say. Identical logic to github.Client.relatedSection.
func (c *Client) relatedSection(e providers.KBEntry) string {
	if len(e.Related) == 0 && e.Occurrences <= 1 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Related knowledge\n")
	for _, r := range e.Related {
		fmt.Fprintf(&b, "\n- [%s](%s) — score %.2f", r.Title, c.blobURL(r.Path), r.Score)
		if r.Resource != "" {
			fmt.Fprintf(&b, " · resource %s", r.Resource)
		}
	}
	if e.Occurrences > 1 {
		fmt.Fprintf(&b, "\n\nTrigger seen ×%d", e.Occurrences)
		if e.PrevCuratedURL != "" {
			fmt.Fprintf(&b, " · previous entry: %s", e.PrevCuratedURL)
		}
	}
	return b.String()
}

// blobURL is the web URL of a catalog file on the base branch. GitLab's blob
// path is namespace/project/-/blob/<branch>/<path> — the "/-/" separator marks
// a project's scoped pages (blob, tree, issues, merge_requests, …), unlike
// GitHub's plain /blob/<branch>/<path>. host is the instance root with the
// "/api/v4" suffix stripped back off.
func (c *Client) blobURL(path string) string {
	host := strings.TrimSuffix(c.baseURL, apiSuffix)
	branch := c.baseBranch
	if branch == "" {
		branch = "main"
	}
	return fmt.Sprintf("%s/%s/-/blob/%s/%s", host, c.projectPath, branch, path)
}

// imageRe matches Markdown image syntax: ![alt text](url) — identical to
// github.Client's imageRe. Duplicated rather than shared: it is a small (few-
// line), stable, already-tested-elsewhere helper, and duplicating it here
// keeps internal/forge/github and internal/forge/gitlab independently
// buildable with no cross-package coupling for one regex.
var imageRe = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)

// neutralizeImages replaces every Markdown image in untrusted body text with a
// non-fetching inline code span, so an attacker-influenced investigation can't
// plant an image-beacon URL that GitLab auto-fetches on render (the same
// concern github.Client.neutralizeImages addresses for GitHub-rendered
// surfaces). Applied to all body text that reaches GitLab-rendered surfaces
// (the MR description, the KB entry file body).
func neutralizeImages(s string) string {
	return imageRe.ReplaceAllStringFunc(s, func(m string) string {
		alt := imageRe.FindStringSubmatch(m)
		if len(alt) < 2 || alt[1] == "" {
			return "`[image]`"
		}
		return "`[image: " + alt[1] + "]`"
	})
}

// renderEntry serializes a KBEntry as OKF markdown (frontmatter + body),
// identical to github.Client's renderEntry: the timestamp is stamped at
// render time, last_validated stays unset (a draft claims no human
// confirmation), and the body is neutralized before it reaches a GitLab-
// rendered surface.
func renderEntry(e providers.KBEntry) string {
	e.Body = neutralizeImages(e.Body)
	return okf.Render(e, okf.Meta{Timestamp: time.Now().UTC().Format(time.RFC3339)})
}

// entryPath is where the drafted entry lives in the KB bundle: a type
// directory plus the title slug suffixed with a short fingerprint, so two
// different incidents sharing a title don't collide on one path. Identical to
// github.Client's entryPath.
func entryPath(e providers.KBEntry, slug string, now int64) string {
	suffix := e.Fingerprint
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	if suffix == "" {
		suffix = fmt.Sprintf("%d", now)
	}
	return fmt.Sprintf("%ss/%s-%s.md", strings.ToLower(e.Type), slug, suffix)
}

func slugify(s string) string { return okf.Slugify(s) }
