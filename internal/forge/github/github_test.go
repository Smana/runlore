package github

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func staticToken(string) func(context.Context) (string, error) {
	return func(context.Context) (string, error) { return "tok", nil }
}

func TestOpenIssue(t *testing.T) {
	var gotAuth, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotPath = r.Header.Get("Authorization"), r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/o/r/issues/7"}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	ref, err := c.OpenIssue(context.Background(), providers.Investigation{Title: "Boom", Confidence: 0.4,
		RootCauses: []providers.Hypothesis{{Summary: "db down"}}})
	if err != nil {
		t.Fatalf("OpenIssue: %v", err)
	}
	if gotAuth != "Bearer tok" || gotPath != "/repos/o/r/issues" {
		t.Fatalf("auth=%q path=%q", gotAuth, gotPath)
	}
	if title, _ := gotBody["title"].(string); title != "Boom" {
		t.Fatalf("title=%v", gotBody["title"])
	}
	if ref.URL != "https://github.com/o/r/issues/7" {
		t.Fatalf("ref=%s", ref.URL)
	}
}

func TestListPRsByLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/issues" || r.URL.Query().Get("labels") != "runlore" || r.URL.Query().Get("state") != "open" {
			t.Fatalf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		// one PR (has pull_request), one plain issue (no pull_request) → only the PR is returned
		_, _ = w.Write([]byte(`[
		  {"number":48,"title":"KB: HarborRegistryDown","body":"b","labels":[{"name":"runlore"},{"name":"triggered"}],"pull_request":{"url":"x"}},
		  {"number":39,"title":"Harbor install failing","body":"b","labels":[{"name":"runlore"}]}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	prs, err := c.ListPRsByLabel(context.Background(), "runlore")
	if err != nil {
		t.Fatalf("ListPRsByLabel: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 48 || prs[0].Title != "KB: HarborRegistryDown" {
		t.Fatalf("want only PR #48, got %+v", prs)
	}
	if len(prs[0].Labels) != 2 || prs[0].Labels[0] != "runlore" {
		t.Fatalf("labels not parsed: %+v", prs[0].Labels)
	}
}

func TestListPRsByLabelParsesUpdatedAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
		  {"number":48,"title":"KB: X","body":"b","labels":[{"name":"runlore"}],"pull_request":{"url":"x"},"updated_at":"2026-06-01T12:00:00Z"}
		]`))
	}))
	defer srv.Close()
	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	prs, err := c.ListPRsByLabel(context.Background(), "runlore")
	if err != nil {
		t.Fatalf("ListPRsByLabel: %v", err)
	}
	if len(prs) != 1 || prs[0].UpdatedAt.IsZero() {
		t.Fatalf("updated_at not parsed: %+v", prs)
	}
	if got := prs[0].UpdatedAt.UTC().Format(time.RFC3339); got != "2026-06-01T12:00:00Z" {
		t.Fatalf("unexpected updated_at: %s", got)
	}
}

func TestOpenPR(t *testing.T) {
	var paths []string
	mux := http.NewServeMux()
	mux.HandleFunc("GET /repos/o/r/git/ref/heads/main", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"object":{"sha":"basesha"}}`))
	})
	mux.HandleFunc("POST /repos/o/r/git/refs", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("PUT /repos/o/r/contents/", func(w http.ResponseWriter, _ *http.Request) {
		paths = append(paths, "PUT contents")
		_, _ = w.Write([]byte(`{}`))
	})
	mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		_, _ = w.Write([]byte(`{"html_url":"https://github.com/o/r/pull/9"}`))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	ref, err := c.OpenPR(context.Background(), providers.KBEntry{Type: "Incident", Title: "DB outage", Body: "## body"})
	if err != nil {
		t.Fatalf("OpenPR: %v", err)
	}
	if ref.URL != "https://github.com/o/r/pull/9" {
		t.Fatalf("ref=%s", ref.URL)
	}
	if len(paths) != 4 || !strings.Contains(strings.Join(paths, ","), "PUT contents") {
		t.Fatalf("expected the 4-call PR sequence, got %v", paths)
	}
}

func TestClose(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{}`))
	}))
	defer srv.Close()
	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	if err := c.Close(context.Background(), 42); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if gotMethod != http.MethodPatch || gotPath != "/repos/o/r/issues/42" || gotBody["state"] != "closed" {
		t.Fatalf("unexpected: %s %s body=%v", gotMethod, gotPath, gotBody)
	}
}

func TestListPRsByLabelPaginates(t *testing.T) {
	// page 1 returns a full page (100) → the client must fetch page 2 for the rest.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Query().Get("page") {
		case "1":
			items := make([]string, 100)
			for i := range items {
				items[i] = fmt.Sprintf(`{"number":%d,"title":"KB: t%d","labels":[{"name":"runlore"}],"pull_request":{}}`, i+1, i+1)
			}
			_, _ = w.Write([]byte("[" + strings.Join(items, ",") + "]"))
		case "2":
			_, _ = w.Write([]byte(`[{"number":101,"title":"KB: t101","labels":[{"name":"runlore"}],"pull_request":{}}]`))
		default:
			_, _ = w.Write([]byte(`[]`))
		}
	}))
	defer srv.Close()
	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	prs, err := c.ListPRsByLabel(context.Background(), "runlore")
	if err != nil {
		t.Fatalf("ListPRsByLabel: %v", err)
	}
	if len(prs) != 101 {
		t.Fatalf("want 101 PRs across 2 pages (no truncation at 100), got %d", len(prs))
	}
}

func TestRenderEntryIncludesFingerprintFrontmatter(t *testing.T) {
	out := renderEntry(providers.KBEntry{Type: "Incident", Title: "T", Fingerprint: "deadbeef"})
	if !strings.Contains(out, "fingerprint: deadbeef") {
		t.Fatalf("frontmatter missing fingerprint:\n%s", out)
	}
	out = renderEntry(providers.KBEntry{Type: "Incident", Title: "T"})
	if strings.Contains(out, "fingerprint:") {
		t.Fatalf("empty fingerprint must be omitted:\n%s", out)
	}
}

func TestPRBodyIncludesFingerprintMarker(t *testing.T) {
	body := prBody(providers.KBEntry{Title: "T", Description: "d", Fingerprint: "deadbeef"})
	if providers.ParseFingerprintMarker(body) != "deadbeef" {
		t.Fatalf("PR body missing recoverable fingerprint marker:\n%s", body)
	}
}
