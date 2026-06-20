package catalog

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/blevesearch/bleve/v2"
)

// Catalog is an in-memory BM25 index over OKF entries.
type Catalog struct {
	index   bleve.Index
	entries []Entry
}

// Searcher is the read surface used by the kb_search tool.
type Searcher interface {
	Search(query string, k int) ([]Entry, error)
}

// New loads the OKF bundle at dir and builds an in-memory index.
func New(dir string) (*Catalog, error) {
	entries, err := Load(dir)
	if err != nil {
		return nil, err
	}
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
	return &Catalog{index: idx, entries: entries}, nil
}

// Len reports the number of indexed entries.
func (c *Catalog) Len() int { return len(c.entries) }

var _ Searcher = (*Catalog)(nil)

// Search returns up to k entries best matching the query (BM25).
func (c *Catalog) Search(query string, k int) ([]Entry, error) {
	q := bleve.NewMatchQuery(query)
	q.SetField("text")
	req := bleve.NewSearchRequestOptions(q, k, 0, false)
	res, err := c.index.Search(req)
	if err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	out := make([]Entry, 0, len(res.Hits))
	for _, hit := range res.Hits {
		i, err := strconv.Atoi(hit.ID)
		if err != nil || i < 0 || i >= len(c.entries) {
			continue
		}
		out = append(out, c.entries[i])
	}
	return out, nil
}
