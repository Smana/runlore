// SPDX-License-Identifier: Apache-2.0

package investigate

// Hybrid-recall eval harness — the SearchHybrid counterpart of recalleval_test.go.
//
// CI regime: a deterministic bag-of-words embedder (fnv token buckets,
// L2-normalized — cosine ≈ lexical overlap) drives the REAL catalog.SearchHybrid
// fusion + the REAL production hybrid gates, with pinned honest baselines.
// It measures the MACHINERY and the gate philosophy, not semantic quality.
//
// Live regime (TestHybridRecallEvalLive): the same fixtures against a real
// /embeddings endpoint, env-gated; prints the cosine distributions the
// hybrid_min_score / hybrid_margin_gap defaults must be derived from. Semantic
// quality is ONLY measurable there — never pin its numbers into CI.

import (
	"context"
	"hash/fnv"
	"math"
	"strings"
	"testing"
)

// bowEmbedder is a deterministic bag-of-words embedder: each lowercase token is
// hashed into one of dims buckets, counts are L2-normalized. No network, no
// randomness — same text, same vector, forever.
type bowEmbedder struct{ dims int }

func newBowEmbedder() *bowEmbedder { return &bowEmbedder{dims: 512} }

func (b *bowEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	out := make([][]float32, len(texts))
	for i, text := range texts {
		v := make([]float32, b.dims)
		for _, tok := range strings.Fields(strings.ToLower(text)) {
			h := fnv.New32a()
			_, _ = h.Write([]byte(tok))
			v[h.Sum32()%uint32(b.dims)]++
		}
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if norm > 0 {
			n := float32(math.Sqrt(norm))
			for j := range v {
				v[j] /= n
			}
		}
		out[i] = v
	}
	return out, nil
}

// TestBowEmbedderDeterministicAndDiscriminative pins the embedder's contract:
// identical texts → cosine 1, overlapping texts → cosine strictly between the
// identical and disjoint cases, disjoint texts → cosine ~0.
func TestBowEmbedderDeterministicAndDiscriminative(t *testing.T) {
	b := newBowEmbedder()
	vs, err := b.Embed(context.Background(), []string{
		"harbor registry iam quota exhausted",
		"harbor registry iam quota exhausted",
		"harbor registry credential rotation",
		"kafka broker partitions isr",
	})
	if err != nil {
		t.Fatal(err)
	}
	cos := func(a, c []float32) float64 {
		var dot float64
		for i := range a {
			dot += float64(a[i]) * float64(c[i])
		}
		return dot // vectors are L2-normalized
	}
	if got := cos(vs[0], vs[1]); math.Abs(got-1.0) > 1e-6 {
		t.Fatalf("identical texts: cosine=%v, want 1.0", got)
	}
	overlap, disjoint := cos(vs[0], vs[2]), cos(vs[0], vs[3])
	if !(overlap > disjoint && overlap < 1.0) {
		t.Fatalf("cosine ordering broken: overlap=%v disjoint=%v", overlap, disjoint)
	}
	if disjoint > 0.10 {
		t.Fatalf("disjoint texts: cosine=%v, want ~0 (hash collisions only)", disjoint)
	}
}
