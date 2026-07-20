// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"fmt"
	"hash/fnv"
	"math"
	"os"
	"path/filepath"
	"testing"
)

// Hermetic hot-path benchmarks for the catalog (audit 2026-07-19, roadmap Later
// wave). Fixtures are ~200 synthetic OKF entries. Run with:
//
//	go test ./internal/catalog/ -bench Benchmark -benchtime 5x -run '^$'

const benchCorpusSize = 200

// writeBenchCorpus writes n deterministic OKF entries and returns the dir.
// Each entry shares common vocabulary ("failure", "rollout") and carries unique
// tokens (svc-i, ns-i%10) so BM25 and the bag-of-words vectors both discriminate.
func writeBenchCorpus(tb testing.TB, n int) string {
	tb.Helper()
	dir := tb.TempDir()
	for i := 0; i < n; i++ {
		body := fmt.Sprintf(`---
type: Incident
title: svc-%d rollout failure in ns-%d
description: HelmRelease svc-%d upgrade failed with probe timeouts
resource: ns-%d/deployment/svc-%d
tags: [rollout, svc-%d]
---
The svc-%d deployment in ns-%d failed its rollout: readiness probes timed out
after the config change. Resolution: revert the values change and reconcile.
`, i, i%10, i, i%10, i, i, i, i%10)
		p := filepath.Join(dir, fmt.Sprintf("entry-%03d.md", i))
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			tb.Fatal(err)
		}
	}
	return dir
}

// benchEmbedder is a minimal deterministic bag-of-words embedder (fnv token
// buckets, L2-normalized). Replicates the shape of the eval's embedder; kept
// local because that one is unexported test code in another package.
type benchEmbedder struct{}

func (benchEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	const dims = 256
	out := make([][]float32, len(texts))
	for i, t := range texts {
		v := make([]float32, dims)
		start := -1
		for j := 0; j <= len(t); j++ {
			if j < len(t) && (t[j] >= 'a' && t[j] <= 'z' || t[j] >= '0' && t[j] <= '9') {
				if start < 0 {
					start = j
				}
				continue
			}
			if start >= 0 {
				h := fnv.New32a()
				_, _ = h.Write([]byte(t[start:j]))
				v[h.Sum32()%dims]++
				start = -1
			}
		}
		var norm float64
		for _, x := range v {
			norm += float64(x) * float64(x)
		}
		if norm > 0 {
			n := float32(math.Sqrt(norm))
			for k := range v {
				v[k] /= n
			}
		}
		out[i] = v
	}
	return out, nil
}

// warmCatalog returns a loaded catalog over dir; hybrid additionally wires the
// deterministic embedder and builds vectors.
func warmCatalog(tb testing.TB, dir string, hybrid bool) *Catalog {
	tb.Helper()
	c := NewEmpty()
	if hybrid {
		c.SetEmbedder(benchEmbedder{})
	}
	if _, err := c.ReloadContext(context.Background(), dir); err != nil {
		tb.Fatal(err)
	}
	if hybrid && !c.HasVectors() {
		tb.Fatal("bench: vectors not built")
	}
	return c
}

// BenchmarkReloadBM25 guards the cost of a full BM25 index rebuild — the price
// paid on every KB HEAD move regardless of embeddings.
func BenchmarkReloadBM25(b *testing.B) {
	dir := writeBenchCorpus(b, benchCorpusSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := NewEmpty()
		if _, err := c.Reload(dir); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReloadEmbedColdCache guards the worst-case reload: index rebuild plus
// embedding the ENTIRE corpus with an empty vector cache (first boot, cache loss).
func BenchmarkReloadEmbedColdCache(b *testing.B) {
	dir := writeBenchCorpus(b, benchCorpusSize)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := NewEmpty()
		c.SetEmbedder(benchEmbedder{})
		if _, err := c.ReloadContext(context.Background(), dir); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkReloadEmbedWarmCache guards the steady-state reload after PR #328:
// unchanged entries must hit the content-hash cache, so a warm reload should sit
// close to BenchmarkReloadBM25, far below the cold-cache cost. A collapse of this
// gap means the cache regressed.
func BenchmarkReloadEmbedWarmCache(b *testing.B) {
	dir := writeBenchCorpus(b, benchCorpusSize)
	c := warmCatalog(b, dir, true) // warm the vecCache once, unmeasured
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.ReloadContext(context.Background(), dir); err != nil {
			b.Fatal(err)
		}
	}
}
