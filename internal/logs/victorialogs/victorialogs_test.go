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
