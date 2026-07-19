// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"

	"github.com/blevesearch/bleve/v2"

	"github.com/Smana/runlore/internal/embed"
)

// Embedder turns texts into vectors (one per input, in order). Satisfied by
// internal/embed.Client — defined here (consumer side) so the catalog needn't import
// a concrete embedder.
type Embedder interface {
	Embed(ctx context.Context, texts []string) ([][]float32, error)
}

// HybridSearcher fuses BM25 and embedding similarity. HasVectors reports whether the
// vector side is live (an embedder configured AND vectors built for the corpus);
// callers fall back to SearchScored when it is false.
type HybridSearcher interface {
	SearchHybrid(ctx context.Context, query string, k int) ([]ScoredEntry, error)
	HasVectors() bool
}

var _ HybridSearcher = (*Catalog)(nil)

// SetEmbedder enables hybrid retrieval: entry vectors are (re)built on the next
// Reload. Call once at startup, before the first Reload/Search.
func (c *Catalog) SetEmbedder(e Embedder) { c.embedder = e }

// HasVectors reports whether hybrid retrieval is live — a vector per entry.
func (c *Catalog) HasVectors() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.vectors) > 0 && len(c.vectors) == len(c.entries)
}

// embedWithCache returns one vector per entry plus the refreshed cache, reusing
// cached vectors for unchanged texts and embedding only the missing subset (the
// client already chunks oversized batches). The hybrid invariant is
// ALL-OR-NOTHING — on any embed failure it returns (nil, previous cache):
// vectors drop for this reload (HasVectors goes false, recall degrades to BM25 —
// never a partial vector set that would silently exclude new entries from the
// cosine ranking), while the surviving cache makes the next attempt embed only
// what is still missing. nil embedder or empty corpus → (nil, nil).
func (c *Catalog) embedWithCache(ctx context.Context, entries []Entry) ([][]float32, map[string][]float32) {
	if c.embedder == nil || len(entries) == 0 {
		return nil, nil
	}
	c.mu.RLock()
	prev := c.vecCache
	c.mu.RUnlock()

	keys := make([]string, len(entries))
	vectors := make([][]float32, len(entries))
	var missTexts []string
	var missIdx []int
	for i, e := range entries {
		text := entryText(e)
		sum := sha256.Sum256([]byte(text))
		keys[i] = hex.EncodeToString(sum[:])
		if v, ok := prev[keys[i]]; ok {
			vectors[i] = v
			continue
		}
		missTexts = append(missTexts, text)
		missIdx = append(missIdx, i)
	}
	if len(missTexts) > 0 {
		vecs, err := c.embedder.Embed(ctx, missTexts)
		if err != nil {
			if c.Log != nil {
				c.Log.Warn("catalog embed failed; hybrid recall degrades to BM25-only until the next successful sync",
					"missing", len(missTexts), "entries", len(entries), "err", err)
			}
			return nil, prev
		}
		for j, v := range vecs {
			vectors[missIdx[j]] = v
		}
	}
	cache := make(map[string][]float32, len(entries))
	for i, k := range keys {
		cache[k] = vectors[i]
	}
	return vectors, cache
}

// SearchHybrid combines lexical and semantic retrieval for instant recall. It pools
// candidates by Reciprocal Rank Fusion of the BM25 and embedding-cosine rankings (so
// an entry strong in EITHER signal is reachable), then returns up to k of them
// ORDERED BY COSINE with Score set to the cosine similarity — a [0,1] semantic
// confidence the recall gate keys off, kept score-descending so the gate's
// top-vs-runner-up margin stays well-defined. Falls back to BM25 (SearchScored) when
// vectors are unavailable or the query can't be embedded — hybrid never regresses
// recall, it only adds a vector signal when one is present.
//
// The fusion choice and the cosine gate thresholds are eval-tunable knobs: recall
// records its score even on rejection (see recall.lookup) so they can be measured
// against the live instant-recall scenarios rather than guessed.
func (c *Catalog) SearchHybrid(ctx context.Context, query string, k int) ([]ScoredEntry, error) {
	if !c.HasVectors() || c.embedder == nil {
		return c.SearchScored(query, k)
	}
	qv, err := c.embedder.Embed(ctx, []string{query}) // network call — outside the lock
	if err != nil || len(qv) == 0 {
		return c.SearchScored(query, k)
	}

	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.index == nil || len(c.vectors) != len(c.entries) {
		return nil, nil
	}
	// BM25 ranking (entry-index ids in score order).
	bm25IDs, err := c.bm25IDsLocked(query, k)
	if err != nil {
		return nil, err
	}
	// Cosine over the whole (small, in-RAM) corpus → similarity + a cosine ranking.
	cos := make([]float64, len(c.vectors))
	ranked := make([]int, len(c.vectors))
	for i, v := range c.vectors {
		cos[i] = embed.Cosine(qv[0], v)
		ranked[i] = i
	}
	sort.SliceStable(ranked, func(a, b int) bool { return cos[ranked[a]] > cos[ranked[b]] })
	cosIDs := make([]string, len(ranked))
	for i, idx := range ranked {
		cosIDs[i] = strconv.Itoa(idx)
	}
	// Fuse the two rankings to select the candidate pool, then order that pool by
	// cosine (the gate score).
	pool := embed.FuseRRF(0, bm25IDs, cosIDs)
	out := make([]ScoredEntry, 0, len(pool))
	for _, f := range pool {
		i, cerr := strconv.Atoi(f.ID)
		if cerr != nil || i < 0 || i >= len(c.entries) {
			continue
		}
		out = append(out, ScoredEntry{Entry: c.entries[i], Score: cos[i]})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].Score > out[b].Score })
	if len(out) > k {
		out = out[:k]
	}
	return out, nil
}

// bm25IDsLocked returns entry-index ids ranked by BM25. The caller MUST hold the read
// lock (it reads c.index directly — must not re-lock, RWMutex is not reentrant).
func (c *Catalog) bm25IDsLocked(query string, k int) ([]string, error) {
	q := bleve.NewMatchQuery(query)
	q.SetField("text")
	req := bleve.NewSearchRequestOptions(q, k, 0, false)
	res, err := c.index.Search(req)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	ids := make([]string, 0, len(res.Hits))
	for _, hit := range res.Hits {
		ids = append(ids, hit.ID)
	}
	return ids, nil
}
