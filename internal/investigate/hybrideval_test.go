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
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
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

// --- CI hybrid retrieval quality: Recall@k / MRR through the REAL SearchHybrid ---

// writeHybridEvalCatalog is writeEvalCatalog with vectors: the same fixture KB and
// real bleve index, PLUS bag-of-words embeddings built through the very ReloadContext
// path production uses (NewEmpty → SetEmbedder → ReloadContext, incl. the N2
// content-hash vector cache). The deterministic embedder makes that cache transparent:
// same text → same vector → same cache, every run.
func writeHybridEvalCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	dir := t.TempDir()
	for name, md := range evalCatalogEntries {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(md), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cat := catalog.NewEmpty()
	cat.SetEmbedder(newBowEmbedder())
	if _, err := cat.ReloadContext(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if !cat.HasVectors() {
		t.Fatal("fixture catalog has no vectors — hybrid harness would silently test BM25")
	}
	if cat.Len() != len(evalCatalogEntries) {
		t.Fatalf("indexed %d entries, want %d", cat.Len(), len(evalCatalogEntries))
	}
	return cat
}

// computeRetrievalHybrid mirrors computeRetrieval (recalleval_test.go) verbatim —
// same window, same accumulation, same normalization — differing in ONE line: it
// ranks with the REAL cat.SearchHybrid (RRF candidate pool ordered by cosine)
// instead of cat.SearchScored. Kept apples-to-apples so its Recall@1 is directly
// comparable to the BM25 harness's (graduation criterion 2).
func computeRetrievalHybrid(t *testing.T, cat *catalog.Catalog, cases []evalCase) retrievalMetrics {
	t.Helper()
	m := retrievalMetrics{ranks: map[string]int{}}
	for _, c := range cases {
		if c.negative() {
			continue
		}
		m.positives++
		hits, err := cat.SearchHybrid(context.Background(), buildRecallQuery(c.request()), retrievalWindow)
		if err != nil {
			t.Fatalf("%s: SearchHybrid: %v", c.name, err)
		}
		rank := rankOfTarget(hits, c.targets)
		m.ranks[c.name] = rank
		switch {
		case rank == 0:
			// miss: no acceptable target in the top retrievalWindow
		case rank <= 1:
			m.h1, m.h3, m.h5 = m.h1+1, m.h3+1, m.h5+1
		case rank <= 3:
			m.h3, m.h5 = m.h3+1, m.h5+1
		case rank <= 5:
			m.h5++
		}
		if rank >= 1 {
			m.mrr += 1.0 / float64(rank)
		}
	}
	if m.positives > 0 {
		n := float64(m.positives)
		m.r1, m.r3, m.r5 = float64(m.h1)/n, float64(m.h3)/n, float64(m.h5)/n
		m.mrr /= n
	}
	return m
}

// PINNED AFTER FIRST HONEST RUN — transcribe from `go test -run
// TestHybridRecallEvalRetrieval -v`.
//
// Measured (bag-of-words CI regime, 13 positive cases):
//
//	[hybrid/bow] retrieval over 13 positive cases: Recall@1=0.62 Recall@3=0.77 Recall@5=0.92 MRR=0.731
//
// This is the MACHINERY baseline, NOT semantic quality: the bow embedder does bare
// whitespace tokenization (no stemming, no subword), so a hyphenated workload token
// ("harbor-registry") never matches the runbook's split "harbor"/"registry" — hence
// hybrid Recall@1 (8/13) sits BELOW the BM25 harness's 13/13 here. Graduation
// criterion 2 (hybrid Recall@1 ≥ BM25) is a claim about a REAL semantic embedder,
// measured live — never inferred from this deterministic proxy.
const wantHybridRetrievalHitsAt1 = 8 // 8/13 = Recall@1 0.62 (measured, see the metrics line above)

// TestHybridRecallEvalRetrieval measures hybrid ranking quality (Recall@1/3/5, MRR)
// over the REAL SearchHybrid fusion, before any gate. It prints the metrics for CI.
func TestHybridRecallEvalRetrieval(t *testing.T) {
	cat := writeHybridEvalCatalog(t)
	m := computeRetrievalHybrid(t, cat, evalCases())
	logRetrieval(t, "hybrid/bow", m)
	if wantHybridRetrievalHitsAt1 < 0 {
		t.Fatal("pin wantHybridRetrievalHitsAt1 from the -v output above before committing")
	}
	if m.positives != 13 {
		t.Fatalf("expected 13 positive cases, got %d", m.positives)
	}
	if m.h1 != wantHybridRetrievalHitsAt1 {
		t.Fatalf("hybrid Recall@1 hits = %d, want pinned %d (fusion changed — re-measure and re-pin deliberately)", m.h1, wantHybridRetrievalHitsAt1)
	}
}
