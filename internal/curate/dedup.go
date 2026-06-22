package curate

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
)

// Dedup collapses near-identical open KB PRs: the lowest-numbered PR in a cluster
// is canonical; the rest are closed with a back-ref comment. Conservative by
// design (high similarity threshold) — a missed merge is cheaper than a wrong close.
type Dedup struct {
	Forge     Forge
	Threshold float64 // Jaccard over title token-sets; default 0.6 when 0
	Log       *slog.Logger
}

// Run clusters open PRs by title similarity and closes duplicates.
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
			if jaccard(titleTokens(prs[i].Title), titleTokens(prs[j].Title)) >= thr {
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
