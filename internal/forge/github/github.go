// Package github implements providers.IssueProvider over the GitHub REST API,
// authenticated with short-lived GitHub App installation tokens.
package github

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"gopkg.in/yaml.v3"
)

// TokenFunc returns a valid installation token (minted/cached by the caller).
type TokenFunc func(ctx context.Context) (string, error)

// Client is a GitHub forge client scoped to one repo.
type Client struct {
	baseURL    string
	owner      string
	repo       string
	baseBranch string
	token      TokenFunc
	http       *http.Client
}

// DefaultBaseURL is the public GitHub REST API. Override for GitHub Enterprise
// Server (e.g. https://ghe.example.com/api/v3) or tests.
const DefaultBaseURL = "https://api.github.com"

// New builds a client. baseURL may be empty (defaults to DefaultBaseURL);
// baseBranch is the PR target (e.g. "main").
func New(baseURL, owner, repo, baseBranch string, token TokenFunc) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"), owner: owner, repo: repo,
		baseBranch: baseBranch, token: token, http: &http.Client{Timeout: 30 * time.Second},
	}
}

var (
	_ providers.IssueProvider = (*Client)(nil)
	_ providers.ReinvestForge = (*Client)(nil)
)

// do performs an authenticated JSON request and decodes the response into out (if non-nil).
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	tok, err := c.token(ctx)
	if err != nil {
		return fmt.Errorf("token: %w", err)
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Accept", "application/vnd.github+json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("github %s %s: status %d: %s", method, path, resp.StatusCode, string(data[:min(len(data), 512)]))
	}
	if out != nil {
		return json.Unmarshal(data, out)
	}
	return nil
}

// lifecycleLabels are the labels applied to a freshly curated artifact. "triggered"
// is the first state of the KB lifecycle (triggered → investigating → solved); only
// a "solved" entry with a captured resolution should be merged as a Playbook.
var lifecycleLabels = []string{"runlore", "triggered"}

// OpenIssue files an issue describing the investigation.
func (c *Client) OpenIssue(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	body := map[string]any{"title": issueTitle(inv), "body": issueBody(inv), "labels": lifecycleLabels}
	var out struct {
		HTMLURL string `json:"html_url"`
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues", c.owner, c.repo), body, &out); err != nil {
		return providers.Ref{}, err
	}
	return providers.Ref{URL: out.HTMLURL}, nil
}

// OpenPR drafts the KB entry on a new branch and opens a PR.
func (c *Client) OpenPR(ctx context.Context, e providers.KBEntry) (providers.Ref, error) {
	slug := slugify(e.Title)
	branch := fmt.Sprintf("runlore/kb-%s-%d", slug, time.Now().Unix())
	path := slug + ".md"

	// 1. base ref SHA
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", c.owner, c.repo, c.baseBranch), nil, &ref); err != nil {
		return providers.Ref{}, err
	}
	// 2. create branch
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/refs", c.owner, c.repo),
		map[string]any{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA}, nil); err != nil {
		return providers.Ref{}, err
	}
	// 3. write the OKF file on the branch
	content := base64.StdEncoding.EncodeToString([]byte(renderEntry(e)))
	if err := c.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/%s/contents/%s", c.owner, c.repo, path),
		map[string]any{"message": "runlore: draft KB entry " + e.Title, "content": content, "branch": branch}, nil); err != nil {
		return providers.Ref{}, err
	}
	// 4. open the PR
	var out struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", c.owner, c.repo),
		map[string]any{"title": "KB: " + e.Title, "head": branch, "base": c.baseBranch, "body": "Drafted by RunLore."}, &out); err != nil {
		return providers.Ref{}, err
	}
	// 5. label the PR (the create-PR API doesn't accept labels; set them via the
	// issues endpoint). Best-effort: a labelling failure must not lose the PR, so
	// the error is intentionally ignored — the PR URL is already returned.
	if out.Number != 0 {
		_ = c.addLabels(ctx, out.Number, lifecycleLabels)
	}
	return providers.Ref{URL: out.HTMLURL}, nil
}

// addLabels POSTs labels onto an issue/PR (the create-PR API doesn't accept them).
func (c *Client) addLabels(ctx context.Context, number int, labels []string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues/%d/labels", c.owner, c.repo, number),
		map[string]any{"labels": labels}, nil)
}

