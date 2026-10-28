// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestFiring(t *testing.T) {
	var gotQuery map[string][]string
	body, code := `[{"labels":{"alertname":"KubePodCrashLooping"}}]`, 200
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v2/alerts" {
			http.NotFound(w, r)
			return
		}
		gotQuery = r.URL.Query()
		w.WriteHeader(code)
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()
	ctx := context.Background()

	got, err := Firing(ctx, srv.Client(), srv.URL+"/", "KubePodCrashLooping", "apps")
	if err != nil || !got {
		t.Fatalf("one alert fires: %v %v", got, err)
	}
	for k, want := range map[string][]string{
		"active": {"true"}, "silenced": {"false"}, "inhibited": {"false"},
		"filter": {`alertname="KubePodCrashLooping"`, `namespace="apps"`},
	} {
		if g := gotQuery[k]; len(g) != len(want) || g[0] != want[0] || (len(want) == 2 && g[1] != want[1]) {
			t.Fatalf("query %s = %v, want %v", k, g, want)
		}
	}

	// No namespace in the ledger: only the alertname filter.
	if _, err := Firing(ctx, srv.Client(), srv.URL, "KubePodCrashLooping", ""); err != nil || len(gotQuery["filter"]) != 1 {
		t.Fatalf("alertname only: %v %v", err, gotQuery["filter"])
	}
	// A value with a quote cannot break out of the matcher.
	if _, err := Firing(ctx, srv.Client(), srv.URL, `a"b`, ""); err != nil || gotQuery["filter"][0] != `alertname="a\"b"` {
		t.Fatalf("escaping: %v %v", err, gotQuery["filter"])
	}

	body = `[]`
	if got, err := Firing(ctx, srv.Client(), srv.URL, "x", "apps"); err != nil || got {
		t.Fatalf("empty list is not firing: %v %v", got, err)
	}
	body, code = `oops`, 500
	if _, err := Firing(ctx, srv.Client(), srv.URL, "x", "apps"); err == nil {
		t.Fatal("a 500 is an error, not a verdict")
	}
	body, code = `not json`, 200
	if _, err := Firing(ctx, srv.Client(), srv.URL, "x", "apps"); err == nil {
		t.Fatal("malformed JSON is an error")
	}
	srv.Close()
	if _, err := Firing(ctx, srv.Client(), srv.URL, "x", "apps"); err == nil {
		t.Fatal("a closed server is an error")
	}
}
