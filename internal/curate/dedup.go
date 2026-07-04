package curate

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// Dedup collapses near-identical open KB PRs: the lowest-numbered PR in a cluster
// is canonical; the rest are closed with a back-ref comment. Conservative by
// design (high similarity threshold) — a missed merge is cheaper than a wrong close.
type Dedup struct {
	Forge     Forge
	Threshold float64 // title-Jaccard fallback threshold for markerless PRs; default 0.6 when 0
	Log       *slog.Logger
}

// Run clusters open PRs and closes duplicates — fingerprint-first (deterministic
// DupFingerprint marker), falling back to title-Jaccard for markerless PRs.
func (d Dedup) Run(ctx context.Context) error {
	thr := d.Threshold
	if thr == 0 {
		thr = 0.6
	}
	prs, err := d.Forge.ListPRsByLabel(ctx, "runlore")
	if err != nil {
		return fmt.Errorf("list PRs: %w", err)
	}
	sort.Slice(prs, func(i, j int) bool { return prs[i].Number < prs[j].Number })
	canonicalOf := map[int]int{} // dup PR number -> canonical PR number
	for i := range prs {
		if _, isDup := canonicalOf[prs[i].Number]; isDup {
			continue
		}
		for j := i + 1; j < len(prs); j++ {
			if _, taken := canonicalOf[prs[j].Number]; taken {
				continue
			}
			// Never auto-close a human-touched artifact (solved/ready-to-merge/
			// accepted/investigating/knowledge-gap) as a duplicate.
			if isProtected(prs[j].Labels) {
				continue
			}
			if isDuplicatePair(prs[i], prs[j], thr) {
				canonicalOf[prs[j].Number] = prs[i].Number
			}
		}
	}
	for dup, canon := range canonicalOf {
		if err := d.Forge.Comment(ctx, dup, fmt.Sprintf("Duplicate of #%d — closed by RunLore curate. Reopen if these are genuinely distinct.", canon)); err != nil {
			d.Log.Warn("dedup: comment failed", "pr", dup, "err", err)
			continue
		}
		if err := d.Forge.Close(ctx, dup); err != nil {
			d.Log.Warn("dedup: close failed", "pr", dup, "err", err)
			continue
		}
		d.Log.Info("dedup: closed duplicate", "pr", dup, "canonical", canon)
	}
	return nil
}

// isDuplicatePair decides whether b duplicates the canonical a. It is
// fingerprint-FIRST: every curator-drafted PR carries a deterministic
// DupFingerprint persisted in its body as a marker (the same identity the
// file-time gate's Curator.duplicateOpenPR reads back). When BOTH PRs carry a
// parseable marker and the fingerprints are EQUAL, that is the strong signal —
// duplicates regardless of title phrasing, exactly the title fragility
// DupFingerprint was built to retire.
//
// EQUAL fingerprints are conclusive; DIFFERENT fingerprints are NOT. The same
// incident yields two different fingerprints when investigated via an alert
// (trigger-key identity) versus a manual `lore investigate` (cause-token identity),
// so a fingerprint mismatch cannot be trusted to mean "distinct". Rather than
// short-circuit to false — which let those cross-path duplicates escape dedup and
// merge as permanent catalog dupes — we fall through to title-Jaccard, the same
// check the markerless path uses. The tradeoff: two genuinely distinct incidents on
// the same resource with similar titles can now be collapsed by Jaccard where the
// fingerprint mismatch used to shield them; that shield was also hiding true
// duplicates, so catching them is the intended net.
//
// Title-Jaccard is also the FALLBACK for markerless PRs (legacy/hand-filed entries
// with no marker).
func isDuplicatePair(a, b providers.CuratedIssue, thr float64) bool {
	fa := providers.ParseFingerprintMarker(a.Body)
	fb := providers.ParseFingerprintMarker(b.Body)
	if fa != "" && fb != "" && fa == fb {
		return true
	}
	return jaccard(titleTokens(a.Title), titleTokens(b.Title)) >= thr
}

var titleNoise = map[string]bool{"kb": true, "in": true, "the": true, "due": true, "to": true, "a": true, "of": true, "and": true}

func titleTokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, w := range strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return (r < 'a' || r > 'z') && (r < '0' || r > '9')
	}) {
		if !titleNoise[w] {
			out[w] = true
		}
	}
	return out
}

func jaccard(a, b map[string]bool) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	inter := 0
	for k := range a {
		if b[k] {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	return float64(inter) / float64(union)
}
