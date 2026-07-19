// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// retireLabel is the forge label the retirement pass lists on for idempotency and
// human-veto detection — the same label OpenRetirePR stamps on the PRs it opens.
const retireLabel = "runlore-retire"

// RetireForge is the forge surface the Retirement pass needs (consumer-side, like
// ContestedForge — widening the shared Forge would bloat every other pass's fake).
type RetireForge interface {
	ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	ListClosedUnmergedPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	OpenRetirePR(ctx context.Context, entryPath, body string) (providers.Ref, error)
}

// RetireStats is the ledger view the pass needs: the per-entry outcome roll-up.
type RetireStats interface {
	OpenCounts() (map[string]outcome.Aggregate, error)
}

// Retirement opens a human-reviewed "retire" PR for a merged catalog entry whose
// outcome track record has decayed below the trust floor and stayed there across
// at least MinObservations observations — closing the garbage-collection half of
// the learning loop. It never merges and never deletes: a human is the load-bearing
// gate, and the PR only stamps `status: retired` frontmatter (the entry stays in
// git history). Ledger-driven and idempotent like the other passes — a hidden
// per-entry marker in the PR body is the "already proposed" record, and a
// closed-unmerged retire PR carrying the marker is a human veto that is never
// re-nagged. Opt-in (see config): retirement is a judgment call an operator enables.
type Retirement struct {
	Forge           RetireForge
	Stats           RetireStats
	MinObservations int     // sustained-decay bar: total observations before retirement is considered
	Floor           float64 // retire when Factor(Prior) < Floor
	Prior           float64 // k — must equal recall's outcome_prior so both gates agree
	Log             *slog.Logger
}

// Run proposes one retire PR per sustained-decay candidate. Per-item forge failures
// are logged and skipped so one flaky entry never starves the rest.
func (p Retirement) Run(ctx context.Context) error {
	counts, err := p.Stats.OpenCounts()
	if err != nil {
		return fmt.Errorf("retirement: open counts: %w", err)
	}
	// Candidate = an entry whose decay is BOTH deep (Factor < Floor) and sustained
	// (>= MinObservations observations) — a single bad recall must never retire an
	// entry. Sorted for deterministic logs and tests.
	var candidates []string
	for path, agg := range counts {
		obs := agg.Recalls + agg.FeedbackUp + agg.FeedbackDown
		if obs < p.MinObservations {
			continue
		}
		if agg.Factor(p.Prior) >= p.Floor {
			continue
		}
		candidates = append(candidates, path)
	}
	if len(candidates) == 0 {
		return nil // nothing decayed enough: zero forge calls
	}
	slices.Sort(candidates)

	// One listing of each retire-PR set per run (the Contested per-run-cache
	// pattern): open PRs give idempotency, closed-unmerged PRs give the human veto.
	openPRs, err := p.Forge.ListPRsByLabel(ctx, retireLabel)
	if err != nil {
		return fmt.Errorf("retirement: list open retire PRs: %w", err)
	}
	closedPRs, err := p.Forge.ListClosedUnmergedPRsByLabel(ctx, retireLabel)
	if err != nil {
		return fmt.Errorf("retirement: list closed-unmerged retire PRs: %w", err)
	}
	openBodies := bodiesOf(openPRs)
	vetoedBodies := bodiesOf(closedPRs)

	for _, path := range candidates {
		agg := counts[path]
		marker := retireMarker(path)
		if anyContains(openBodies, marker) {
			continue // already proposed on an open PR — the marker is the idempotency record
		}
		if anyContains(vetoedBodies, marker) {
			// A human closed a retire PR for this entry without merging: a deliberate
			// "keep it". Never re-propose (the ClosedPRSuppression philosophy).
			p.Log.Debug("retirement: entry has a human-vetoed retire PR; skipping", "entry", path)
			continue
		}
		factor := agg.Factor(p.Prior)
		if _, err := p.Forge.OpenRetirePR(ctx, path, retireBody(path, agg, factor, p.Floor, marker)); err != nil {
			if errors.Is(err, github.ErrAlreadyRetired) {
				p.Log.Debug("retirement: entry already retired on base branch; skipping", "entry", path)
				continue
			}
			p.Log.Warn("retirement: open retire PR failed", "entry", path, "err", err)
			continue // per-item isolation: one flaky entry never starves the rest
		}
		p.Log.Info("retirement: proposed retiring decayed entry", "entry", path, "factor", factor,
			"recalls", agg.Recalls, "resolved", agg.Resolved, "up", agg.FeedbackUp, "down", agg.FeedbackDown)
	}
	return nil
}

// retireBody renders the reviewer-facing retirement proposal. It is honest about
// the recall side effect (the entry is already rejected at the same floor) and
// about the veto path (close to keep). The invisible marker (last, like
// contestedComment) makes re-runs idempotent and encodes the human veto.
func retireBody(path string, agg outcome.Aggregate, factor, floor float64, marker string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**RunLore proposes retiring `%s`** — its outcome track record decayed below the trust floor.\n\n", path)
	fmt.Fprintf(&b, "| recalls | resolved | 👍 | 👎 | factor | floor |\n|---|---|---|---|---|---|\n| %d | %d | %d | %d | %.2f | %.2f |\n\n",
		agg.Recalls, agg.Resolved, agg.FeedbackUp, agg.FeedbackDown, factor, floor)
	b.WriteString("Recall already rejects this entry at the same floor, so every recurrence pays a full investigation; ")
	b.WriteString("merging this PR retires the entry (`status: retired` — it stops being recallable but stays in git history). ")
	b.WriteString("Close this PR to keep the entry: RunLore will not propose it again.\n\n")
	b.WriteString(marker)
	return b.String()
}

// retireMarker is the hidden idempotency/veto marker embedded in a retire PR body:
// one per entry path. The path is hashed rather than embedded verbatim so a path
// with "--"/">" sequences can never break out of the HTML comment.
func retireMarker(entryPath string) string {
	sum := sha256.Sum256([]byte(entryPath))
	return fmt.Sprintf("<!-- runlore:retire:%x -->", sum[:8])
}

// bodiesOf projects the PR bodies for marker scanning.
func bodiesOf(prs []providers.CuratedIssue) []string {
	bodies := make([]string, 0, len(prs))
	for _, pr := range prs {
		bodies = append(bodies, pr.Body)
	}
	return bodies
}
