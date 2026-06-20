package victorialogs

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestQuery(t *testing.T) {
	var gotQuery, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/select/logsql/query" {
			t.Errorf("path=%q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		gotCT = r.Header.Get("Content-Type")
		if vals, err := parseForm(string(body)); err == nil {
			gotQuery = vals
		}
		_, _ = io.WriteString(w, `{"_time":"2026-06-20T10:00:00Z","_msg":"db connection refused","kubernetes.pod_name":"harbor-db-0"}
{"_time":"2026-06-20T10:00:01Z","_msg":"retrying","kubernetes.pod_name":"harbor-core-1"}
`)
	}))
	defer srv.Close()

	res, err := New(srv.URL).Query(context.Background(), `{namespace="apps"} | error`, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if !strings.Contains(gotCT, "x-www-form-urlencoded") {
		t.Fatalf("content-type=%q", gotCT)
	}
	if gotQuery == "" {
		t.Fatalf("query not form-encoded")
	}
	if len(res) != 2 {
		t.Fatalf("want 2 lines, got %d", len(res))
	}
	if res[0].Message != "db connection refused" || res[0].Fields["kubernetes.pod_name"] != "harbor-db-0" {
		t.Fatalf("unexpected first line: %+v", res[0])
	}
	if res[0].Time.IsZero() {
		t.Fatalf("_time not parsed")
	}
}

// parseForm returns the decoded "query" value if present.
func parseForm(body string) (string, error) {
	for _, kv := range strings.Split(body, "&") {
		if strings.HasPrefix(kv, "query=") {
			return kv, nil
		}
	}
	return "", io.EOF
}
