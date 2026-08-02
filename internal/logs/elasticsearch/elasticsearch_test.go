// SPDX-License-Identifier: Apache-2.0

package elasticsearch

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// esSearchResponse and osSearchResponse are near-identical (OpenSearch forked
// from Elasticsearch 7.10 and kept the same wire envelope for _search); the two
// fixtures differ in the cosmetic metadata a real cluster of each distribution
// actually returns, so a test run against BOTH proves the client isn't
// accidentally keyed on Elasticsearch-only quirks.
const esSearchResponse = `{
  "took": 3, "timed_out": false,
  "_shards": {"total": 1, "successful": 1, "skipped": 0, "failed": 0},
  "hits": {
    "total": {"value": 2, "relation": "eq"},
    "max_score": null,
    "hits": [
      {
        "_index": "logs-2026.08.02", "_id": "a1", "_score": null,
        "_source": {
          "@timestamp": "2026-08-02T10:00:01.000Z",
          "message": "connection refused",
          "log": {"level": "error"},
          "kubernetes": {"namespace": "apps", "pod": {"name": "harbor-db-0"}, "container": {"name": "db"}}
        }
      },
      {
        "_index": "logs-2026.08.02", "_id": "a2", "_score": null,
        "_source": {
          "@timestamp": "2026-08-02T10:00:00.000Z",
          "message": "retrying",
          "log": {"level": "warn"},
          "kubernetes": {"namespace": "apps", "pod": {"name": "harbor-db-0"}, "container": {"name": "db"}}
        }
      }
    ]
  }
}`

const osSearchResponse = `{
  "took": 5, "timed_out": false,
  "_shards": {"total": 1, "successful": 1, "skipped": 0, "failed": 0},
  "hits": {
    "total": {"value": 2, "relation": "eq"},
    "max_score": null,
    "hits": [
      {
        "_index": "logs-000001", "_id": "b1", "_score": null,
        "_source": {
          "@timestamp": "2026-08-02T10:00:01.000Z",
          "message": "connection refused",
          "log": {"level": "error"},
          "kubernetes": {"namespace": "apps", "pod": {"name": "harbor-db-0"}, "container": {"name": "db"}}
        }
      },
      {
        "_index": "logs-000001", "_id": "b2", "_score": null,
        "_source": {
          "@timestamp": "2026-08-02T10:00:00.000Z",
          "message": "retrying",
          "log": {"level": "warn"},
          "kubernetes": {"namespace": "apps", "pod": {"name": "harbor-db-0"}, "container": {"name": "db"}}
        }
      }
    ]
  }
}`

func TestQuery(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{{"elasticsearch", esSearchResponse}, {"opensearch", osSearchResponse}} {
		t.Run(tc.name, func(t *testing.T) {
			var gotPath, gotMethod string
			var gotBody map[string]any
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotMethod = r.Method
				_ = json.NewDecoder(r.Body).Decode(&gotBody)
				_, _ = io.WriteString(w, tc.body)
			}))
			defer srv.Close()

			res, err := New(srv.URL, "logs-*").Query(context.Background(), `kubernetes.namespace:"apps"`, providers.TimeWindow{})
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if gotMethod != http.MethodPost {
				t.Fatalf("method=%q, want POST", gotMethod)
			}
			if gotPath != "/logs-*/_search" {
				t.Fatalf("path=%q", gotPath)
			}
			qs, _ := gotBody["query"].(map[string]any)
			boolQ, _ := qs["bool"].(map[string]any)
			must, _ := boolQ["must"].([]any)
			if len(must) != 1 {
				t.Fatalf("expected one must clause, got %+v", boolQ)
			}
			qstr, _ := must[0].(map[string]any)["query_string"].(map[string]any)
			if qstr["query"] != `kubernetes.namespace:"apps"` {
				t.Fatalf("query_string.query = %v", qstr["query"])
			}
			sort, _ := gotBody["sort"].([]any)
			if len(sort) != 1 {
				t.Fatalf("expected a sort clause, got %+v", gotBody["sort"])
			}
			sortField, _ := sort[0].(map[string]any)["@timestamp"].(map[string]any)
			if sortField["order"] != "desc" {
				t.Fatalf("must sort newest-first, got %+v", sort)
			}
			if len(res) != 2 {
				t.Fatalf("want 2 lines, got %d: %+v", len(res), res)
			}
			// Newest first, matching VictoriaLogs/Loki ordering.
			if res[0].Message != "connection refused" || res[1].Message != "retrying" {
				t.Fatalf("order/messages wrong: %+v", res)
			}
			if res[0].Time.UTC().Format(time.RFC3339) != "2026-08-02T10:00:01Z" {
				t.Fatalf("time not parsed: %v", res[0].Time)
			}
			// ECS-nested _source fields flatten to DOT-NOTATION keys matching the
			// field-convention names, so streamIdentity/query_logs field filters work.
			if res[0].Fields["kubernetes.pod.name"] != "harbor-db-0" || res[0].Fields["kubernetes.container.name"] != "db" {
				t.Fatalf("flattened fields wrong: %+v", res[0].Fields)
			}
			if res[0].Fields["log.level"] != "error" {
				t.Fatalf("level field not flattened: %+v", res[0].Fields)
			}
		})
	}
}

