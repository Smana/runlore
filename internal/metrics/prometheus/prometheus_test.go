// SPDX-License-Identifier: Apache-2.0

package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func TestQuery(t *testing.T) {
	var gotPath, gotQuery string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotQuery = r.URL.Path, r.URL.Query().Get("query")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[
		  {"metric":{"__name__":"up","job":"api"},"value":[1700000000,"0"]},
		  {"metric":{"__name__":"up","job":"db"},"value":[1700000000,"1"]}]}}`))
	}))
	defer srv.Close()

	s, err := New(srv.URL).Query(context.Background(), "up", time.Unix(1700000000, 0))
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotPath != "/api/v1/query" || gotQuery != "up" {
		t.Fatalf("path=%q query=%q", gotPath, gotQuery)
	}
	if len(s) != 2 || s[0].Metric["job"] != "api" || s[0].Value != 0 || s[1].Value != 1 {
		t.Fatalf("unexpected samples: %+v", s)
	}
}

func TestQueryRange(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/query_range" {
			t.Errorf("path=%q", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[
		  {"metric":{"pod":"x"},"values":[[1700000000,"1"],[1700000060,"2"]]}]}}`))
	}))
	defer srv.Close()

	m, err := New(srv.URL).QueryRange(context.Background(), "rate(x[5m])",
		providers.TimeWindow{Start: time.Unix(1700000000, 0), End: time.Unix(1700000300, 0)}, time.Minute)
	if err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if len(m) != 1 || len(m[0].Points) != 2 || m[0].Points[1].Value != 2 {
		t.Fatalf("unexpected matrix: %+v", m)
	}
}

func TestQueryAuthHeaders(t *testing.T) {
	t.Setenv("RUNLORE_TEST_VM_TOKEN", "s3cr3t")
	var gotAuth, gotTenant string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotTenant = r.Header.Get("X-Scope-OrgID")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()

	c := NewWithAuth(srv.URL, "RUNLORE_TEST_VM_TOKEN", map[string]string{"X-Scope-OrgID": "tenant-a"})
	if _, err := c.Query(context.Background(), "up", time.Time{}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("Authorization: got %q, want %q", gotAuth, "Bearer s3cr3t")
	}
	if gotTenant != "tenant-a" {
		t.Fatalf("X-Scope-OrgID: got %q, want tenant-a", gotTenant)
	}

	// The custom header must also ride QueryRange (both request builders share get()).
	gotAuth, gotTenant = "", ""
	if _, err := c.QueryRange(context.Background(), "rate(up[5m])",
		providers.TimeWindow{Start: time.Unix(1700000000, 0), End: time.Unix(1700000300, 0)}, time.Minute); err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	if gotAuth != "Bearer s3cr3t" || gotTenant != "tenant-a" {
		t.Fatalf("QueryRange auth: auth=%q tenant=%q", gotAuth, gotTenant)
	}
}

func TestQueryNoAuthByDefault(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL).Query(context.Background(), "up", time.Time{}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("unconfigured client must send no Authorization header, got %q", gotAuth)
	}
}

