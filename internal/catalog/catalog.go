package catalog

import (
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/blevesearch/bleve/v2"
)

// Catalog is an in-memory BM25 index over OKF entries. It is safe for concurrent
// Search while a background sync calls Reload (the index is swapped atomically).
type Catalog struct {
	mu      sync.RWMutex
	index   bleve.Index
	entries []Entry
}

// Searcher is the read surface used by the kb_search tool.
type Searcher interface {
	Search(query string, k int) ([]Entry, error)
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
	idx, _ := bleve.NewMemOnly(bleve.NewIndexMapping())
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
	c.index, c.entries = idx, entries
	c.mu.Unlock()
	return skipped, nil
}

func buildIndex(entries []Entry) (bleve.Index, error) {
	idx, err := bleve.NewMemOnly(bleve.NewIndexMapping())
	if err != nil {
		return nil, fmt.Errorf("new index: %w", err)
	}
	for i, e := range entries {
		doc := map[string]any{
			"title": e.Title,
			"text":  strings.Join([]string{e.Title, e.Description, strings.Join(e.Tags, " "), e.Body}, " "),
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
