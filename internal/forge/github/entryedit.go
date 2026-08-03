// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// entryEdit describes one surgical, human-reviewed frontmatter edit to a MERGED
// catalog entry: fetch the entry on the base branch, run stamp over its bytes,
// and open a PR carrying the result. Retirement (`status: retired`) and
// revalidation (`last_validated: <date>`) are the two instances; they differ
// only in the stamp and the PR's naming, so the six-call GitHub dance lives here
// once rather than being copy-pasted per pass — the two can never drift apart on
// branch naming, sha handling, or labelling.
type entryEdit struct {
	path        string                       // entry path on the base branch
	stamp       func([]byte) ([]byte, error) // the frontmatter edit; a sentinel error means "nothing to do"
	verb        string                       // "retire" | "revalidate": names the branch AND the commit
	titlePrefix string                       // PR title prefix
	labels      []string                     // applied after the PR opens; a failure here IS an error (step 6)
	body        string                       // reviewer-facing body (carries the caller's hidden marker)
}

// openEntryEditPR performs the fetch → stamp → branch → commit → PR → label
// sequence for one entryEdit. It never merges and never deletes: the PR is the
// proposal and a human is the load-bearing gate. A stamp sentinel (already
// retired, recently validated, inactive entry) is wrapped with the entry path and
// returned, so `errors.Is` still identifies it and the caller's pass can treat it
// as a done-skip; a 404 on the entry file surfaces as an error (entry deleted →
// the pass logs and skips it).
func (c *Client) openEntryEditPR(ctx context.Context, e entryEdit) (providers.Ref, error) {
	// 1. fetch the entry on the base branch: its content and blob sha.
	var file struct {
		Content string `json:"content"`
		SHA     string `json:"sha"`
	}
	if err := c.do(ctx, http.MethodGet,
		fmt.Sprintf("/repos/%s/%s/contents/%s?ref=%s", c.owner, c.repo, e.path, c.baseBranch), nil, &file); err != nil {
		return providers.Ref{}, err
	}
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(file.Content, "\n", ""))
	if err != nil {
		return providers.Ref{}, fmt.Errorf("decode %s: %w", e.path, err)
	}
	stamped, err := e.stamp(raw)
	if err != nil {
		return providers.Ref{}, fmt.Errorf("%s: %w", e.path, err)
	}

	// 2. base ref SHA.
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/git/ref/heads/%s", c.owner, c.repo, c.baseBranch), nil, &ref); err != nil {
		return providers.Ref{}, err
	}
	// 3. create the edit branch.
	branch := fmt.Sprintf("runlore/%s-%s-%d", e.verb, slugify(e.path), time.Now().Unix())
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/git/refs", c.owner, c.repo),
		map[string]any{"ref": "refs/heads/" + branch, "sha": ref.Object.SHA}, nil); err != nil {
		return providers.Ref{}, err
	}
	// 4. update the entry file in place — the file sha makes this an update, not a create.
	if err := c.do(ctx, http.MethodPut, fmt.Sprintf("/repos/%s/%s/contents/%s", c.owner, c.repo, e.path),
		map[string]any{
			"message": "runlore: " + e.verb + " " + e.path,
			"content": base64.StdEncoding.EncodeToString(stamped),
			"branch":  branch,
			"sha":     file.SHA,
		}, nil); err != nil {
		return providers.Ref{}, err
	}
	// 5. open the PR.
	var out struct {
		HTMLURL string `json:"html_url"`
		Number  int    `json:"number"`
	}
	if err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/pulls", c.owner, c.repo),
		map[string]any{"title": e.titlePrefix + e.path, "head": branch, "base": c.baseBranch, "body": e.body}, &out); err != nil {
		return providers.Ref{}, err
	}
	// 6. label the PR. NOT best-effort, unlike OpenPR: for these proposals the label
	// is the only index that exists. The pass finds its own open PRs, its human
	// vetoes and its queue depth by listing on it, so an unlabelled proposal is
	// invisible — the hidden marker in its body is never scanned, it never counts
	// against max_open, and the next sweep opens another one for the same entry,
	// every sweep, without bound. Silently arming that loop is worse than failing
	// loudly, so the error carries the PR's URL: the PR really is open, and a human
	// must label or close it. The pass isolates this per entry, so one such failure
	// never starves the rest of the sweep.
	if out.Number != 0 {
		if err := c.addLabels(ctx, out.Number, e.labels); err != nil {
			return providers.Ref{}, fmt.Errorf(
				"%s is open but UNLABELLED, so the %s pass cannot see it — label or close it by hand: %w",
				out.HTMLURL, e.verb, err)
		}
	}
	return providers.Ref{URL: out.HTMLURL}, nil
}

