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

func TestListClosedUnmergedPRsByLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/issues" || r.URL.Query().Get("state") != "closed" {
			t.Fatalf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		// a closed-unmerged PR (merged_at null), a merged PR (merged_at set → excluded),
		// and a plain closed issue (no pull_request → excluded)
		_, _ = w.Write([]byte(`[
		  {"number":48,"title":"KB: A","body":"b","labels":[{"name":"runlore"},{"name":"not-kb-worthy"}],"pull_request":{"url":"x","merged_at":null}},
		  {"number":50,"title":"KB: B","body":"b","labels":[{"name":"runlore"}],"pull_request":{"url":"y","merged_at":"2026-06-01T12:00:00Z"}},
		  {"number":39,"title":"plain issue","body":"b","labels":[{"name":"runlore"}]}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	prs, err := c.ListClosedUnmergedPRsByLabel(context.Background(), "runlore")
	if err != nil {
		t.Fatalf("ListClosedUnmergedPRsByLabel: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 48 {
		t.Fatalf("want only the closed-unmerged PR #48, got %+v", prs)
	}
	if len(prs[0].Labels) != 2 || prs[0].Labels[1] != "not-kb-worthy" {
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
	mux.HandleFunc("GET /repos/o/r/contents/{path...}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound) // bare bundle: no index.md / log.md yet
	})
	mux.HandleFunc("PUT /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, "PUT "+r.PathValue("path"))
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
	// base ref → branch → entry file → log.md (bundle maintenance) → PR
	got := strings.Join(paths, ",")
	if len(paths) != 5 || !strings.Contains(got, "PUT incidents/db-outage-") || !strings.Contains(got, "PUT log.md") {
		t.Fatalf("expected the 5-call PR sequence ending in pulls, got %v", paths)
	}
	if paths[len(paths)-1] != "/repos/o/r/pulls" {
		t.Fatalf("the PR must be opened last, got %v", paths)
	}
}

// TestOpenPREntryPath pins the entry file path: a type directory plus a
// fingerprint-suffixed slug, so two different incidents that share a title can't
// collide on the same path (the contents PUT would 422 on the second PR after the
// first merges). With no fingerprint the branch timestamp disambiguates instead.
func TestOpenPREntryPath(t *testing.T) {
	openPR := func(e providers.KBEntry) string {
		t.Helper()
		var putPath string
		mux := http.NewServeMux()
		mux.HandleFunc("GET /repos/o/r/git/ref/heads/main", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"object":{"sha":"basesha"}}`))
		})
		mux.HandleFunc("POST /repos/o/r/git/refs", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{}`))
		})
		mux.HandleFunc("PUT /repos/o/r/contents/{path...}", func(w http.ResponseWriter, r *http.Request) {
			putPath = r.PathValue("path")
			_, _ = w.Write([]byte(`{}`))
		})
		mux.HandleFunc("POST /repos/o/r/pulls", func(w http.ResponseWriter, _ *http.Request) {
			_, _ = w.Write([]byte(`{"html_url":"u"}`))
		})
		srv := httptest.NewServer(mux)
		defer srv.Close()
		c := New(srv.URL, "o", "r", "main", staticToken("tok"))
		if _, err := c.OpenPR(context.Background(), e); err != nil {
			t.Fatalf("OpenPR: %v", err)
		}
		return putPath
	}

	got := openPR(providers.KBEntry{Type: "Incident", Title: "DB outage", Fingerprint: "deadbeefcafebabe", Body: "## body"})
	if got != "incidents/db-outage-deadbeef.md" {
		t.Fatalf("fingerprinted incident path = %q, want incidents/db-outage-deadbeef.md", got)
	}

	got = openPR(providers.KBEntry{Type: "Playbook", Title: "DB outage", Body: "## body"})
	if !strings.HasPrefix(got, "playbooks/db-outage-") || !strings.HasSuffix(got, ".md") || got == "playbooks/db-outage-.md" {
		t.Fatalf("unfingerprinted playbook path = %q, want playbooks/db-outage-<ts>.md", got)
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

func TestRenderEntryIncludesTimestamp(t *testing.T) {
	// OKF recommends a timestamp; seed entries carry one. Curated entries must too,
	// for provenance parity in the PR diff. It must be a parseable RFC3339 value.
	out := renderEntry(providers.KBEntry{Type: "Incident", Title: "T", Body: "## body"})
	const key = "timestamp: "
	i := strings.Index(out, key)
	if i < 0 {
		t.Fatalf("frontmatter missing timestamp:\n%s", out)
	}
	line := out[i+len(key):]
	if j := strings.IndexByte(line, '\n'); j >= 0 {
		line = line[:j]
	}
	// yaml.v3 quotes a timestamp-shaped string to keep it a string (not a YAML
	// timestamp); strip the quotes before parsing the value.
	val := strings.Trim(strings.TrimSpace(line), `"`)
	if _, err := time.Parse(time.RFC3339, val); err != nil {
		t.Fatalf("timestamp %q is not RFC3339: %v", val, err)
	}
}

// TestRenderEntryIncludesConfidenceAndProvenance: confidence and change
// provenance are queryable OKF extension frontmatter keys (frontmatter is for
// the fields you filter/index on), omitted when unset.
func TestRenderEntryIncludesConfidenceAndProvenance(t *testing.T) {
	out := renderEntry(providers.KBEntry{
		Type: "Incident", Title: "T", Confidence: 0.9,
		Provenance: []string{"crossplane/xplane-harbor"},
	})
	if !strings.Contains(out, "confidence: 0.9") {
		t.Fatalf("frontmatter missing confidence:\n%s", out)
	}
	if !strings.Contains(out, "provenance:") || !strings.Contains(out, "crossplane/xplane-harbor") {
		t.Fatalf("frontmatter missing provenance:\n%s", out)
	}

	out = renderEntry(providers.KBEntry{Type: "Incident", Title: "T"})
	if strings.Contains(out, "confidence:") || strings.Contains(out, "provenance:") {
		t.Fatalf("unset confidence/provenance must be omitted:\n%s", out)
	}
}

func TestPRBodyIncludesFingerprintMarker(t *testing.T) {
	body := prBody(providers.KBEntry{Title: "T", Description: "d", Fingerprint: "deadbeef"})
	if providers.ParseFingerprintMarker(body) != "deadbeef" {
		t.Fatalf("PR body missing recoverable fingerprint marker:\n%s", body)
	}
}
