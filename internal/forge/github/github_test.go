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
