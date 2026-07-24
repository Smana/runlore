// SPDX-License-Identifier: Apache-2.0

package loki

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

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
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "too many outstanding requests", http.StatusTooManyRequests)
	}))
	defer bad.Close()
	if _, err := New(bad.URL).Query(context.Background(), `{a="b"}`, providers.TimeWindow{}); err == nil ||
		!strings.Contains(err.Error(), "429") {
		t.Fatalf("non-200 must error with the status, got %v", err)
	}

	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":`)
	}))
	defer malformed.Close()
	if _, err := New(malformed.URL).Query(context.Background(), `{a="b"}`, providers.TimeWindow{}); err == nil {
		t.Fatalf("malformed body must error")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[]}}`)
	}))
	defer empty.Close()
	res, err := New(empty.URL).Query(context.Background(), `{a="b"}`, providers.TimeWindow{})
	if err != nil || len(res) != 0 {
		t.Fatalf("empty result must be (nil, nil), got %v / %v", res, err)
	}
}

func TestHits(t *testing.T) {
	var gotQuery, gotStep string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotQuery = r.URL.Query().Get("query")
		gotStep = r.URL.Query().Get("step")
		_, _ = io.WriteString(w, `{
		  "status": "success",
		  "data": {
		    "resultType": "matrix",
		    "result": [
		      {"metric": {"detected_level": "error"}, "values": [[1704103200, "3"], [1704103500, "412"]]},
		      {"metric": {"detected_level": "warn"},  "values": [[1704103200, "1"]]}
		    ]
		  }
		}`)
	}))
	defer srv.Close()

	buckets, err := New(srv.URL).Hits(context.Background(), `{namespace="apps"} | detected_level="error"`,
		providers.TimeWindow{}, 5*time.Minute)
	if err != nil {
		t.Fatalf("Hits: %v", err)
	}
	// The log query must be wrapped in the LogQL metric form, split by the level label.
	want := `sum by (detected_level) (count_over_time({namespace="apps"} | detected_level="error" [300s]))`
	if gotQuery != want {
		t.Fatalf("metric query = %q, want %q", gotQuery, want)
	}
	if gotStep != "300s" {
		t.Fatalf("step=%q, want 300s", gotStep)
	}
	if len(buckets) != 3 {
		t.Fatalf("want 3 buckets, got %d: %+v", len(buckets), buckets)
	}
	var sawSpike bool
	for _, b := range buckets {
		if b.Level == "error" && b.Count == 412 && !b.Time.IsZero() {
			sawSpike = true
		}
	}
	if !sawSpike {
		t.Fatalf("missing error=412 bucket: %+v", buckets)
	}
}

func TestTopMessages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"status":"success","data":{"resultType":"streams","result":[
		  {"stream":{"namespace":"apps"},"values":[
		    ["1750413603000000000","connection refused to 10.0.0.7"],
		    ["1750413602000000000","connection refused to 10.0.0.9"],
		    ["1750413601000000000","connection refused to 10.0.0.9"],
		    ["1750413600000000000","timeout waiting for db"]]}]}}`)
	}))
	defer srv.Close()

	msgs, err := New(srv.URL).TopMessages(context.Background(), `{namespace="apps"}`, providers.TimeWindow{}, 10)
	if err != nil {
		t.Fatalf("TopMessages: %v", err)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 grouped messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Count != 3 || !strings.Contains(msgs[0].Message, "connection refused") {
		t.Fatalf("dominant message wrong: %+v", msgs[0])
	}
	if !msgs[0].First.Before(msgs[0].Last) {
		t.Fatalf("first→last span not tracked: %+v", msgs[0])
	}
	if msgs[1].Message != "timeout waiting for db" || msgs[1].Count != 1 {
		t.Fatalf("second message wrong: %+v", msgs[1])
	}
}

func TestHitsErrorPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "parse error: unexpected token", http.StatusBadRequest)
	}))
	defer srv.Close()
	if _, err := New(srv.URL).Hits(context.Background(), `{a="b"}`, providers.TimeWindow{}, time.Minute); err == nil ||
		!strings.Contains(err.Error(), "400") {
		t.Fatalf("bad request must error with status, got %v", err)
	}
}

// TestFieldNames: discovery merges STREAM labels (/loki/api/v1/labels — the
// selector building blocks the model needs first) with detected body fields
// (/loki/api/v1/detected_fields, Loki 3.x). Hits carries the detected field's
// value CARDINALITY (Loki reports no per-field hit count); stream labels carry 0
// and the discover tool renders the number only when > 0.
func TestFieldNames(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/loki/api/v1/labels":
			if q := r.URL.Query().Get("query"); q != `{namespace="apps"}` {
				t.Errorf("labels query=%q", q)
			}
			_, _ = io.WriteString(w, `{"status":"success","data":["namespace","pod","container"]}`)
		case "/loki/api/v1/detected_fields":
			_, _ = io.WriteString(w, `{"fields":[
			  {"label":"level","type":"string","cardinality":4,"parsers":["logfmt"]},
			  {"label":"duration","type":"duration","cardinality":99,"parsers":["logfmt"]}]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	fields, err := New(srv.URL).FieldNames(context.Background(), `{namespace="apps"}`, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("FieldNames: %v", err)
	}
	if len(fields) != 5 {
		t.Fatalf("want 3 labels + 2 detected fields, got %d: %+v", len(fields), fields)
	}
	if fields[0].Name != "namespace" || fields[0].Hits != 0 {
		t.Fatalf("stream labels must come first with Hits=0: %+v", fields[0])
	}
	if fields[3].Name != "level" || fields[3].Hits != 4 {
		t.Fatalf("detected field must carry cardinality as Hits: %+v", fields[3])
	}
}

// TestFieldNamesOldLoki: a Loki without detected_fields (pre-3.0 returns 404)
// still answers discovery with the stream labels alone — degrade, don't fail.
func TestFieldNamesOldLoki(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/loki/api/v1/labels" {
			_, _ = io.WriteString(w, `{"status":"success","data":["namespace","pod"]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	fields, err := New(srv.URL).FieldNames(context.Background(), `{namespace="apps"}`, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("FieldNames must degrade to labels-only, got %v", err)
	}
	if len(fields) != 2 || fields[0].Name != "namespace" {
		t.Fatalf("labels-only result wrong: %+v", fields)
	}
}

// TestFieldNamesBothDown: when neither endpoint answers, the error must surface.
func TestFieldNamesBothDown(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	if _, err := New(srv.URL).FieldNames(context.Background(), `{a="b"}`, providers.TimeWindow{}); err == nil {
		t.Fatalf("both endpoints failing must error")
	}
}