func TestQueryTokenEnvUnset(t *testing.T) {
	// TokenEnv names a var that is not set ⇒ no Authorization header (treated as
	// keyless), mirroring the model provider's empty-key behaviour.
	t.Setenv("RUNLORE_TEST_VM_TOKEN", "")
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"vector","result":[]}}`))
	}))
	defer srv.Close()

	c := NewWithAuth(srv.URL, "RUNLORE_TEST_VM_TOKEN", nil)
	if _, err := c.Query(context.Background(), "up", time.Time{}); err != nil {
		t.Fatalf("Query: %v", err)
	}
	if gotAuth != "" {
		t.Fatalf("empty token must send no Authorization header, got %q", gotAuth)
	}
}

func TestLabelValues(t *testing.T) {
	var gotPath string
	var gotMatch []string
	var gotStart, gotEnd string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotMatch = r.URL.Query()["match[]"]
		gotStart = r.URL.Query().Get("start")
		gotEnd = r.URL.Query().Get("end")
		_, _ = w.Write([]byte(`{"status":"success","data":["http_requests_total","up","process_cpu_seconds_total"]}`))
	}))
	defer srv.Close()

	vals, err := New(srv.URL).LabelValues(context.Background(), "__name__",
		[]string{`{namespace="apps"}`},
		providers.TimeWindow{Start: time.Unix(1700000000, 0), End: time.Unix(1700000300, 0)})
	if err != nil {
		t.Fatalf("LabelValues: %v", err)
	}
	if gotPath != "/api/v1/label/__name__/values" {
		t.Fatalf("path=%q", gotPath)
	}
	if len(gotMatch) != 1 || gotMatch[0] != `{namespace="apps"}` {
		t.Fatalf("match[]=%v", gotMatch)
	}
	if gotStart != "1700000000" || gotEnd != "1700000300" {
		t.Fatalf("start=%q end=%q", gotStart, gotEnd)
	}
	if len(vals) != 3 || vals[0] != "http_requests_total" || vals[2] != "process_cpu_seconds_total" {
		t.Fatalf("unexpected values: %v", vals)
	}
}

func TestLabelValuesEmptyMatchers(t *testing.T) {
	// Empty/blank matchers must not emit a match[] param (whole-TSDB enumeration).
	var gotMatch []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMatch = r.URL.Query()["match[]"]
		_, _ = w.Write([]byte(`{"status":"success","data":[]}`))
	}))
	defer srv.Close()

	if _, err := New(srv.URL).LabelValues(context.Background(), "job", []string{"", ""}, providers.TimeWindow{}); err != nil {
		t.Fatalf("LabelValues: %v", err)
	}
	if len(gotMatch) != 0 {
		t.Fatalf("want no match[] params, got %v", gotMatch)
	}
}

func TestLabelValuesError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"error","error":"bad matcher"}`))
	}))
	defer srv.Close()
	if _, err := New(srv.URL).LabelValues(context.Background(), "__name__", nil, providers.TimeWindow{}); err == nil {
		t.Fatal("expected error for status=error")
	}
}

func TestQueryScalarResult(t *testing.T) {
	// A scalar PromQL result (e.g. `scalar(...)` or `2+2`) is `[ts, "val"]` — not the
	// vector `[{metric,value}]` shape. It must parse into a single unlabeled sample
	// rather than surfacing a raw json.Unmarshal error to the model.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"scalar","result":[1700000000,"42"]}}`))
	}))
	defer srv.Close()

	s, err := New(srv.URL).Query(context.Background(), "scalar(up)", time.Time{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(s) != 1 || s[0].Value != 42 || len(s[0].Metric) != 0 {
		t.Fatalf("scalar must be one unlabeled sample value=42, got: %+v", s)
	}
}

func TestQueryStringResult(t *testing.T) {
	// A string PromQL result is also `[ts, "val"]`; parse the value string into a
	// single unlabeled sample without erroring (value is 0 when non-numeric).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"string","result":[1700000000,"1.5"]}}`))
	}))
	defer srv.Close()

	s, err := New(srv.URL).Query(context.Background(), `"1.5"`, time.Time{})
	if err != nil {
		t.Fatalf("Query: %v", err)
	}
	if len(s) != 1 || s[0].Value != 1.5 {
		t.Fatalf("string result must parse into one unlabeled sample, got: %+v", s)
	}
}

func TestQueryRangeClampGuard(t *testing.T) {
	// A huge window / tiny step would blow past Prometheus's ~11k-point limit; the
	// provider raises the step so the request stays bounded even if the caller
	// forgot to. 24h at 1s = 86400 points → must be coarsened well below the cap.
	var gotStep string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotStep = r.URL.Query().Get("step")
		_, _ = w.Write([]byte(`{"status":"success","data":{"resultType":"matrix","result":[]}}`))
	}))
	defer srv.Close()

	start := time.Unix(1700000000, 0)
	end := start.Add(24 * time.Hour)
	if _, err := New(srv.URL).QueryRange(context.Background(), "up",
		providers.TimeWindow{Start: start, End: end}, time.Second); err != nil {
		t.Fatalf("QueryRange: %v", err)
	}
	stepSec, _ := strconv.Atoi(gotStep)
	if stepSec <= 1 {
		t.Fatalf("step must be raised above 1s to bound points, got %qs", gotStep)
	}
	points := int((24 * time.Hour).Seconds()) / stepSec
	if points > maxRangePoints {
		t.Fatalf("clamped request still %d points, want <= %d (step=%ss)", points, maxRangePoints, gotStep)
	}
}

func TestQueryError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"error","error":"bad query"}`))
	}))
	defer srv.Close()
	if _, err := New(srv.URL).Query(context.Background(), "(", time.Time{}); err == nil {
		t.Fatal("expected error for status=error")
	}
}
