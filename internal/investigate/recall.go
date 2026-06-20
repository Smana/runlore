package investigate

import (
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// Recall short-circuits an investigation when the knowledge catalog already has a
// high-confidence answer for the symptom — skipping the slow, paid ReAct loop. It
// is opt-in (off by default) with a tunable MinScore, since BM25 scores are
// corpus-dependent and a stale hit shouldn't silently replace an investigation.
type Recall struct {
	Catalog  catalog.ScoredSearcher
	MinScore float64
}

// lookup returns the best catalog entry for the request and its score if it meets
// the threshold, else (nil, 0).
func (r *Recall) lookup(req Request) (*catalog.Entry, float64) {
	if r == nil || r.Catalog == nil {
		return nil, 0
	}
	// Query the symptom (title + message); severity/reason is noise for matching.
	hits, err := r.Catalog.SearchScored(strings.TrimSpace(req.Title+" "+req.Message), 1)
	if err != nil || len(hits) == 0 || hits[0].Score < r.MinScore {
		return nil, 0
	}
	e := hits[0].Entry
	return &e, hits[0].Score
}

// recalledInvestigation builds findings directly from a catalog entry. It is
// explicit that this is a recalled match, not a fresh investigation.
func recalledInvestigation(req Request, e catalog.Entry) providers.Investigation {
	rc := providers.Hypothesis{
		Summary:    e.Title + " — " + e.Description,
		Confidence: 0.8,
		Evidence:   []string{fmt.Sprintf("instant recall: matched knowledge-base entry %q", e.Path)},
	}
	return providers.Investigation{
		Title:      req.Title,
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{rc},
		Unresolved: []string{"recalled from the catalog without a fresh investigation — confirm it still applies"},
	}
}
