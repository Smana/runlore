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

var _ providers.IssueProvider = (*Client)(nil)

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

// OpenIssue files an issue describing the investigation.
func (c *Client) OpenIssue(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	body := map[string]any{"title": issueTitle(inv), "body": issueBody(inv), "labels": []string{"runlore"}}
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
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", c.owner, c.repo),
		map[string]any{"title": "KB: " + e.Title, "head": branch, "base": c.baseBranch, "body": "Drafted by RunLore."}, &out); err != nil {
		return providers.Ref{}, err
	}
	return providers.Ref{URL: out.HTMLURL}, nil
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
