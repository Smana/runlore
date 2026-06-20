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

func TestQueryError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"error","error":"bad query"}`))
	}))
	defer srv.Close()
	if _, err := New(srv.URL).Query(context.Background(), "(", time.Time{}); err == nil {
		t.Fatal("expected error for status=error")
	}
}
