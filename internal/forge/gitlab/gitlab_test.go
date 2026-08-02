// SPDX-License-Identifier: Apache-2.0

package gitlab

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func staticToken(tok string) TokenFunc {
	return func(context.Context) (string, error) { return tok, nil }
}

// isBundleRead reports whether r is one of the two OKF bundle reads OpenPR makes
// before it commits (index.md / log.md on the target branch).
func isBundleRead(r *http.Request) bool {
	return strings.Contains(r.URL.EscapedPath(), "/repository/files/")
}

// bundleAbsent answers a bundle read with a 404 — "this bundle has neither file".
// Tests that are not ABOUT bundle maintenance use it so they see the entry-only
// commit; the maintenance behaviour itself is pinned by TestOpenPRMaintainsBundle*.
func bundleAbsent(w http.ResponseWriter) {
	http.Error(w, `{"message":"404 File Not Found"}`, http.StatusNotFound)
}

// TestOpenPRURLEncodesProjectPath pins the single most common GitLab-client bug:
// the project path ("group/project") must be URL-encoded into the API path
// (group%2Fproject), or every call 404s in a way that looks like a permissions
// problem rather than an encoding one. It also covers a NESTED group path
// ("group/subgroup/project"), which has more than one slash to encode.
//
// It reads r.URL.EscapedPath(), not r.URL.Path: net/url auto-DECODES Path (so
// "%2F" and a literal "/" are indistinguishable there — the exact reason
// http.ServeMux patterns can't be used to assert this at all), while
// EscapedPath preserves what the client actually put on the wire.
func TestOpenPRURLEncodesProjectPath(t *testing.T) {
	var gotCommitPath, gotMRPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case isBundleRead(r):
			bundleAbsent(w)
		case strings.HasSuffix(r.URL.EscapedPath(), "/repository/commits"):
			gotCommitPath = r.URL.EscapedPath()
			_, _ = w.Write([]byte(`{"id":"abc123"}`))
		case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests"):
			gotMRPath = r.URL.EscapedPath()
			_, _ = w.Write([]byte(`{"iid":9,"web_url":"https://gitlab.example.com/group/subgroup/project/-/merge_requests/9"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "group/subgroup/project", "main", staticToken("tok"))
	ref, err := c.OpenPR(context.Background(), providers.KBEntry{Type: "Incident", Title: "DB outage", Body: "## body"})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if gotCommitPath == "" || gotMRPath == "" {
		t.Fatalf("requests did not hit the expected endpoints: commit=%q mr=%q", gotCommitPath, gotMRPath)
	}
	if !strings.Contains(gotCommitPath, "group%2Fsubgroup%2Fproject") || !strings.Contains(gotMRPath, "group%2Fsubgroup%2Fproject") {
		t.Fatalf("project path not URL-encoded: commit=%q mr=%q", gotCommitPath, gotMRPath)
	}
	if ref.URL != "https://gitlab.example.com/group/subgroup/project/-/merge_requests/9" {
		t.Fatalf("ref = %q", ref.URL)
	}
}

// TestOpenPR covers the full lifecycle described in the task brief: OpenPR
// creates a branch (via start_branch on the commit call), commits the entry,
// opens the merge request, and applies labels — all observable from the mock
// server's requests.
func TestOpenPR(t *testing.T) {
	var commitBody, mrBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case isBundleRead(r):
			bundleAbsent(w)
		case strings.HasSuffix(r.URL.EscapedPath(), "/repository/commits"):
			if got := r.Header.Get("PRIVATE-TOKEN"); got != "tok" {
				t.Fatalf("PRIVATE-TOKEN header = %q, want tok", got)
			}
			_ = json.NewDecoder(r.Body).Decode(&commitBody)
			_, _ = w.Write([]byte(`{"id":"deadbeef"}`))
		case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests"):
			_ = json.NewDecoder(r.Body).Decode(&mrBody)
			_, _ = w.Write([]byte(`{"iid":9,"web_url":"https://gitlab.com/o/r/-/merge_requests/9"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	ref, err := c.OpenPR(context.Background(), providers.KBEntry{Type: "Incident", Title: "DB outage", Body: "## body", Fingerprint: "deadbeefcafe"})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if ref.URL != "https://gitlab.com/o/r/-/merge_requests/9" {
		t.Fatalf("ref = %q", ref.URL)
	}

	// branch creation is folded into the commit call via start_branch
	if commitBody["branch"] == "" || commitBody["start_branch"] != "main" {
		t.Fatalf("commit body missing branch/start_branch: %+v", commitBody)
	}
	actions, ok := commitBody["actions"].([]any)
	if !ok || len(actions) == 0 {
		t.Fatalf("commit body missing actions: %+v", commitBody)
	}
	first, _ := actions[0].(map[string]any)
	if first["action"] != "create" || !strings.HasPrefix(first["file_path"].(string), "incidents/db-outage-") {
		t.Fatalf("unexpected commit action: %+v", first)
	}
	if content, _ := first["content"].(string); !strings.Contains(content, "## body") {
		t.Fatalf("commit content missing entry body: %q", content)
	}

	// the MR is opened with both lifecycle labels applied directly (GitLab's
	// create-MR call accepts labels; unlike GitHub there is no follow-up call).
	if mrBody["source_branch"] != commitBody["branch"] || mrBody["target_branch"] != "main" {
		t.Fatalf("unexpected MR branches: %+v", mrBody)
	}
	labels, _ := mrBody["labels"].(string)
	if labels != "runlore,triggered" {
		t.Fatalf("labels = %q, want runlore,triggered", labels)
	}
}

// TestOpenPRFingerprintMarkerRoundTrips proves the dedup fingerprint survives a
// round trip through the MR description exactly like the GitHub PR body.
func TestOpenPRFingerprintMarkerRoundTrips(t *testing.T) {
	var gotDescription string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case isBundleRead(r):
			bundleAbsent(w)
		case strings.HasSuffix(r.URL.EscapedPath(), "/repository/commits"):
			_, _ = w.Write([]byte(`{"id":"x"}`))
		case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			gotDescription, _ = body["description"].(string)
			_, _ = w.Write([]byte(`{"iid":1,"web_url":"https://gitlab.com/o/r/-/merge_requests/1"}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if _, err := c.OpenPR(context.Background(), providers.KBEntry{Type: "Incident", Title: "T", Body: "b", Fingerprint: "deadbeef"}); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if got := providers.ParseFingerprintMarker(gotDescription); got != "deadbeef" {
		t.Fatalf("fingerprint marker did not round-trip through the MR description: %q (got %q)", gotDescription, got)
	}
}

func TestListPRsByLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// EscapedPath, not Path: net/url has already DECODED Path, so "%2F" and a
		// literal "/" read identically there and a regression to an un-escaped
		// project path would sail through this assertion.
		if r.URL.EscapedPath() != "/api/v4/projects/o%2Fr/merge_requests" {
			t.Fatalf("unexpected path: %s", r.URL.EscapedPath())
		}
		if r.URL.Query().Get("state") != "opened" || r.URL.Query().Get("labels") != "runlore" {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[
		  {"iid":48,"title":"KB: HarborRegistryDown","description":"body text","labels":["runlore","triggered"],"updated_at":"2026-06-01T12:00:00Z"}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	prs, err := c.ListPRsByLabel(context.Background(), "runlore")
	if err != nil {
		t.Fatalf("ListPRsByLabel: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 48 || prs[0].Title != "KB: HarborRegistryDown" {
		t.Fatalf("want MR !48, got %+v", prs)
	}
	// Body must carry the MR DESCRIPTION. It is the field curator.duplicateOpenPR
	// scans for the hidden fingerprint marker, so mapping it to anything else (or
	// dropping it) silently kills GitLab dedup — every recurrence would open a
	// fresh duplicate MR with nothing erroring. See TestListPRsByLabelCarriesFingerprint.
	if prs[0].Body != "body text" {
		t.Fatalf("Body must carry the MR description, got %q", prs[0].Body)
	}
	if len(prs[0].Labels) != 2 || prs[0].Labels[0] != "runlore" {
		t.Fatalf("labels not parsed: %+v", prs[0].Labels)
	}
	if prs[0].UpdatedAt.IsZero() {
		t.Fatalf("updated_at not parsed: %+v", prs[0])
	}
}

// TestListPRsByLabelCarriesFingerprint drives the INBOUND half of the dedup
// round trip end to end: an MR description carrying a fingerprint marker (the
// shape OpenPR writes) must come back out of ListPRsByLabel intact enough for
// providers.ParseFingerprintMarker to recover the fingerprint. That pairing is
// the whole of GitLab dedup — curator.duplicateOpenPR does exactly this — and
// asserting only Number/Title/Labels leaves it untested in both directions.
func TestListPRsByLabelCarriesFingerprint(t *testing.T) {
	const fp = "cafebabedeadbeef"
	desc := "Drafted by RunLore — valkey down\n\n" + providers.FingerprintMarker(fp)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		body, _ := json.Marshal([]map[string]any{
			{"iid": 48, "title": "KB: HarborRegistryDown", "description": desc, "labels": []string{"runlore"}},
		})
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	prs, err := c.ListPRsByLabel(context.Background(), "runlore")
	if err != nil {
		t.Fatalf("ListPRsByLabel: %v", err)
	}
	if len(prs) != 1 {
		t.Fatalf("want 1 MR, got %d", len(prs))
	}
	if got := providers.ParseFingerprintMarker(prs[0].Body); got != fp {
		t.Fatalf("fingerprint did not survive the MR-description round trip: got %q want %q", got, fp)
	}
}

// pagedItems renders n list items with sequential iids starting at from.
func pagedItems(from, n int) []byte {
	items := make([]string, n)
	for i := range items {
		items[i] = fmt.Sprintf(`{"iid":%d,"title":"KB: t%d","labels":["runlore"]}`, from+i, from+i)
	}
	return []byte("[" + strings.Join(items, ",") + "]")
}

func TestListPRsByLabelPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			_, _ = w.Write(pagedItems(1, 100))
		case "2":
			w.Header().Set("X-Next-Page", "") // last page
			_, _ = w.Write(pagedItems(101, 1))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	prs, err := c.ListPRsByLabel(context.Background(), "runlore")
	if err != nil {
		t.Fatalf("ListPRsByLabel: %v", err)
	}
	if len(prs) != 101 {
		t.Fatalf("want 101 MRs across 2 pages (no truncation at 100), got %d", len(prs))
	}
}

// TestListPRsByLabelPaginatesBelowMaxPageSize is the regression this fix exists
// for: max_page_size is an instance-wide APPLICATION SETTING an admin can lower,
// so per_page=100 is a request, not a promise. On an instance capped at 50 the
// first page comes back short, and the old "a short page is the last page"
// heuristic stopped there — the curator's dedup then saw only the first 50 open
// MRs and filed a duplicate KB entry on every recurrence past them, silently.
func TestListPRsByLabelPaginatesBelowMaxPageSize(t *testing.T) {
	const instanceMax = 50
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			w.Header().Set("X-Next-Page", "2")
			_, _ = w.Write(pagedItems(1, instanceMax)) // clamped: short, but NOT last
		case "2":
			_, _ = w.Write(pagedItems(51, 10)) // no X-Next-Page ⇒ last page
		default:
			t.Errorf("unexpected page %q", r.URL.Query().Get("page"))
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	prs, err := c.ListPRsByLabel(context.Background(), "runlore")
	if err != nil {
		t.Fatalf("ListPRsByLabel: %v", err)
	}
	if len(prs) != instanceMax+10 {
		t.Fatalf("a page shorter than per_page must not end pagination: got %d, want %d", len(prs), instanceMax+10)
	}
}

// TestListPRsByLabelStopsOnUnboundedPagination pins the backstop. An instance (or
// a proxy) that ignores `page` serves page 1 forever with X-Next-Page still set;
// following the header alone would spin until the context died, hanging the
// curator. maxListPages bounds it, and the call still RETURNS rather than erroring
// — under-reporting a broken instance's backlog beats never returning.
func TestListPRsByLabelStopsOnUnboundedPagination(t *testing.T) {
	var requests int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		requests++
		w.Header().Set("X-Next-Page", "2") // always "there is more", never advances
		_, _ = w.Write(pagedItems(1, 1))
	}))
	defer srv.Close()
	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	prs, err := c.ListPRsByLabel(context.Background(), "runlore")
	if err != nil {
		t.Fatalf("ListPRsByLabel: %v", err)
	}
	if requests != maxListPages {
		t.Fatalf("pagination must stop at the %d-page cap, made %d requests", maxListPages, requests)
	}
	if len(prs) != maxListPages {
		t.Fatalf("want the %d collected items, got %d", maxListPages, len(prs))
	}
}

func TestListIssuesByLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.EscapedPath() != "/api/v4/projects/o%2Fr/issues" {
			t.Fatalf("unexpected path: %s", r.URL.EscapedPath())
		}
		if r.URL.Query().Get("state") != "opened" || r.URL.Query().Get("labels") != "reinvestigate" {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[{"iid":12,"title":"KB: Recurring","description":"b","labels":["runlore","reinvestigate"]}]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	issues, err := c.ListIssuesByLabel(context.Background(), "reinvestigate")
	if err != nil {
		t.Fatalf("ListIssuesByLabel: %v", err)
	}
	if len(issues) != 1 || issues[0].Number != 12 {
		t.Fatalf("want issue #12, got %+v", issues)
	}
}

// notesRecorder answers both notes endpoints and records which one was hit and
// with what body. Both iids EXIST in this mock — which is the realistic GitLab
// state (merge requests and issues have independent iid sequences, so in a KB
// project with open MRs, issue #9 and merge request !9 both exist) and precisely
// the state under which a kind-guessing client misroutes without erroring.
type notesRecorder struct {
	mrBody, issueBody string
	mrHits, issueHits int
}

func (n *notesRecorder) server(t *testing.T, iid int) *httptest.Server {
	t.Helper()
	read := func(r *http.Request) string {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		v, _ := body["body"].(string)
		return v
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.EscapedPath() {
		case fmt.Sprintf("/api/v4/projects/o%%2Fr/merge_requests/%d/notes", iid):
			n.mrHits++
			n.mrBody = read(r)
			_, _ = w.Write([]byte(`{}`))
		case fmt.Sprintf("/api/v4/projects/o%%2Fr/issues/%d/notes", iid):
			n.issueHits++
			n.issueBody = read(r)
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
			http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
		}
	}))
}

// TestCommentOnIssueReachesTheIssueEndpoint is the regression test for the
// misroute this API split exists to make impossible.
//
// The reinvestigate loop calls the issue-scoped method with an ISSUE iid. Under
// the previous "POST to merge_requests first, fall back to issues on 404" design
// this never even reached the issue: merge request !9 exists too, so the note
// landed on an unrelated merge request, GitLab answered 200, ReplaceLabel then
// correctly flipped the ISSUE's label — and the issue looked handled while its
// findings were nowhere near it. No error, no log, no way to notice.
func TestCommentOnIssueReachesTheIssueEndpoint(t *testing.T) {
	rec := &notesRecorder{}
	srv := rec.server(t, 9)
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if err := c.CommentOnIssue(context.Background(), 9, "re-investigated"); err != nil {
		t.Fatalf("CommentOnIssue: %v", err)
	}
	if rec.mrHits != 0 {
		t.Fatalf("an issue-scoped comment must never touch the merge-request notes endpoint (hit %d times)", rec.mrHits)
	}
	if rec.issueHits != 1 || rec.issueBody != "re-investigated" {
		t.Fatalf("issue notes endpoint: hits=%d body=%q", rec.issueHits, rec.issueBody)
	}
}

// TestCommentOnPRReachesTheMergeRequestEndpoint is the other half: the curation
// duplicate-coalesce path names a MERGE REQUEST iid, and must reach the MR notes
// endpoint even though an issue with the same iid also exists.
func TestCommentOnPRReachesTheMergeRequestEndpoint(t *testing.T) {
	rec := &notesRecorder{}
	srv := rec.server(t, 9)
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if err := c.CommentOnPR(context.Background(), 9, "coalesced"); err != nil {
		t.Fatalf("CommentOnPR: %v", err)
	}
	if rec.issueHits != 0 {
		t.Fatalf("a PR-scoped comment must never touch the issue notes endpoint (hit %d times)", rec.issueHits)
	}
	if rec.mrHits != 1 || rec.mrBody != "coalesced" {
		t.Fatalf("merge-request notes endpoint: hits=%d body=%q", rec.mrHits, rec.mrBody)
	}
}

// TestScopedCommentsDoNotFallBack pins the absence of the old fallback: a scoped
// comment whose artifact genuinely does not exist must ERROR, loudly, rather than
// quietly re-aiming at the other resource kind. A 404 here is a real signal (a
// closed/deleted artifact, a mis-encoded project path, a token without scope),
// and swallowing it is what made the original misroute invisible.
func TestScopedCommentsDoNotFallBack(t *testing.T) {
	for _, tc := range []struct {
		name    string
		call    func(*Client) error
		mustNot string
	}{
		{"issue-scoped", func(c *Client) error { return c.CommentOnIssue(context.Background(), 12, "x") }, "/merge_requests/"},
		{"pr-scoped", func(c *Client) error { return c.CommentOnPR(context.Background(), 12, "x") }, "/issues/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var paths []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				paths = append(paths, r.URL.EscapedPath())
				http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
			}))
			defer srv.Close()
			c := New(srv.URL, "o/r", "main", staticToken("tok"))
			if err := tc.call(c); err == nil {
				t.Fatal("want an error when the scoped artifact does not exist")
			}
			if len(paths) != 1 {
				t.Fatalf("want exactly one request (no fallback probe), got %v", paths)
			}
			if strings.Contains(paths[0], tc.mustNot) {
				t.Fatalf("request must not target the other resource kind: %s", paths[0])
			}
		})
	}
}

func TestReplaceLabel(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.EscapedPath() != "/api/v4/projects/o%2Fr/issues/7" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if err := c.ReplaceLabel(context.Background(), 7, "reinvestigate", "investigating"); err != nil {
		t.Fatalf("ReplaceLabel: %v", err)
	}
	if gotBody["remove_labels"] != "reinvestigate" || gotBody["add_labels"] != "investigating" {
		t.Fatalf("unexpected body: %+v", gotBody)
	}
}

func TestReplaceLabelAddOnly(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Assert the method and the exact wire path, not just the body: a label
		// flip aimed at /merge_requests/7 would relabel an unrelated merge request
		// (GitLab's iid sequences are independent) and a body-only assertion would
		// pass. EscapedPath for the same reason as TestListPRsByLabel.
		if r.Method != http.MethodPut || r.URL.EscapedPath() != "/api/v4/projects/o%2Fr/issues/7" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if err := c.ReplaceLabel(context.Background(), 7, "", "ready-to-merge"); err != nil {
		t.Fatalf("ReplaceLabel: %v", err)
	}
	if _, has := gotBody["remove_labels"]; has {
		t.Fatalf("remove_labels must be absent when remove is empty: %+v", gotBody)
	}
	if gotBody["add_labels"] != "ready-to-merge" {
		t.Fatalf("unexpected body: %+v", gotBody)
	}
}

// TestRetriesOn429And5xx pins the retry contract: a 429 or 5xx is retried and
// eventually succeeds.
func TestRetriesOn429And5xx(t *testing.T) {
	for _, status := range []int{http.StatusTooManyRequests, http.StatusInternalServerError, http.StatusBadGateway, http.StatusServiceUnavailable} {
		t.Run(fmt.Sprintf("status_%d", status), func(t *testing.T) {
			var attempts int
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				attempts++
				if attempts == 1 {
					w.WriteHeader(status)
					return
				}
				_, _ = w.Write([]byte(`[]`))
			}))
			defer srv.Close()
			c := New(srv.URL, "o/r", "main", staticToken("tok"))
			if _, err := c.ListPRsByLabel(context.Background(), "runlore"); err != nil {
				t.Fatalf("ListPRsByLabel: %v", err)
			}
			if attempts < 2 {
				t.Fatalf("status %d was not retried: attempts=%d", status, attempts)
			}
		})
	}
}

// TestNoRetryOn404 pins the other half of the retry contract: a 404 is a
// permanent failure (typically a mis-encoded project path or a bad token
// scope) and must NOT be retried.
func TestNoRetryOn404(t *testing.T) {
	var attempts int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		attempts++
		http.Error(w, `{"message":"404 Project Not Found"}`, http.StatusNotFound)
	}))
	defer srv.Close()
	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if _, err := c.ListPRsByLabel(context.Background(), "runlore"); err == nil {
		t.Fatal("want an error on a persistent 404")
	}
	if attempts != 1 {
		t.Fatalf("404 must not be retried, got %d attempts", attempts)
	}
}

func TestDefaultBaseURLIsGitLabCom(t *testing.T) {
	c := New("", "o/r", "main", staticToken("tok"))
	if !strings.HasPrefix(c.baseURL, "https://gitlab.com") {
		t.Fatalf("baseURL = %q, want it to default to gitlab.com", c.baseURL)
	}
}

func TestBlobURLSelfManaged(t *testing.T) {
	c := New("https://gitlab.example.com", "acme/kb", "main", nil)
	want := "https://gitlab.example.com/acme/kb/-/blob/main/incidents/a.md"
	if got := c.blobURL("incidents/a.md"); got != want {
		t.Errorf("blobURL = %q, want %q", got, want)
	}
}

func TestMRBodyRelatedKnowledge(t *testing.T) {
	c := New("", "acme/kb", "main", nil)
	e := providers.KBEntry{
		Title: "T", Description: "d", Fingerprint: "abc123",
		Related: []providers.RelatedEntry{
			{Path: "incidents/a.md", Title: "A", Resource: "apps/web", Score: 2.5},
		},
		Occurrences:    3,
		PrevCuratedURL: "https://kb/mr/12",
	}
	body := c.mrBody(e)
	for _, want := range []string{
		"## Related knowledge",
		"[A](https://gitlab.com/acme/kb/-/blob/main/incidents/a.md)",
		"score 2.50",
		"resource apps/web",
		"Trigger seen ×3",
		"previous entry: https://kb/mr/12",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("mrBody missing %q\n---\n%s", want, body)
		}
	}
	if got := providers.ParseFingerprintMarker(body); got != "abc123" {
		t.Errorf("ParseFingerprintMarker = %q, want abc123", got)
	}
}

func TestNeutralizeImages(t *testing.T) {
	got := neutralizeImages("before ![logo](https://cdn/logo.png) after")
	if got != "before `[image: logo]` after" {
		t.Fatalf("neutralizeImages = %q", got)
	}
}

func TestEntryPathFingerprinted(t *testing.T) {
	got := entryPath(providers.KBEntry{Type: "Incident", Title: "DB outage", Fingerprint: "deadbeefcafebabe"}, "db-outage", 123)
	if got != "incidents/db-outage-deadbeef.md" {
		t.Fatalf("entryPath = %q", got)
	}
}

func TestOpenPRTokenError(t *testing.T) {
	c := New("http://unused.invalid", "o/r", "main", func(context.Context) (string, error) {
		return "", fmt.Errorf("boom")
	})
	if _, err := c.OpenPR(context.Background(), providers.KBEntry{Title: "T"}); err == nil {
		t.Fatal("want an error when the token source fails")
	}
}

// bundleServer mocks the endpoints OpenPR touches, serving the given bundle
// files (path -> content; absent paths 404) and capturing the commit actions.
func bundleServer(t *testing.T, files map[string]string, actions *[]map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case isBundleRead(r):
			// GitLab wants the file path as ONE encoded segment; EscapedPath keeps
			// the "%2E"/"%2F" the client actually sent, so this also proves the
			// encoding rather than relying on net/url having decoded it for us.
			name := strings.TrimSuffix(strings.TrimPrefix(r.URL.EscapedPath(),
				"/api/v4/projects/o%2Fr/repository/files/"), "/raw")
			content, ok := files[name]
			if !ok {
				bundleAbsent(w)
				return
			}
			_, _ = w.Write([]byte(content))
		case strings.HasSuffix(r.URL.EscapedPath(), "/repository/commits"):
			var body struct {
				Actions []map[string]any `json:"actions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			*actions = body.Actions
			_, _ = w.Write([]byte(`{"id":"abc"}`))
		case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests"):
			_, _ = w.Write([]byte(`{"iid":3,"web_url":"https://gitlab.com/o/r/-/merge_requests/3"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
}

// findAction returns the commit action for file_path, if any.
func findAction(actions []map[string]any, path string) (map[string]any, bool) {
	for _, a := range actions {
		if a["file_path"] == path {
			return a, true
		}
	}
	return nil, false
}

func bundleKBEntry() providers.KBEntry {
	return providers.KBEntry{
		Type: "Incident", Title: "Harbor down", Description: "valkey down",
		Body: "## body", Fingerprint: "deadbeefcafebabe",
	}
}

// TestOpenPRMaintainsBundleFiles pins GitLab parity with github.Client.OpenPR:
// the merge request keeps the OKF bundle self-describing — index.md gains the
// entry's link line and log.md gains a chronological record. Without this a
// GitLab-hosted catalog keeps accumulating entries while its index and log rot,
// and nothing errors to say so.
//
// It also pins the structural win over GitHub: all three writes ride ONE commit
// call (GitHub needs four further round trips after creating its branch), so the
// bundle can never be left half-maintained on the branch.
func TestOpenPRMaintainsBundleFiles(t *testing.T) {
	var actions []map[string]any
	srv := bundleServer(t, map[string]string{
		"index.md": "# Catalog\n\n## Incidents\n",
		"log.md":   "# Knowledge catalog update log\n\n## 2026-06-20\n\n* **Creation**: Added [Old](o.md).\n",
	}, &actions)
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if _, err := c.OpenPR(context.Background(), bundleKBEntry()); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if len(actions) != 3 {
		t.Fatalf("want one commit carrying entry+index+log (3 actions), got %d: %+v", len(actions), actions)
	}
	entry, ok := findAction(actions, "incidents/harbor-down-deadbeef.md")
	if !ok || entry["action"] != "create" {
		t.Fatalf("entry action missing or not a create: %+v", actions)
	}
	idx, ok := findAction(actions, "index.md")
	if !ok || idx["action"] != "update" {
		t.Fatalf("index.md must be UPDATED when the bundle has one: %+v", actions)
	}
	if c, _ := idx["content"].(string); !strings.Contains(c, "- [Harbor down](incidents/harbor-down-deadbeef.md) — valkey down") {
		t.Fatalf("index.md not maintained: %q", c)
	}
	logAct, ok := findAction(actions, "log.md")
	if !ok || logAct["action"] != "update" {
		t.Fatalf("log.md must be UPDATED when the bundle has one: %+v", actions)
	}
	content, _ := logAct["content"].(string)
	if !strings.Contains(content, "* **Creation**: Added [Harbor down](incidents/harbor-down-deadbeef.md).") {
		t.Fatalf("log.md not maintained: %q", content)
	}
	if !strings.Contains(content, "## 2026-06-20") {
		t.Fatalf("log.md must preserve older entries: %q", content)
	}
}

// TestOpenPRSkipsIndexWhenAbsent: no index.md in the bundle → RunLore does not
// impose one (its structure is the owner's choice); log.md is still CREATED (its
// shape is fully specified by OKF §7). Same rule as the GitHub client.
func TestOpenPRSkipsIndexWhenAbsent(t *testing.T) {
	var actions []map[string]any
	srv := bundleServer(t, nil, &actions)
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if _, err := c.OpenPR(context.Background(), bundleKBEntry()); err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if _, ok := findAction(actions, "index.md"); ok {
		t.Fatalf("an absent index.md must not be created: %+v", actions)
	}
	logAct, ok := findAction(actions, "log.md")
	if !ok || logAct["action"] != "create" {
		t.Fatalf("log.md must be created when absent: %+v", actions)
	}
}

// TestOpenPRSurvivesBundleReadFailure: bundle upkeep is best-effort and must
// never cost the entry. A bundle read failing for any reason other than "absent"
// (a 500, a token whose scope covers merge requests but not repository files)
// leaves the entry committed and the merge request opened — the entry is the
// product, the index is the convenience.
func TestOpenPRSurvivesBundleReadFailure(t *testing.T) {
	var actions []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case isBundleRead(r):
			http.Error(w, `{"message":"403 Forbidden"}`, http.StatusForbidden)
		case strings.HasSuffix(r.URL.EscapedPath(), "/repository/commits"):
			var body struct {
				Actions []map[string]any `json:"actions"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			actions = body.Actions
			_, _ = w.Write([]byte(`{"id":"abc"}`))
		case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests"):
			_, _ = w.Write([]byte(`{"iid":3,"web_url":"https://gitlab.com/o/r/-/merge_requests/3"}`))
		default:
			t.Errorf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	ref, err := c.OpenPR(context.Background(), bundleKBEntry())
	if err != nil {
		t.Fatalf("a bundle read failure must not fail OpenPR: %v", err)
	}
	if ref.URL == "" {
		t.Fatal("merge request was not opened")
	}
	if len(actions) != 1 {
		t.Fatalf("want the entry action alone when bundle reads fail, got %+v", actions)
	}
}
