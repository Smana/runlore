package github

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