func TestQuerySizeCapAndSort(t *testing.T) {
	var gotSize float64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		gotSize, _ = body["size"].(float64)
		_, _ = io.WriteString(w, esSearchResponse)
	}))
	defer srv.Close()
	c := New(srv.URL, "logs-*")
	c.maxLines = 2
	res, err := c.Query(context.Background(), "*", providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotSize != 2 {
		t.Fatalf("size=%v, want 2 (the maxLines cap)", gotSize)
	}
	// Server returned exactly maxLines hits -> truncation sentinel appended.
	if len(res) != 3 || !strings.Contains(res[2].Message, "results truncated at 2") {
		t.Fatalf("want 2 lines + sentinel, got %d: %+v", len(res), res)
	}
}

func TestQueryTimeWindowRangeFilter(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"hits":{"total":{"value":0},"hits":[]}}`)
	}))
	defer srv.Close()
	start := time.Date(2026, 8, 2, 9, 0, 0, 0, time.UTC)
	end := time.Date(2026, 8, 2, 10, 0, 0, 0, time.UTC)
	if _, err := New(srv.URL, "logs-*").Query(context.Background(), "*", providers.TimeWindow{Start: start, End: end}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	qs, _ := gotBody["query"].(map[string]any)
	boolQ, _ := qs["bool"].(map[string]any)
	filter, _ := boolQ["filter"].([]any)
	if len(filter) != 1 {
		t.Fatalf("expected a range filter, got %+v", boolQ)
	}
	rangeClause, _ := filter[0].(map[string]any)["range"].(map[string]any)
	ts, _ := rangeClause["@timestamp"].(map[string]any)
	if ts["gte"] != start.Format(time.RFC3339) || ts["lte"] != end.Format(time.RFC3339) {
		t.Fatalf("range filter = %+v", ts)
	}
}

func TestQueryAuthHeaders(t *testing.T) {
	t.Setenv("RUNLORE_TEST_ES_TOKEN", "s3cr3t")
	var gotAuth, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTenant = r.Header.Get("X-Tenant")
		_, _ = io.WriteString(w, `{"hits":{"total":{"value":0},"hits":[]}}`)
	}))
	defer srv.Close()
	c := NewWithAuth(srv.URL, "logs-*", "RUNLORE_TEST_ES_TOKEN", map[string]string{"X-Tenant": "team-a"})
	if _, err := c.Query(context.Background(), "*", providers.TimeWindow{}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" || gotTenant != "team-a" {
		t.Fatalf("auth not applied: auth=%q tenant=%q", gotAuth, gotTenant)
	}
}

func TestQueryErrorPaths(t *testing.T) {
	down := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	down.Close()
	if _, err := New(down.URL, "logs-*").Query(context.Background(), "*", providers.TimeWindow{}); err == nil {
		t.Fatalf("backend down must error")
	}

	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"illegal_argument_exception"}`, http.StatusBadRequest)
	}))
	defer bad.Close()
	if _, err := New(bad.URL, "logs-*").Query(context.Background(), "*", providers.TimeWindow{}); err == nil ||
		!strings.Contains(err.Error(), "400") {
		t.Fatalf("non-200 must error with the status, got %v", err)
	}

	malformed := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"hits":{"hits":`)
	}))
	defer malformed.Close()
	if _, err := New(malformed.URL, "logs-*").Query(context.Background(), "*", providers.TimeWindow{}); err == nil {
		t.Fatalf("malformed body must error")
	}

	empty := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, `{"hits":{"total":{"value":0},"hits":[]}}`)
	}))
	defer empty.Close()
	res, err := New(empty.URL, "logs-*").Query(context.Background(), "*", providers.TimeWindow{})
	if err != nil || len(res) != 0 {
		t.Fatalf("empty result must be (nil, nil), got %v / %v", res, err)
	}
}

func TestQueryDefaultIndex(t *testing.T) {
	var gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		_, _ = io.WriteString(w, `{"hits":{"total":{"value":0},"hits":[]}}`)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "").Query(context.Background(), "*", providers.TimeWindow{}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotPath != "/logs-*/_search" {
		t.Fatalf("empty index must default to logs-*, got path %q", gotPath)
	}
}

// TestHits: date_histogram split by the level field. Each (time bucket, level
// sub-bucket) pair becomes one providers.Bucket.
func TestHits(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{
		  "aggregations": {
		    "by_time": {
		      "buckets": [
		        {"key_as_string": "2026-08-02T10:00:00.000Z", "key": 1785664800000, "doc_count": 5,
		         "by_level": {"buckets": [{"key": "error", "doc_count": 3}, {"key": "warn", "doc_count": 2}]}},
		        {"key_as_string": "2026-08-02T10:05:00.000Z", "key": 1785665100000, "doc_count": 412,
		         "by_level": {"buckets": [{"key": "error", "doc_count": 412}]}}
		      ]
		    }
		  }
		}`)
	}))
	defer srv.Close()

	buckets, err := New(srv.URL, "logs-*").Hits(context.Background(), `log.level:"error"`, providers.TimeWindow{}, 5*time.Minute)
	if err != nil {
		t.Fatalf("Hits: %v", err)
	}
	aggs, _ := gotBody["aggs"].(map[string]any)
	byTime, _ := aggs["by_time"].(map[string]any)
	dh, _ := byTime["date_histogram"].(map[string]any)
	if dh["field"] != "@timestamp" || dh["fixed_interval"] != "300s" {
		t.Fatalf("date_histogram = %+v", dh)
	}
	sub, _ := byTime["aggs"].(map[string]any)
	byLevel, _ := sub["by_level"].(map[string]any)
	terms, _ := byLevel["terms"].(map[string]any)
	if terms["field"] != "log.level" {
		t.Fatalf("by_level terms field = %v", terms["field"])
	}
	if len(buckets) != 3 {
		t.Fatalf("want 3 (time,level) buckets, got %d: %+v", len(buckets), buckets)
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

func TestHitsErrorPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "parse_exception", http.StatusBadRequest)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "logs-*").Hits(context.Background(), "*", providers.TimeWindow{}, time.Minute); err == nil ||
		!strings.Contains(err.Error(), "400") {
		t.Fatalf("bad request must error with status, got %v", err)
	}
}

