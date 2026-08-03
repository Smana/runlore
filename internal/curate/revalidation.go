// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// revalidateLabel is the forge label the revalidation pass lists on for
// idempotency and human-veto detection — the same label OpenRevalidatePR stamps
// on the PRs it opens.
const revalidateLabel = "runlore-revalidate"

// DefaultMaxOpenRevalidations is the fallback reviewer-queue bound when none is
// configured. config.ApplyDefaults ships the same value; this guard only protects
// direct users of the package — and it matters more here than a missing interval
// would, because an unset bound computes a budget of zero and would silently make
// the whole pass a no-op.
const DefaultMaxOpenRevalidations = 5

// RevalidateForge is the forge surface the Revalidation pass needs (consumer-side,
// like RetireForge — widening the shared Forge would bloat every other pass's fake).
type RevalidateForge interface {
	ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	ListClosedUnmergedPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	OpenRevalidatePR(ctx context.Context, entryPath string, validated time.Time, minGap time.Duration, body string) (providers.Ref, error)
}

// Revalidation is the mirror image of Retirement: where retirement proposes
// letting go of an entry whose track record decayed, this proposes CONFIRMING one
// whose track record held — opening a human-reviewed PR that stamps
// `last_validated` with the date a recall of that entry was followed by the
// incident actually clearing.
//
// It exists because `last_validated` could previously only decay. The forge
// leaves it unset when it drafts an entry (renderEntry: "that field claims human
// confirmation", and a fresh draft has none), and nothing ever wrote it
// afterwards — so with catalog.instant_recall.stale_after configured, every
// entry's delivered confidence stepped down permanently from its creation date
// and could only be restored by hand-editing YAML. This pass is how the field is
// earned instead.
//
// The evidence bar is ONE resolved recall, and the pass is deliberately not
// apologetic about that. A resolved recall is not a bare coincidence: the entry
// won recall's structural and margin gates, was confirmed against LIVE cluster
// state, survived the adversarial verify pass, was delivered as the answer, and
// the incident then cleared. That is a far denser chain of checks than
// retirement's evidence (an alert simply failing to clear), which is why
// retirement needs MinObservations to distinguish signal from noise and this pass
// does not. The asymmetry is also one of consequence: retirement REMOVES
// knowledge, while this only proposes a date — and the field claims HUMAN
// confirmation, which merging the PR is precisely the act of giving. RunLore's
// job here is only to bring honest evidence to a reviewer; the human remains the
// thing that makes the claim true.
//
// Interaction with Retirement: the two are DISJOINT BY CONSTRUCTION, not by a
// cross-pass check that could drift. Retirement fires strictly below the trust
// floor (Factor < Floor), this one only at or above it, both using
// outcome.Aggregate.Factor with the same prior/floor recall's Gate 3 uses. So an
// entry can never be proposed for both in one sweep, and where they meet
// RETIREMENT WINS: an entry recall already refuses to fire has not earned a
// "still valid" stamp, whatever a stale resolve in its history says.
//
// Like the other passes it never merges, never writes directly, is idempotent via
// a hidden per-entry marker in the PR body, treats a closed-unmerged PR carrying
// that marker as a permanent human veto, and isolates per-item forge failures.
// Opt-in (see config): it opens PRs against the operator's repo.
type Revalidation struct {
	Forge RevalidateForge
	Stats EntryStats
	// MinInterval is the anti-spam bar: the candidate date must be at least this
	// much newer than the entry's recorded freshness (last_validated, else
	// timestamp — recall's own age-gate fallback) or no PR is opened. Evaluated
	// against the file on the base branch by the forge, so a merged revalidation
	// silences the next sweep with no state to keep.
	MinInterval time.Duration
	// MaxOpen bounds how many revalidation PRs may be waiting on a human at once.
	// It is a reviewer-queue bound, not a per-run one: outstanding PRs from earlier
	// sweeps count against it, so the queue drains as it is reviewed instead of
	// growing every interval.
	MaxOpen int
	Floor   float64 // revalidate only at/above this factor — the retirement boundary
	Prior   float64 // k — must equal recall's outcome_prior so all three gates agree
	Log     *slog.Logger
}

