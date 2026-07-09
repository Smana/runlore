// SPDX-License-Identifier: Apache-2.0

package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
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

func TestQueryError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"error","error":"bad query"}`))
	}))
	defer srv.Close()
	if _, err := New(srv.URL).Query(context.Background(), "(", time.Time{}); err == nil {
		t.Fatal("expected error for status=error")
	}
}
