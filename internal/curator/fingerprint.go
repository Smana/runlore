package curator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"unicode"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// stopWords are content-free English filler dropped so reworded causes hash alike.
var stopWords = map[string]struct{}{
	"the": {}, "a": {}, "an": {}, "and": {}, "or": {}, "but": {}, "in": {}, "on": {}, "at": {}, "to": {}, "for": {}, "of": {}, "by": {}, "with": {}, "is": {}, "are": {}, "was": {}, "were": {}, "be": {}, "been": {}, "being": {}, "have": {}, "has": {}, "had": {}, "do": {}, "does": {}, "did": {}, "will": {}, "would": {}, "could": {}, "should": {}, "may": {}, "might": {}, "must": {}, "can": {}, "that": {}, "which": {}, "who": {}, "when": {}, "where": {}, "why": {}, "how": {}, "as": {}, "from": {}, "up": {}, "down": {}, "out": {}, "so": {}, "if": {}, "any": {}, "all": {}, "each": {}, "every": {}, "both": {}, "few": {}, "more": {}, "most": {}, "other": {}, "same": {}, "such": {}, "no": {}, "nor": {}, "not": {}, "only": {}, "than": {}, "too": {}, "very": {}, "just": {}, "what": {}, "then": {}, "you": {}, "your": {}, "his": {}, "her": {}, "its": {}, "our": {}, "their": {}, "this": {}, "these": {}, "happened": {}, "happen": {},
}

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

// DupFingerprint is a deterministic identity for "the same problem on the same
// resource": the affected-resource ref plus the sorted significant-token set of the
// top root cause, hashed. Unlike Fingerprint (a fuzzy BM25 query), it is stable
// across the LLM's prose phrasing, so two investigations of one incident hash
// alike. It returns "" when there is neither a resource nor a cause to key on — an
// empty fingerprint must never match another.
func DupFingerprint(inv providers.Investigation) string {
	ref := strings.ToLower(inv.Resource.Ref())
	cause := ""
	if len(inv.RootCauses) > 0 {
		cause = inv.RootCauses[0].Summary
	}
	tokens := tokenSet(cause)
	if ref == "" && len(tokens) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(ref + "|" + strings.Join(tokens, " ")))
	return hex.EncodeToString(sum[:])
}

// tokenSet lowercases s, splits on non-alphanumeric runes, drops generic English
// stopwords and tokens shorter than 3 chars, dedupes, and sorts — an order-independent
// significant-token set so reworded phrasings of one cause normalize to the same key.
func tokenSet(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := make(map[string]struct{}, len(fields))
	var out []string
	for _, f := range fields {
		if len(f) < 3 {
			continue
		}
		if _, ok := stopWords[f]; ok {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