// ListIssuesByLabel returns open issues carrying the given label. Pull requests
// (which the issues API also returns) are filtered out.
func (c *Client) ListIssuesByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	var raw []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		PullRequest *struct{} `json:"pull_request"`
	}
	path := fmt.Sprintf("/repos/%s/%s/issues?state=open&labels=%s", c.owner, c.repo, url.QueryEscape(label))
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var out []providers.CuratedIssue
	for _, i := range raw {
		if i.PullRequest != nil {
			continue // skip PRs
		}
		labels := make([]string, 0, len(i.Labels))
		for _, l := range i.Labels {
			labels = append(labels, l.Name)
		}
		out = append(out, providers.CuratedIssue{Number: i.Number, Title: i.Title, Body: i.Body, Labels: labels})
	}
	return out, nil
}

// ListPRsByLabel returns open PRs carrying the given label. GitHub's issues
// endpoint returns both issues and PRs; this keeps ONLY the entries that have a
// pull_request object (the inverse of ListIssuesByLabel).
func (c *Client) ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	var raw []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	}
	path := fmt.Sprintf("/repos/%s/%s/issues?state=open&labels=%s&per_page=100", c.owner, c.repo, url.QueryEscape(label))
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var out []providers.CuratedIssue
	for _, it := range raw {
		if it.PullRequest == nil {
			continue // a plain issue, not a PR
		}
		labels := make([]string, 0, len(it.Labels))
		for _, l := range it.Labels {
			labels = append(labels, l.Name)
		}
		out = append(out, providers.CuratedIssue{Number: it.Number, Title: it.Title, Body: it.Body, Labels: labels})
	}
	return out, nil
}

// Comment posts a comment on an issue.
func (c *Client) Comment(ctx context.Context, number int, body string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues/%d/comments", c.owner, c.repo, number),
		map[string]any{"body": body}, nil)
}

// ReplaceLabel removes one label and adds another (best-effort on the removal —
// a 404 when the label isn't present is not fatal).
func (c *Client) ReplaceLabel(ctx context.Context, number int, remove, add string) error {
	if remove != "" {
		// DELETE is best-effort: ignore "label not set" errors.
		_ = c.do(ctx, http.MethodDelete, fmt.Sprintf("/repos/%s/%s/issues/%d/labels/%s", c.owner, c.repo, number, url.PathEscape(remove)), nil, nil)
	}
	if add != "" {
		return c.addLabels(ctx, number, []string{add})
	}
	return nil
}

// Close closes an issue or PR (they share the issues endpoint for state).
func (c *Client) Close(ctx context.Context, number int) error {
	return c.do(ctx, http.MethodPatch, fmt.Sprintf("/repos/%s/%s/issues/%d", c.owner, c.repo, number),
		map[string]any{"state": "closed"}, nil)
}

func issueTitle(inv providers.Investigation) string {
	if inv.Title != "" {
		return inv.Title
	}
	return "RunLore investigation"
}

func issueBody(inv providers.Investigation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Confidence %.0f%%.\n\n", inv.Confidence*100)
	for i, rc := range inv.RootCauses {
		fmt.Fprintf(&b, "%d. %s (%.0f%%)\n", i+1, rc.Summary, rc.Confidence*100)
	}
	for _, u := range inv.Unresolved {
		fmt.Fprintf(&b, "- unresolved: %s\n", u)
	}
	return b.String()
}

// kbFrontmatter is the YAML frontmatter of an OKF entry. Marshaled (not string-
// formatted) so a newline-bearing title/description from LLM output can't inject
// extra frontmatter keys.
type kbFrontmatter struct {
	Type        string   `yaml:"type"`
	Title       string   `yaml:"title"`
	Description string   `yaml:"description"`
	Tags        []string `yaml:"tags,omitempty"`
}

// renderEntry serializes a KBEntry as OKF markdown (frontmatter + body).
func renderEntry(e providers.KBEntry) string {
	fm, _ := yaml.Marshal(kbFrontmatter{Type: e.Type, Title: e.Title, Description: e.Description, Tags: e.Tags})
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fm)
	b.WriteString("---\n\n")
	b.WriteString(e.Body)
	b.WriteString("\n")
	return b.String()
}

func slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
