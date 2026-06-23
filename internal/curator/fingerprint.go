package curator

import (
	"context"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// Fingerprint builds the dedup query string for a finding: the alert/title, the
// top root-cause signature, and the affected workload. It is a BM25 query (fuzzy),
// not a hash — matched against the catalog index and open-PR titles.
func Fingerprint(inv providers.Investigation) string {
	var b strings.Builder
	b.WriteString(inv.Title)
	if len(inv.RootCauses) > 0 {
		b.WriteString(" " + inv.RootCauses[0].Summary)
	}
	if len(inv.Changes) > 0 {
		w := inv.Changes[0].Workload
		b.WriteString(" " + w.Namespace + " " + w.Name)
	}
	return strings.TrimSpace(b.String())
}

// Novelty decides whether a finding duplicates an existing catalog entry, by
// scoring its fingerprint against the BM25 catalog index.
type Novelty struct {
	Catalog  catalog.ScoredSearcher // nil → everything is novel (no catalog configured)
	DupScore float64                // top-hit BM25 score ≥ this ⇒ duplicate
}

// TopHit returns the highest-scoring catalog entry for a finding's fingerprint.
// ok is false when no catalog is configured or there are no hits. It surfaces the
// score regardless of any threshold, so callers can both observe it and decide.
func (n Novelty) TopHit(ctx context.Context, inv providers.Investigation) (catalog.ScoredEntry, bool, error) { //nolint:revive // ctx kept for future remote-index symmetry
	if n.Catalog == nil {
		return catalog.ScoredEntry{}, false, nil
	}
	hits, err := n.Catalog.SearchScored(Fingerprint(inv), 1)
	if err != nil {
		return catalog.ScoredEntry{}, false, err
	}
	if len(hits) == 0 {
		return catalog.ScoredEntry{}, false, nil
	}
	return hits[0], true, nil
}

// IsDuplicate returns true and the matching entry when the top hit's score is
// ≥ DupScore. Returns false (novel) when no catalog is configured, there are no
// hits, or the top score falls below the threshold.
func (n Novelty) IsDuplicate(ctx context.Context, inv providers.Investigation) (bool, catalog.Entry, error) {
	top, ok, err := n.TopHit(ctx, inv)
	if err != nil || !ok {
		return false, catalog.Entry{}, err
	}
	if top.Score >= n.DupScore {
		return true, top.Entry, nil
	}
	return false, catalog.Entry{}, nil
}
