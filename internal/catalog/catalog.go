// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/blevesearch/bleve/v2"
	"github.com/blevesearch/bleve/v2/mapping"
)

// Catalog is an in-memory BM25 index over OKF entries. It is safe for concurrent
// Search while a background sync calls Reload (the index is swapped atomically).
type Catalog struct {
	mu      sync.RWMutex
	index   bleve.Index
	entries []Entry
	// vectors holds one embedding per entry (parallel to entries), built on Reload
	// only when an embedder is configured; nil keeps the catalog BM25-only. embedder
	// is set once at startup (SetEmbedder) before the first Reload/Search.
	vectors  [][]float32
	embedder Embedder
	ready    atomic.Bool // set on first successful Reload; gates readyz on catalog warmth
	// vecCache maps sha256(entryText) → embedding, carried ACROSS reloads so a KB
	// sync only embeds entries whose text actually changed (RunLore merges its own
	// PRs — without this, every merge re-embeds the whole corpus). Rebuilt from the
	// current corpus on each successful embed pass so deleted entries are evicted.
	// Guarded by mu.
	vecCache map[string][]float32
	// vecCachePath/vecCacheModel arm disk persistence of vecCache (EnableVectorCache).
	// Empty path ⇒ in-memory only. Set once at wiring time, before the first Reload.
	vecCachePath  string
	vecCacheModel string
	// pathIdx maps Entry.Path → position in entries; maintained in lockstep with
	// entries under mu. It exists because bleve docs are keyed by Path (stable
	// across reloads — the prerequisite for incremental Index/Delete), so search
	// hits resolve IDs through it instead of parsing positions.
	pathIdx map[string]int
	// Log, when set (wiring time, before the first Reload), surfaces non-fatal
	// reload degradations — an embed failure that leaves hybrid BM25-only. Nil-safe.
	Log *slog.Logger
}

// pathIndex maps each entry's Path to its slice position — the resolution table
// for path-keyed bleve doc IDs. Built alongside every entries swap.
func pathIndex(entries []Entry) map[string]int {
	m := make(map[string]int, len(entries))
	for i, e := range entries {
		m[e.Path] = i
	}
	return m
}

// EnableVectorCache persists the embedding cache at path, keyed to the given
// embedding-model identifier: the file is loaded now (before the first reload —
// a warm cache means a restart embeds nothing) and rewritten after each
// successful embed pass. Any load problem is a cold start, never an error.
func (c *Catalog) EnableVectorCache(path, model string) {
	c.vecCachePath, c.vecCacheModel = path, model
	if warm := loadVecCache(path, model, c.Log); warm != nil {
		c.mu.Lock()
		c.vecCache = warm
		c.mu.Unlock()
		if c.Log != nil {
			c.Log.Info("vector cache loaded from disk", "path", path, "vectors", len(warm))
		}
	}
}

// persistVecCache is the post-reload save hook: only a successful, complete
// embed pass (vectors non-nil ⇒ all-or-nothing invariant held) writes the file.
func (c *Catalog) persistVecCache(vectors [][]float32, cache map[string][]float32) {
	if c.vecCachePath == "" || vectors == nil || cache == nil {
		return
	}
	if err := saveVecCache(c.vecCachePath, c.vecCacheModel, cache); err != nil && c.Log != nil {
		c.Log.Warn("vector cache save failed; next restart re-embeds", "path", c.vecCachePath, "err", err)
	}
}

// Searcher is the read surface used by the kb_search tool.
type Searcher interface {
	Search(query string, k int) ([]Entry, error)
}

// newIndexMapping returns the index mapping used by every catalog index. It pins
// the scoring model to BM25 — bleve defaults to legacy TF-IDF when ScoringModel
// is unset, whose unbounded, non-saturating scores are not corpus-portable.
func newIndexMapping() *mapping.IndexMappingImpl {
	im := bleve.NewIndexMapping()
	im.ScoringModel = "bm25" // validated by bleve against SupportedScoringModels
	return im
}

// New loads the OKF bundle at dir and builds an in-memory index.
func New(dir string) (*Catalog, error) {
	c := &Catalog{}
	if _, err := c.Reload(dir); err != nil {
		return nil, err
	}
	return c, nil
}

