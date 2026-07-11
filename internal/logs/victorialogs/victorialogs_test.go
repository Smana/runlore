// SPDX-License-Identifier: Apache-2.0

package victorialogs

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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

func TestQueryAuthHeaders(t *testing.T) {
	t.Setenv("RUNLORE_TEST_VL_TOKEN", "s3cr3t")
	var gotAuth, gotTenant, gotCT string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTenant = r.Header.Get("X-Scope-OrgID")
		gotCT = r.Header.Get("Content-Type")
		_, _ = io.WriteString(w, ``)
	}))
	defer srv.Close()

	c := NewWithAuth(srv.URL, "RUNLORE_TEST_VL_TOKEN", map[string]string{"X-Scope-OrgID": "tenant-b"})
	if _, err := c.Query(context.Background(), `{namespace="apps"}`, providers.TimeWindow{}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("Authorization: got %q, want %q", gotAuth, "Bearer s3cr3t")
	}
	if gotTenant != "tenant-b" {
		t.Fatalf("X-Scope-OrgID: got %q, want tenant-b", gotTenant)
	}
	// Auth headers must not displace the form Content-Type the query relies on.
	if !strings.Contains(gotCT, "x-www-form-urlencoded") {
		t.Fatalf("content-type=%q", gotCT)
	}
}

func TestQueryNoAuthByDefault(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = io.WriteString(w, ``)
	}))
	defer srv.Close()

	if _, err := New(srv.URL).Query(context.Background(), `{namespace="apps"}`, providers.TimeWindow{}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("unconfigured client must send no Authorization header, got %q", gotAuth)
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

// formValue returns the value of key in an x-www-form-urlencoded body.
func formValue(body, key string) string {
	vals, err := url.ParseQuery(body)
	if err != nil {
		return ""
	}
	return vals.Get(key)
}

// ndjson renders n synthetic log lines as VictoriaLogs NDJSON.
func ndjson(n int) string {
	var b strings.Builder
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, `{"_time":"2026-06-20T10:00:0%dZ","_msg":"line %d"}`+"\n", i%10, i)
	}
	return b.String()
}

func TestHits(t *testing.T) {
	var gotPath, gotStep string
	var gotField []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		gotStep = formValue(string(body), "step")
		if vals, err := url.ParseQuery(string(body)); err == nil {
			gotField = vals["field"]
		}
		_, _ = io.WriteString(w, `{"hits":[
		  {"fields":{"level":"error"},"timestamps":["2024-01-01T10:00:00Z","2024-01-01T10:05:00Z"],"values":[3,412],"total":415},
		  {"fields":{"level":"warn"},"timestamps":["2024-01-01T10:00:00Z","2024-01-01T10:05:00Z"],"values":[1,2],"total":3}]}`)
	}))
	defer srv.Close()

	buckets, err := New(srv.URL).Hits(context.Background(), `{namespace="apps"} | error`, providers.TimeWindow{}, 5*60*1e9)
	if err != nil {
		t.Fatalf("Hits: %v", err)
	}
	if gotPath != "/select/logsql/hits" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotStep != "300s" {
		t.Fatalf("step=%q, want 300s", gotStep)
	}
	if len(gotField) != 1 || gotField[0] != "level" {
		t.Fatalf("field=%v, want [level]", gotField)
	}
	if len(buckets) != 4 {
		t.Fatalf("want 4 buckets, got %d: %+v", len(buckets), buckets)
	}
	// Error series must be preserved with its level + count.
	var sawSpike bool
	for _, b := range buckets {
		if b.Level == "error" && b.Count == 412 {
			sawSpike = true
		}
	}
	if !sawSpike {
		t.Fatalf("missing error=412 bucket: %+v", buckets)
	}
}