// TestTopMessages: the happy path where the message field IS aggregatable
// (e.g. a keyword sub-field configured) — server-side terms aggregation wins,
// no client-side fallback triggered.
func TestTopMessages(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{
		  "aggregations": {
		    "top_messages": {
		      "buckets": [
		        {"key": "connection refused", "doc_count": 388,
		         "first_seen": {"value": 1785664800000, "value_as_string": "2026-08-02T10:00:00.000Z"},
		         "last_seen":  {"value": 1785665100000, "value_as_string": "2026-08-02T10:05:00.000Z"}},
		        {"key": "timeout waiting for db", "doc_count": 12,
		         "first_seen": {"value": 1785664800000, "value_as_string": "2026-08-02T10:00:00.000Z"},
		         "last_seen":  {"value": 1785664800000, "value_as_string": "2026-08-02T10:00:00.000Z"}}
		      ]
		    }
		  }
		}`)
	}))
	defer srv.Close()

	msgs, err := New(srv.URL, "logs-*").TopMessages(context.Background(), `log.level:"error"`, providers.TimeWindow{}, 10)
	if err != nil {
		t.Fatalf("TopMessages: %v", err)
	}
	aggs, _ := gotBody["aggs"].(map[string]any)
	tm, _ := aggs["top_messages"].(map[string]any)
	terms, _ := tm["terms"].(map[string]any)
	if terms["field"] != "message" {
		t.Fatalf("terms field = %v", terms["field"])
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 messages, got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Message != "connection refused" || msgs[0].Count != 388 {
		t.Fatalf("dominant message wrong: %+v", msgs[0])
	}
	if !msgs[0].First.Before(msgs[0].Last) {
		t.Fatalf("first->last span not tracked: %+v", msgs[0])
	}
}

// TestTopMessagesFallsBackWhenMessageFieldIsTextOnly: the parity caveat this
// task exists to prove. ECS `message` ships as a plain `text` field with no
// keyword sub-field by default, so Elasticsearch/OpenSearch reject a terms
// aggregation on it with a 400 "Fielddata is disabled on text fields"
// illegal_argument_exception. On exactly that signature, TopMessages falls
// back to client-side aggregation over the capped, newest-first Query sample
// (mirroring loki.Client.TopMessages) instead of surfacing the error.
func TestTopMessagesFallsBackWhenMessageFieldIsTextOnly(t *testing.T) {
	var aggAttempts, plainSearches int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		if aggs, ok := body["aggs"].(map[string]any); ok {
			if _, ok := aggs["top_messages"]; ok {
				aggAttempts++
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{
				  "error": {
				    "type": "search_phase_execution_exception",
				    "reason": "all shards failed",
				    "caused_by": {
				      "type": "illegal_argument_exception",
				      "reason": "Fielddata is disabled on text fields by default. Set fielddata=true on [message] in order to load fielddata in memory by uninverting the inverted index. Note that this can however use significant memory."
				    }
				  },
				  "status": 400
				}`)
				return
			}
		}
		plainSearches++
		_, _ = io.WriteString(w, `{"hits":{"total":{"value":4},"hits":[
		  {"_source":{"@timestamp":"2026-08-02T10:00:03.000Z","message":"connection refused to 10.0.0.7"}},
		  {"_source":{"@timestamp":"2026-08-02T10:00:02.000Z","message":"connection refused to 10.0.0.9"}},
		  {"_source":{"@timestamp":"2026-08-02T10:00:01.000Z","message":"connection refused to 10.0.0.9"}},
		  {"_source":{"@timestamp":"2026-08-02T10:00:00.000Z","message":"timeout waiting for db"}}
		]}}`)
	}))
	defer srv.Close()

	msgs, err := New(srv.URL, "logs-*").TopMessages(context.Background(), "*", providers.TimeWindow{}, 10)
	if err != nil {
		t.Fatalf("TopMessages must degrade, not error: %v", err)
	}
	if aggAttempts != 1 || plainSearches != 1 {
		t.Fatalf("want exactly one aggregation attempt then one fallback search, got agg=%d plain=%d", aggAttempts, plainSearches)
	}
	if len(msgs) != 2 {
		t.Fatalf("want 2 grouped messages (numeric tokens collapsed), got %d: %+v", len(msgs), msgs)
	}
	if msgs[0].Count != 3 || !strings.Contains(msgs[0].Message, "connection refused") {
		t.Fatalf("dominant message wrong: %+v", msgs[0])
	}
	if msgs[1].Message != "timeout waiting for db" || msgs[1].Count != 1 {
		t.Fatalf("second message wrong: %+v", msgs[1])
	}
}

