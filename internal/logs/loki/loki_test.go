// SPDX-License-Identifier: Apache-2.0

package loki

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
	var gotPath, gotQuery, gotLimit, gotDirection string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQuery = r.URL.Query().Get("query")
		gotLimit = r.URL.Query().Get("limit")
		gotDirection = r.URL.Query().Get("direction")
		_, _ = io.WriteString(w, `{
		  "status": "success",
		  "data": {
		    "resultType": "streams",
		    "result": [
		      {
		        "stream": {"namespace": "apps", "pod": "harbor-db-0", "container": "db", "detected_level": "error"},
		        "values": [
		          ["1750413601000000000", "retrying"],
		          ["1750413600000000000", "db connection refused"]
		        ]
		      }
		    ]
		  }
		}`)
	}))
	defer srv.Close()

	res, err := New(srv.URL).Query(context.Background(), `{namespace="apps"}`, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotPath != "/loki/api/v1/query_range" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotQuery != `{namespace="apps"}` {
		t.Fatalf("query=%q", gotQuery)
	}
	if gotLimit != "1000" || gotDirection != "backward" {
		t.Fatalf("limit=%q direction=%q, want 1000/backward", gotLimit, gotDirection)
	}
	if len(res) != 2 {
		t.Fatalf("want 2 lines, got %d", len(res))
	}
	// Newest first, matching the VictoriaLogs provider's ordering contract.
	if res[0].Message != "retrying" || res[1].Message != "db connection refused" {
		t.Fatalf("order/messages wrong: %+v", res)
	}
	// Nanosecond epoch parsed; stream labels become LogLine.Fields (so the
	// renderer's streamIdentity finds pod/container under the Loki names).
	if res[1].Time.UTC().Format("2006-01-02T15:04:05Z") != "2025-06-20T10:00:00Z" {
		t.Fatalf("time not parsed from ns epoch: %v", res[1].Time)
	}
	if res[0].Fields["pod"] != "harbor-db-0" || res[0].Fields["container"] != "db" {
		t.Fatalf("stream labels not mapped to fields: %+v", res[0].Fields)
	}
}

func TestQueryAuthHeaders(t *testing.T) {
	t.Setenv("RUNLORE_TEST_LOKI_TOKEN", "s3cr3t")
	var gotAuth, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTenant = r.Header.Get("X-Scope-OrgID")
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer srv.Close()

	c := NewWithAuth(srv.URL, "RUNLORE_TEST_LOKI_TOKEN", map[string]string{"X-Scope-OrgID": "tenant-b"})
	if _, err := c.Query(context.Background(), `{namespace="apps"}`, providers.TimeWindow{}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" || gotTenant != "tenant-b" {
		t.Fatalf("auth not applied: auth=%q tenant=%q", gotAuth, gotTenant)
	}
}

// TestQueryTruncation: Loki has no offset pagination; the client sends
// limit=maxLines once and appends the shared TruncationLine sentinel when the
// server returned exactly the limit (more likely matched upstream).
func TestQueryTruncation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[
		  {"stream":{"namespace":"apps"},"values":[
		    ["1750413602000000000","a"],["1750413601000000000","b"],["1750413600000000000","c"]]}]}}`)
	}))
	defer srv.Close()

	c := New(srv.URL)
	c.maxLines = 3
	res, err := c.Query(context.Background(), `{namespace="apps"}`, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(res) != 4 || !strings.Contains(res[3].Message, "results truncated at 3") {
		t.Fatalf("want 3 lines + sentinel, got %d: %+v", len(res), res)
	}
}

// TestQueryErrorPaths: backend down, non-200, JSON error status, and malformed
// body must each surface as an error (never a silent empty result), so the tool
// call fails visibly and the loop records a data gap instead of "no logs".
func TestQueryErrorPaths(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	down.Close() // connection refused
	if _, err := New(down.URL).Query(context.Background(), `{a="b"}`, providers.TimeWindow{}); err == nil {
		t.Fatalf("backend down must error")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "too many outstanding requests", http.StatusTooManyRequests)
	}))
	defer bad.Close()
	if _, err := New(bad.URL).Query(context.Background(), `{a="b"}`, providers.TimeWindow{}); err == nil ||
		!strings.Contains(err.Error(), "429") {
		t.Fatalf("non-200 must error with the status, got %v", err)
	}

	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":`)
	}))
	defer malformed.Close()
	if _, err := New(malformed.URL).Query(context.Background(), `{a="b"}`, providers.TimeWindow{}); err == nil {
		t.Fatalf("malformed body must error")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer empty.Close()
	res, err := New(empty.URL).Query(context.Background(), `{a="b"}`, providers.TimeWindow{})
	if err != nil || len(res) != 0 {
		t.Fatalf("empty result must be (nil, nil), got %v / %v", res, err)
	}
}
