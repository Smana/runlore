package catalog

import (
	"fmt"
	"strconv"
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
	ready   atomic.Bool // set on first successful Reload; gates readyz on catalog warmth
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
	return &Catalog{index: idx}
}

// Reload rebuilds the index from dir and swaps it in atomically. The new index is
// built outside the lock so concurrent Search is only blocked for the swap. It
// returns the list of skipped (unparseable) entry paths so the caller can warn;
// these are non-fatal — the good entries are still indexed.
func (c *Catalog) Reload(dir string) ([]string, error) {
	entries, skipped, err := Load(dir)
	if err != nil {
		return nil, err
	}
	idx, err := buildIndex(entries)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	old := c.index
	c.index, c.entries = idx, entries
	c.mu.Unlock()
	// Release the previous index's resources. Search holds the read lock for the
	// whole query, so by the time the swap above acquired the write lock no query
	// is still using `old` — closing it here is safe and prevents a bleve index
	// leaking on every git-sync reload.
	if old != nil {
		_ = old.Close()
	}
	c.ready.Store(true)
	return skipped, nil
}

func buildIndex(entries []Entry) (bleve.Index, error) {
	idx, err := bleve.NewMemOnly(newIndexMapping())
	if err != nil {
		return nil, fmt.Errorf("new index: %w", err)
	}
	for i, e := range entries {
		doc := map[string]any{
			"title": e.Title,
			// Resource is included so a query naming the affected object/pattern gets
			// lexical lift (it also drives the recall structural filter — same signal,
			// now scored). Without it a resource-only term contributes nothing to BM25.
			"text": strings.Join([]string{e.Title, e.Description, e.Resource, strings.Join(e.Tags, " "), e.Body}, " "),
		}
		if err := idx.Index(strconv.Itoa(i), doc); err != nil {
			return nil, fmt.Errorf("index entry %d: %w", i, err)
		}
	}
	return idx, nil
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
		i, err := strconv.Atoi(hit.ID)
		if err != nil || i < 0 || i >= len(c.entries) {
			continue
		}
		out = append(out, ScoredEntry{Entry: c.entries[i], Score: hit.Score})
	}
	return out, nil
}