// frontmatterBlock splits an OKF entry into its frontmatter lines and the
// remainder (everything from the closing fence on, returned verbatim so a
// surgical edit can rejoin the file byte-for-byte). A file without a
// frontmatter block errors: a frontmatter stamp must never write blind.
func frontmatterBlock(content []byte) (lines []string, rest string, err error) {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") {
		return nil, "", fmt.Errorf("entry has no YAML frontmatter block")
	}
	body := s[len("---\n"):]
	end := strings.Index(body, "\n---")
	if end < 0 {
		return nil, "", fmt.Errorf("entry frontmatter block is unterminated")
	}
	return strings.Split(body[:end], "\n"), body[end:], nil
}

// frontmatterValue reads one frontmatter line as key, scalar and trailing comment.
// ok is false for a line carrying no key at all. Both stamps hand-parse the block
// (a re-marshal would reformat a human-authored artifact under review), so this is
// the single place that decides what the value of a line actually is.
func frontmatterValue(line string) (key, scalar, comment string, ok bool) {
	k, afterColon, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", "", false
	}
	scalar, comment = scalarAndComment(afterColon)
	return strings.TrimSpace(k), scalar, comment, true
}

// scalarAndComment splits the text after a key's colon into its scalar and any
// trailing comment, the comment carrying the whitespace that separated the two so
// a surgical edit can re-emit it unchanged.
//
// A YAML reader does not see a comment as part of a value, and neither may we. A
// perfectly legal `last_validated: 2026-07-20  # confirmed by alice` otherwise
// parses as an unreadable date: the entry looks like it has nothing on record, so
// the anti-spam gate never fires, the entry is restamped on every sweep, and the
// human's note is destroyed each time.
//
// A '#' opens a comment only when it follows whitespace (or opens the value) AND
// sits outside quotes — `foo#bar` is the scalar "foo#bar", and a '#' inside a
// quoted scalar is data.
func scalarAndComment(afterColon string) (scalar, comment string) {
	var quote byte
	for i := range len(afterColon) {
		c := afterColon[i]
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '#' && (i == 0 || afterColon[i-1] == ' ' || afterColon[i-1] == '\t'):
			sep := i
			for sep > 0 && (afterColon[sep-1] == ' ' || afterColon[sep-1] == '\t') {
				sep--
			}
			return afterColon[:sep], afterColon[sep:]
		}
	}
	return afterColon, ""
}

// unquoteScalar strips the surrounding quotes yaml.Marshal adds to a scalar
// (okf.Render emits `last_validated: "2026-08-03T10:00:00Z"`), so a hand-parsed
// value means what a YAML reader would say it means.
func unquoteScalar(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// inactiveStatus reports whether a raw frontmatter status scalar names one of the
// two states recall refuses to fire (investigate's entryActive). It reads the
// value exactly as a YAML reader would — quotes stripped, case-insensitive —
// because `status: "retired"` is retired to everything that loads the catalog,
// and a stamp that disagreed would edit an entry recall has already written off.
// An absent or foreign status stays active, per OKF §9 tolerance.
func inactiveStatus(scalar string) bool {
	s := strings.ToLower(unquoteScalar(scalar))
	return s == "retired" || s == "draft"
}