func TestTopMessages(t *testing.T) {
	var gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/select/logsql/query" {
			t.Errorf("path=%q", r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		gotQuery = formValue(string(body), "query")
		_, _ = io.WriteString(w, `{"_msg":"connection refused","rows":"388","first":"2024-01-01T10:00:00Z","last":"2024-01-01T10:05:00Z"}
{"_msg":"timeout waiting for db","rows":"24","first":"2024-01-01T10:01:00Z","last":"2024-01-01T10:04:00Z"}
`)
	}))
	defer srv.Close()

	msgs, err := New(srv.URL).TopMessages(context.Background(), `{namespace="apps"} | error`, providers.TimeWindow{}, 10)
	if err != nil {
		t.Fatalf("TopMessages: %v", err)
	}
	// The stats pipe must be composed with collapse_nums + stats by (_msg) + limit.
	for _, want := range []string{"collapse_nums", "stats by (_msg)", "count() rows", "limit 10"} {
		if !strings.Contains(gotQuery, want) {
			t.Fatalf("query %q missing %q", gotQuery, want)
		}
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d", len(msgs))
	}
	if msgs[0].Message != "connection refused" || msgs[0].Count != 388 {
		t.Fatalf("unexpected first message: %+v", msgs[0])
	}
	if msgs[0].First.IsZero() || msgs[0].Last.IsZero() {
		t.Fatalf("first/last not parsed: %+v", msgs[0])
	}
}

func TestFieldNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/select/logsql/field_names" {
			t.Errorf("path=%q", r.URL.Path)
		}
		_, _ = io.WriteString(w, `{"values":[
		  {"value":"_msg","hits":1033},
		  {"value":"kubernetes.container_name","hits":900}]}`)
	}))
	defer srv.Close()

	fields, err := New(srv.URL).FieldNames(context.Background(), `{namespace="apps"}`, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("FieldNames: %v", err)
	}
	if len(fields) != 2 || fields[0].Name != "_msg" || fields[0].Hits != 1033 {
		t.Fatalf("unexpected fields: %+v", fields)
	}
	if fields[1].Name != "kubernetes.container_name" {
		t.Fatalf("unexpected second field: %+v", fields[1])
	}
}

func TestQueryPagination(t *testing.T) {
	const pageSize = 100

	tests := []struct {
		name          string
		maxLines      int
		page0         int // lines returned for offset=0
		page1         int // lines returned for offset=100 (0 => not requested / empty)
		wantLines     int // real (non-sentinel) lines
		wantTruncated bool
		wantOffsets   []string // offsets the server must have seen, in order
	}{
		{
			name:        "single short page — no pagination, no sentinel",
			maxLines:    1000,
			page0:       40,
			wantLines:   40,
			wantOffsets: []string{"0"},
		},
		{
			name:        "two pages under cap — all lines, no sentinel",
			maxLines:    1000,
			page0:       pageSize,
			page1:       30,
			wantLines:   pageSize + 30,
			wantOffsets: []string{"0", "100"},
		},
		{
			name:          "cap binds on a full page — capped + sentinel",
			maxLines:      150,
			page0:         pageSize,
			page1:         pageSize,
			wantLines:     150,
			wantTruncated: true,
			wantOffsets:   []string{"0", "100"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var offsets []string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				off := formValue(string(body), "offset")
				offsets = append(offsets, off)
				switch off {
				case "", "0":
					_, _ = io.WriteString(w, ndjson(tc.page0))
				case "100":
					_, _ = io.WriteString(w, ndjson(tc.page1))
				default:
					_, _ = io.WriteString(w, "")
				}
			}))
			defer srv.Close()

			c := New(srv.URL)
			c.maxLines = tc.maxLines
			res, err := c.Query(context.Background(), `{namespace="apps"}`, providers.TimeWindow{})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}

			lines, sentinel := 0, 0
			for _, l := range res {
				if strings.Contains(l.Message, "results truncated at") {
					sentinel++
				} else {
					lines++
				}
			}
			if lines != tc.wantLines {
				t.Fatalf("lines = %d, want %d (total %d)", lines, tc.wantLines, len(res))
			}
			if (sentinel == 1) != tc.wantTruncated {
				t.Fatalf("sentinel present = %v, want %v", sentinel == 1, tc.wantTruncated)
			}
			if tc.wantTruncated && !strings.Contains(res[len(res)-1].Message, "truncated at 150") {
				t.Fatalf("last line should be the cap sentinel, got %q", res[len(res)-1].Message)
			}
			if len(offsets) != len(tc.wantOffsets) {
				t.Fatalf("offsets seen = %v, want %v", offsets, tc.wantOffsets)
			}
			for i, want := range tc.wantOffsets {
				got := offsets[i]
				if got == "" {
					got = "0" // VictoriaLogs treats an absent offset as 0
				}
				if got != want {
					t.Fatalf("offset[%d] = %q, want %q", i, offsets[i], want)
				}
			}
		})
	}
}