// TestTopMessagesRealErrorPropagates: a genuine failure (not the text-field
// aggregation-rejection signature) must surface as an error, never be silently
// swallowed as if it were the text-only fallback case.
func TestTopMessagesRealErrorPropagates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":{"type":"parse_exception","reason":"bad query"}}`, http.StatusBadRequest)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "logs-*").TopMessages(context.Background(), "*", providers.TimeWindow{}, 10); err == nil {
		t.Fatalf("a non-text-field 400 must error, not silently fall back")
	}
}

// TestFieldNames: discover_log_fields via _field_caps, present on both ES 8.x
// and OpenSearch 2.x. field_caps carries no per-field hit count (unlike
// VictoriaLogs' field_names), so every FieldCount.Hits is 0 — documented as a
// parity caveat.
func TestFieldNames(t *testing.T) {
	var gotPath, gotFields string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotFields = r.URL.Query().Get("fields")
		_, _ = io.WriteString(w, `{
		  "indices": ["logs-2026.08.02"],
		  "fields": {
		    "kubernetes.namespace": {"keyword": {"type": "keyword", "searchable": true, "aggregatable": true}},
		    "kubernetes.pod.name":  {"keyword": {"type": "keyword", "searchable": true, "aggregatable": true}},
		    "message": {"text": {"type": "text", "searchable": true, "aggregatable": false}}
		  }
		}`)
	}))
	defer srv.Close()

	fields, err := New(srv.URL, "logs-*").FieldNames(context.Background(), `kubernetes.namespace:"apps"`, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("FieldNames: %v", err)
	}
	if gotPath != "/logs-*/_field_caps" {
		t.Fatalf("path=%q", gotPath)
	}
	if gotFields != "*" {
		t.Fatalf("fields param=%q, want *", gotFields)
	}
	if len(fields) != 3 {
		t.Fatalf("want 3 fields, got %d: %+v", len(fields), fields)
	}
	// Deterministic (alphabetical) order, since field_caps carries no frequency
	// signal to sort by.
	if fields[0].Name != "kubernetes.namespace" || fields[0].Hits != 0 {
		t.Fatalf("fields[0] = %+v", fields[0])
	}
}

func TestFieldNamesErrorPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "index_not_found_exception", http.StatusNotFound)
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "logs-*").FieldNames(context.Background(), "*", providers.TimeWindow{}); err == nil {
		t.Fatalf("404 must error")
	}
}

// TestWithLevelFieldAndTimestampField: overrides reach both the query builder
// (via the caller, not tested here) and the client's own request bodies.
func TestWithLevelFieldAndTimestampField(t *testing.T) {
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = io.WriteString(w, `{"hits":{"total":{"value":0},"hits":[]}}`)
	}))
	defer srv.Close()
	c := New(srv.URL, "logs-*").WithLevelField("severity").WithTimestampField("event.created")
	if _, err := c.Query(context.Background(), "*", providers.TimeWindow{}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	sort, _ := gotBody["sort"].([]any)
	if _, ok := sort[0].(map[string]any)["event.created"]; !ok {
		t.Fatalf("custom timestamp field not used for sort: %+v", sort)
	}
}