// Run proposes one revalidation PR per confirmed, still-trusted candidate.
// Per-item forge failures are logged and skipped so one flaky entry never starves
// the rest.
func (p Revalidation) Run(ctx context.Context) error {
	counts, err := p.Stats.OpenCounts()
	if err != nil {
		return fmt.Errorf("revalidation: open counts: %w", err)
	}
	// Candidate = an entry with a resolved recall on record (LastConfirmed is the
	// date of the newest one) that recall still trusts. Sorted for deterministic
	// logs, tests, and — with MaxOpen — a stable queue that advances as PRs are
	// merged or vetoed rather than shuffling every run.
	var candidates []string
	for path, agg := range counts {
		if agg.Resolved == 0 || agg.LastConfirmed.IsZero() {
			continue // no resolved recall: nothing was confirmed
		}
		if agg.Factor(p.Prior) < p.Floor {
			continue // a retirement candidate; see the disjointness note above
		}
		candidates = append(candidates, path)
	}
	if len(candidates) == 0 {
		return nil // nothing confirmed: zero forge calls
	}
	slices.Sort(candidates)

	// One listing of each revalidate-PR set per run (the Contested per-run-cache
	// pattern): open PRs give idempotency AND the queue depth, closed-unmerged PRs
	// give the human veto.
	openPRs, err := p.Forge.ListPRsByLabel(ctx, revalidateLabel)
	if err != nil {
		return fmt.Errorf("revalidation: list open revalidate PRs: %w", err)
	}
	closedPRs, err := p.Forge.ListClosedUnmergedPRsByLabel(ctx, revalidateLabel)
	if err != nil {
		// Fail the run rather than propose blind: without the veto listing every
		// declined entry would be re-nagged.
		return fmt.Errorf("revalidation: list closed-unmerged revalidate PRs: %w", err)
	}
	openBodies := bodiesOf(openPRs)
	vetoedBodies := bodiesOf(closedPRs)

	maxOpen := p.MaxOpen
	if maxOpen <= 0 {
		maxOpen = DefaultMaxOpenRevalidations
	}
	budget := maxOpen - len(openPRs)
	if budget <= 0 {
		p.Log.Info("revalidation: review queue full; proposing nothing this sweep",
			"open", len(openPRs), "max_open", maxOpen, "candidates", len(candidates))
		return nil
	}

	for _, path := range candidates {
		if budget <= 0 {
			p.Log.Info("revalidation: open-PR budget spent; remaining candidates wait for the next sweep",
				"max_open", maxOpen)
			return nil
		}
		agg := counts[path]
		marker := revalidateMarker(path)
		if anyContains(openBodies, marker) {
			continue // already proposed on an open PR — the marker is the idempotency record
		}
		if anyContains(vetoedBodies, marker) {
			// A human closed a revalidation PR for this entry without merging: a
			// deliberate "I am not confirming this". Never re-propose (the
			// ClosedPRSuppression philosophy). The marker is keyed on the entry
			// path alone, NOT on the date, precisely so the veto is permanent — a
			// date-keyed marker would turn every veto into a monthly re-ask.
			p.Log.Debug("revalidation: entry has a human-vetoed revalidate PR; skipping", "entry", path)
			continue
		}
		factor := agg.Factor(p.Prior)
		body := revalidateBody(path, agg, factor, marker)
		if _, err := p.Forge.OpenRevalidatePR(ctx, path, agg.LastConfirmed, p.MinInterval, body); err != nil {
			// Both sentinels mean "no PR was warranted", so neither is a failure and
			// neither spends the review-queue budget.
			switch {
			case errors.Is(err, github.ErrRecentlyValidated):
				p.Log.Debug("revalidation: entry validated recently enough; skipping", "entry", path)
			case errors.Is(err, github.ErrEntryInactive):
				p.Log.Debug("revalidation: entry is retired or draft; skipping", "entry", path)
			default:
				p.Log.Warn("revalidation: open revalidate PR failed", "entry", path, "err", err)
			}
			continue // per-item isolation: one flaky entry never starves the rest
		}
		budget--
		p.Log.Info("revalidation: proposed confirming a still-working entry", "entry", path,
			"validated", agg.LastConfirmed, "factor", factor, "recalls", agg.Recalls, "resolved", agg.Resolved)
	}
	return nil
}

// revalidateBody renders the reviewer-facing confirmation proposal. It states the
// evidence, is explicit that MERGING is the human confirmation the field claims
// (RunLore cannot supply it), and spells out the veto path (close to decline).
// The invisible marker goes last, like retireBody/contestedComment.
func revalidateBody(path string, agg outcome.Aggregate, factor float64, marker string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**RunLore proposes stamping `last_validated: %s` on `%s`** — it was recalled for a live incident, and that incident then resolved.\n\n",
		agg.LastConfirmed.UTC().Format(time.DateOnly), path)
	fmt.Fprintf(&b, "| recalls | resolved | 👍 | 👎 | factor | last resolved |\n|---|---|---|---|---|---|\n| %d | %d | %d | %d | %.2f | %s |\n\n",
		agg.Recalls, agg.Resolved, agg.FeedbackUp, agg.FeedbackDown, factor, agg.LastConfirmed.UTC().Format(time.DateOnly))
	b.WriteString("`last_validated` claims a HUMAN confirmed the entry still works, and RunLore cannot give that — merging this PR is the confirmation. ")
	b.WriteString("It refreshes the entry against `catalog.instant_recall.stale_after`, so a note that keeps working stops looking older than it is (nothing else changes: no status, no content). ")
	b.WriteString("Close this PR to decline: RunLore will not propose it again.\n\n")
	b.WriteString(marker)
	return b.String()
}

// revalidateMarker is the hidden idempotency/veto marker embedded in a revalidate
// PR body: one per entry path. See entryMarker for why the path is hashed.
func revalidateMarker(entryPath string) string { return entryMarker("revalidate", entryPath) }