// NewEmpty returns a catalog with no entries — used before the first git sync.
func NewEmpty() *Catalog {
	idx, _ := bleve.NewMemOnly(newIndexMapping())
	return &Catalog{index: idx, pathIdx: map[string]int{}}
}

// Reload rebuilds the index from dir and swaps it in atomically. The new index is
// built outside the lock so concurrent Search is only blocked for the swap. It
// returns the list of skipped (unparseable) entry paths so the caller can warn;
// these are non-fatal — the good entries are still indexed.
func (c *Catalog) Reload(dir string) ([]string, error) {
	return c.ReloadContext(context.Background(), dir)
}

// ReloadContext is Reload with a caller context bounding the (optional) embedding
// pass. When an embedder is configured, entry vectors are rebuilt here; an embedding
// failure is NON-fatal — vectors fall back to nil and the catalog stays BM25-only
// (hybrid retrieval degrades gracefully rather than breaking the reload).
func (c *Catalog) ReloadContext(ctx context.Context, dir string) ([]string, error) {
	entries, skipped, err := Load(dir)
	if err != nil {
		return nil, err
	}
	idx, err := buildIndex(entries)
	if err != nil {
		return nil, err
	}
	vectors, cache := c.embedWithCache(ctx, entries)
	c.mu.Lock()
	old := c.index
	c.index, c.entries, c.vectors, c.pathIdx = idx, entries, vectors, pathIndex(entries)
	if cache != nil {
		c.vecCache = cache
	}
	c.mu.Unlock()
	// Release the previous index's resources. Search holds the read lock for the
	// whole query, so by the time the swap above acquired the write lock no query
	// is still using `old` — closing it here is safe and prevents a bleve index
	// leaking on every git-sync reload.
	if old != nil {
		_ = old.Close()
	}
	c.persistVecCache(vectors, cache)
	c.ready.Store(true)
	return skipped, nil
}

// ReloadDelta refreshes the catalog for a known set of changed/removed paths,
// mutating the live index (bleve is safe for concurrent search+index) instead
// of rebuilding it. Entries, vectors, and the embed cache still refresh from a
// full Load walk — files are cheap; only the bleve analysis pass is not — so
// slice/vector/pathIdx consistency is trivially preserved. Falls back to a
// full ReloadContext when the delta is nil (unknown), the catalog is cold, or
// ANY index mutation fails: an incremental error must never leave a wrong
// index behind.
func (c *Catalog) ReloadDelta(ctx context.Context, dir string, delta *SyncDelta) ([]string, error) {
	if delta == nil || !c.ready.Load() {
		return c.ReloadContext(ctx, dir)
	}
	entries, skipped, err := Load(dir)
	if err != nil {
		return nil, err
	}
	vectors, cache := c.embedWithCache(ctx, entries)
	loaded := pathIndex(entries)

	// Mutate the live index outside c.mu: concurrent searches resolve hits via
	// pathIdx, so a just-indexed unknown path is skipped and a just-deleted one
	// simply stops matching — momentary staleness, never a wrong entry.
	c.mu.RLock()
	idx := c.index
	c.mu.RUnlock()
	incremental := func() error {
		for _, p := range delta.Removed {
			if err := idx.Delete(p); err != nil {
				return fmt.Errorf("delete %s: %w", p, err)
			}
		}
		for _, p := range delta.Changed {
			i, ok := loaded[p]
			if !ok {
				// Changed upstream but skipped by Load (now unparseable, or a
				// reserved/non-.md name): drop any stale doc.
				if err := idx.Delete(p); err != nil {
					return fmt.Errorf("delete stale %s: %w", p, err)
				}
				continue
			}
			if err := idx.Index(p, docFor(entries[i])); err != nil {
				return fmt.Errorf("index %s: %w", p, err)
			}
		}
		return nil
	}
	if err := incremental(); err != nil {
		if c.Log != nil {
			c.Log.Warn("catalog incremental re-index failed; rebuilding fully", "err", err)
		}
		return c.ReloadContext(ctx, dir)
	}
	c.mu.Lock()
	c.entries, c.vectors, c.pathIdx = entries, vectors, loaded
	if cache != nil {
		c.vecCache = cache
	}
	c.mu.Unlock()
	c.persistVecCache(vectors, cache)
	return skipped, nil
}

