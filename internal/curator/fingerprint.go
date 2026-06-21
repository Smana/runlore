// Package curator writes completed investigations back to the git forge.
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

// IsDuplicate returns true + the matching entry when the catalog already covers
// this finding.
func (n Novelty) IsDuplicate(ctx context.Context, inv providers.Investigation) (bool, catalog.Entry, error) {
	if n.Catalog == nil {
		return false, catalog.Entry{}, nil
	}
	hits, err := n.Catalog.SearchScored(Fingerprint(inv), 1)
	if err != nil {
		return false, catalog.Entry{}, err
	}
	if len(hits) > 0 && hits[0].Score >= n.DupScore {
		return true, hits[0].Entry, nil
	}
	return false, catalog.Entry{}, nil
}
