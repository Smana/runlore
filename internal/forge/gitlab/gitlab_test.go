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
		if r.URL.Path != "/api/v4/projects/o/r/merge_requests" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
		}
		if r.URL.Query().Get("state") != "opened" || r.URL.Query().Get("labels") != "runlore" {
			t.Fatalf("unexpected query: %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`[
		  {"iid":48,"title":"KB: HarborRegistryDown","description":"b","labels":["runlore","triggered"],"updated_at":"2026-06-01T12:00:00Z"}
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
	if len(prs[0].Labels) != 2 || prs[0].Labels[0] != "runlore" {
		t.Fatalf("labels not parsed: %+v", prs[0].Labels)
	}
	if prs[0].UpdatedAt.IsZero() {
		t.Fatalf("updated_at not parsed: %+v", prs[0])
	}
}

func TestListPRsByLabelPaginates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			items := make([]string, 100)
			for i := range items {
				items[i] = fmt.Sprintf(`{"iid":%d,"title":"KB: t%d","labels":["runlore"]}`, i+1, i+1)
			}
			_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
		case "2":
			_, _ = w.Write([]byte(`[{"iid":101,"title":"KB: t101","labels":["runlore"]}]`))
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

func TestListIssuesByLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v4/projects/o/r/issues" {
			t.Fatalf("unexpected path: %s", r.URL.Path)
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

// TestCommentPrefersMergeRequestNotes covers the curation-coalesce path: a
// Comment call for an MR number posts to the merge-request notes endpoint
// directly (no fallback needed).
func TestCommentPrefersMergeRequestNotes(t *testing.T) {
	var hitMR, hitIssue bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests/9/notes"):
			hitMR = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["body"] != "hello" {
				t.Fatalf("unexpected note body: %+v", body)
			}
			_, _ = w.Write([]byte(`{}`))
		case strings.HasSuffix(r.URL.EscapedPath(), "/issues/9/notes"):
			hitIssue = true
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if err := c.Comment(context.Background(), 9, "hello"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if !hitMR || hitIssue {
		t.Fatalf("want only the MR notes endpoint hit, got mr=%v issue=%v", hitMR, hitIssue)
	}
}

// TestCommentFallsBackToIssueNotes covers the reinvestigate path: GitLab keeps
// merge requests and issues as separate iid sequences with separate notes
// endpoints (unlike GitHub, where one issues-comments endpoint serves both).
// A number that is NOT an open MR (404 on the merge_requests notes endpoint)
// falls back to the issue notes endpoint.
func TestCommentFallsBackToIssueNotes(t *testing.T) {
	var hitIssue bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.EscapedPath(), "/merge_requests/12/notes"):
			http.Error(w, `{"message":"404 Not found"}`, http.StatusNotFound)
		case strings.HasSuffix(r.URL.EscapedPath(), "/issues/12/notes"):
			hitIssue = true
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			if body["body"] != "re-investigated" {
				t.Fatalf("unexpected note body: %+v", body)
			}
			_, _ = w.Write([]byte(`{}`))
		default:
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.EscapedPath())
		}
	}))
	defer srv.Close()

	c := New(srv.URL, "o/r", "main", staticToken("tok"))
	if err := c.Comment(context.Background(), 12, "re-investigated"); err != nil {
		t.Fatalf("Comment: %v", err)
	}
	if !hitIssue {
		t.Fatal("want the issue notes fallback to be hit after the MR notes 404")
	}
}

func TestReplaceLabel(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut || r.URL.Path != "/api/v4/projects/o/r/issues/7" {
			t.Fatalf("unexpected request: %s %s", r.Method, r.URL.Path)
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
