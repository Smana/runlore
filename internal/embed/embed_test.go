// SPDX-License-Identifier: Apache-2.0

package embed

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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

// TestEmbedChunksLargeBatches proves Embed splits oversized input into bounded
// per-request batches while preserving input order across chunk boundaries.
func TestEmbedChunksLargeBatches(t *testing.T) {
	const n = 600 // 256 + 256 + 88
	var mu sync.Mutex
	var batchSizes []int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Input []string `json:"input"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode: %v", err)
		}
		mu.Lock()
		batchSizes = append(batchSizes, len(req.Input))
		mu.Unlock()
		// Echo each input's numeric suffix back as its vector so the test can
		// verify global ordering end-to-end ("t42" → [42]).
		type datum struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		out := struct {
			Data []datum `json:"data"`
		}{}
		for i, s := range req.Input {
			v, err := strconv.Atoi(strings.TrimPrefix(s, "t"))
			if err != nil {
				t.Errorf("unexpected input %q", s)
			}
			out.Data = append(out.Data, datum{Index: i, Embedding: []float32{float32(v)}})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	texts := make([]string, n)
	for i := range texts {
		texts[i] = "t" + strconv.Itoa(i)
	}
	c := New(srv.URL, "test-model", "")
	got, err := c.Embed(context.Background(), texts)
	if err != nil {
		t.Fatalf("Embed: %v", err)
	}
	if len(got) != n {
		t.Fatalf("got %d vectors, want %d", len(got), n)
	}
	for i, v := range got {
		if len(v) != 1 || v[0] != float32(i) {
			t.Fatalf("vector %d = %v, want [%d] (order broken across chunks)", i, v, i)
		}
	}
	wantSizes := []int{256, 256, 88}
	if !slices.Equal(batchSizes, wantSizes) {
		t.Fatalf("batch sizes = %v, want %v", batchSizes, wantSizes)
	}
}

// TestEmbedChunkFailurePropagates proves a failure in a later chunk fails the
// whole call (no partial result).
func TestEmbedChunkFailurePropagates(t *testing.T) {
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if calls.Add(1) > 1 { // first chunk OK, second chunk 500s
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		var req struct {
			Input []string `json:"input"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		type datum struct {
			Index     int       `json:"index"`
			Embedding []float32 `json:"embedding"`
		}
		out := struct {
			Data []datum `json:"data"`
		}{}
		for i := range req.Input {
			out.Data = append(out.Data, datum{Index: i, Embedding: []float32{1}})
		}
		_ = json.NewEncoder(w).Encode(out)
	}))
	defer srv.Close()

	texts := make([]string, 300) // 2 chunks
	for i := range texts {
		texts[i] = "x"
	}
	c := New(srv.URL, "test-model", "")
	if _, err := c.Embed(context.Background(), texts); err == nil {
		t.Fatal("want error when a chunk fails, got nil")
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
