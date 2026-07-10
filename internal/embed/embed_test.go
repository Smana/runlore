// SPDX-License-Identifier: Apache-2.0

package embed

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestEmbed(t *testing.T) {
	var gotModel string
	var gotInput []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req embedRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		gotModel, gotInput = req.Model, req.Input
		// Return vectors deliberately OUT OF ORDER to exercise index placement.
		_, _ = w.Write([]byte(`{"data":[
			{"index":1,"embedding":[0.0,1.0]},
			{"index":0,"embedding":[1.0,0.0]}]}`))
	}))
	defer srv.Close()

	vecs, err := New(srv.URL, "text-embed", "k").Embed(context.Background(), []string{"a", "b"})
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if gotModel != "text-embed" || len(gotInput) != 2 {
		t.Fatalf("request: model=%q input=%v", gotModel, gotInput)
	}
	if len(vecs) != 2 || vecs[0][0] != 1.0 || vecs[1][1] != 1.0 {
		t.Fatalf("vectors not placed by index: %v", vecs)
	}
}

func TestEmbedEmptyInput(t *testing.T) {
	v, err := New("http://unused", "m", "").Embed(context.Background(), nil)
	if err != nil || v != nil {
		t.Fatalf("empty input should no-op, got (%v, %v)", v, err)
	}
}

func TestEmbedCountMismatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"data":[{"index":0,"embedding":[1.0]}]}`)) // 1 vector for 2 inputs
	}))
	defer srv.Close()
	if _, err := New(srv.URL, "m", "").Embed(context.Background(), []string{"a", "b"}); err == nil {
		t.Fatal("want error on vector/input count mismatch")
	}
}

func TestEmbedNon2xxOmitsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req-9")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(`secret=sk-LEAKED upstream detail`))
	}))
	defer srv.Close()
	_, err := New(srv.URL, "m", "").Embed(context.Background(), []string{"a"})
	if err == nil {
		t.Fatal("want error for non-2xx")
	}
	if strings.Contains(err.Error(), "sk-LEAKED") || strings.Contains(err.Error(), "upstream detail") {
		t.Fatalf("error leaked upstream body: %v", err)
	}
	if !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "req-9") {
		t.Fatalf("error should carry status + request-id: %v", err)
	}
}

func TestCosine(t *testing.T) {
	cases := []struct {
		name string
		a, b []float32
		want float64
	}{
		{"identical", []float32{1, 0, 1}, []float32{1, 0, 1}, 1},
		{"orthogonal", []float32{1, 0}, []float32{0, 1}, 0},
		{"opposite", []float32{1, 1}, []float32{-1, -1}, -1},
		{"mismatched length", []float32{1, 0}, []float32{1}, 0},
		{"zero vector", []float32{0, 0}, []float32{1, 1}, 0},
		{"empty", nil, nil, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Cosine(tc.a, tc.b); math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("Cosine = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestFuseRRF(t *testing.T) {
	// "b" ranks top of BOTH lists → should win; "a" is second in both; "z" appears
	// in only one list, low → last. (Symmetric ranks would tie, so both lists agree
	// on the b>a order to make the winner unambiguous.)
	bm25 := []string{"b", "a", "z"}
	vec := []string{"b", "a"}
	fused := FuseRRF(0, bm25, vec) // k<=0 → default 60

	if len(fused) != 3 {
		t.Fatalf("want 3 fused ids, got %d: %+v", len(fused), fused)
	}
	if fused[0].ID != "b" {
		t.Fatalf("'b' (high in both rankings) should rank first, got %q (%+v)", fused[0].ID, fused)
	}
	if fused[len(fused)-1].ID != "z" {
		t.Fatalf("'z' (single low appearance) should rank last, got %q", fused[len(fused)-1].ID)
	}
	// Scores must be descending.
	for i := 1; i < len(fused); i++ {
		if fused[i].Score > fused[i-1].Score {
			t.Fatalf("fused scores not descending: %+v", fused)
		}
	}
}