func buildIndex(entries []Entry) (bleve.Index, error) {
	idx, err := bleve.NewMemOnly(newIndexMapping())
	if err != nil {
		return nil, fmt.Errorf("new index: %w", err)
	}
	for _, e := range entries {
		if err := idx.Index(e.Path, docFor(e)); err != nil {
			return nil, fmt.Errorf("index entry %s: %w", e.Path, err)
		}
	}
	return idx, nil
}

// docFor is the single bleve document shape per entry — shared by the full
// buildIndex pass and the incremental ReloadDelta re-index so both views index
// identical content.
func docFor(e Entry) map[string]any {
	return map[string]any{
		"title": e.Title,
		"text":  entryText(e),
	}
}

// entryText is the single corpus text per entry — used for BOTH the BM25 doc and the
// embedding, so the lexical and vector views index the same content. Resource is
// included so a query naming the affected object/pattern gets lexical lift (it also
// drives the recall structural filter — same signal, now scored).
func entryText(e Entry) string {
	return strings.Join([]string{e.Title, e.Description, e.Resource, strings.Join(e.Tags, " "), e.Body}, " ")
}

// Len reports the number of indexed entries.
func (c *Catalog) Len() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.entries)
}

// Entries returns a snapshot of the currently-indexed entries. Used by callers
// that validate catalog content out-of-band (kbvalidate at load time).
func (c *Catalog) Entries() []Entry {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Entry, len(c.entries))
	copy(out, c.entries)
	return out
}

// FindFingerprint returns the entry whose frontmatter fingerprint equals fp —
// the exact-identity lookup behind the curator's deterministic catalog dedup
// (curated entries persist their DupFingerprint; see forge renderEntry). An empty
// fp never matches: hand-written entries carry no fingerprint, and "" means
// "nothing to key on".
func (c *Catalog) FindFingerprint(fp string) (Entry, bool) {
	if fp == "" {
		return Entry{}, false
	}
	c.mu.RLock()
	defer c.mu.RUnlock()
	for _, e := range c.entries {
		if e.Fingerprint == fp {
			return e, true
		}
	}
	return Entry{}, false
}

// Ready reports whether the catalog has completed at least one successful load.
// It stays false for a git-sync catalog (NewEmpty) until the first sync indexes,
// so readyz can keep the leader out of rotation until its KB is warm.
func (c *Catalog) Ready() bool {
	return c.ready.Load()
}

// ScoredEntry is a search hit with its BM25 relevance score.
type ScoredEntry struct {
	Entry Entry
	Score float64
}

// ScoredSearcher exposes relevance scores (used by instant recall).
type ScoredSearcher interface {
	SearchScored(query string, k int) ([]ScoredEntry, error)
}

var (
	_ Searcher       = (*Catalog)(nil)
	_ ScoredSearcher = (*Catalog)(nil)
)

// Search returns up to k entries best matching the query (BM25).
func (c *Catalog) Search(query string, k int) ([]Entry, error) {
	scored, err := c.SearchScored(query, k)
	if err != nil {
		return nil, err
	}
	out := make([]Entry, len(scored))
	for i, s := range scored {
		out[i] = s.Entry
	}
	return out, nil
}

// SearchScored returns up to k hits with their relevance scores.
func (c *Catalog) SearchScored(query string, k int) ([]ScoredEntry, error) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	if c.index == nil {
		return nil, nil
	}
	q := bleve.NewMatchQuery(query)
	q.SetField("text")
	req := bleve.NewSearchRequestOptions(q, k, 0, false)
	res, err := c.index.Search(req)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	out := make([]ScoredEntry, 0, len(res.Hits))
	for _, hit := range res.Hits {
		i, ok := c.pathIdx[hit.ID]
		if !ok {
			continue
		}
		out = append(out, ScoredEntry{Entry: c.entries[i], Score: hit.Score})
	}
	return out, nil
}
