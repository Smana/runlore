// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// ResolutionChecker reports whether the incident behind a curated PR has cleared
// (alert resolved / resource healthy). Implemented read-only over the cluster.
type ResolutionChecker interface {
	IsResolved(ctx context.Context, pr providers.CuratedIssue) (bool, error)
}

const (
	readyLabel    = "ready-to-merge"
	solvedLabel   = "solved"
	acceptedLabel = "accepted"
	runloreLabel  = "runlore"
)

// Queue applies the merge condition. With Evidence nil, a quality-passing (solved)
// PR becomes ready-to-merge when the incident is resolved, OR when a human has
// labelled it accepted. Unresolved + unaccepted PRs wait (surfaced, never
// auto-queued).
//
// With Evidence set, an open entry PR also becomes ready-to-merge when its fix's
// latest observation held, and the label is what the merge gate trusts: a risk other
// than unverified, or a latest observation that failed, keeps it off solved+resolved
// PRs too and withdraws it from a labelled PR. accepted stays a human override.
// Entry edits (isEntryEditProposal) stay a human merge and are never candidates.
type Queue struct {
	Forge    Forge
	Checker  ResolutionChecker // nil: nothing is promoted on resolution alone (Evidence set only)
	Evidence Evidence
	KBRepo   string // forge.kb_repo ("owner/name"), which contested KB links must name; Evidence set only
	Log      *slog.Logger
}

// Run gates the open PRs onto the decision-ready queue. One PR's failure is logged and
// skipped so it never starves the rest.
func (q Queue) Run(ctx context.Context) error {
	if q.Evidence == nil {
		return q.runOnResolution(ctx)
	}
	contested, err := q.contestedPRs()
	if err != nil {
		return err
	}
	verified := q.Evidence.Verifications()
	seen := map[int]bool{}
	for _, label := range []string{solvedLabel, runloreLabel} {
		prs, err := q.Forge.ListPRsByLabel(ctx, label)
		if err != nil {
			return err
		}
		for _, pr := range prs {
			if seen[pr.Number] {
				continue
			}
			seen[pr.Number] = true
			if err := q.applyEvidence(ctx, pr, contested, verified); err != nil {
				q.Log.Warn("queue: evidence check failed", "pr", pr.Number, "err", err)
			}
		}
	}
	return nil
}

// PromoteOne runs Run's evidence check on one open runlore PR, so the label lands from
// a verification rather than waiting for a sweep (KP18). A PR that is not open and
// labelled runlore has nothing to check. Unlike Run it returns a failure, for the caller
// to retry. With Evidence nil it does nothing.
func (q Queue) PromoteOne(ctx context.Context, number int) error {
	if q.Evidence == nil {
		return nil
	}
	contested, err := q.contestedPRs()
	if err != nil {
		return err
	}
	prs, err := q.Forge.ListPRsByLabel(ctx, runloreLabel)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(prs, func(pr providers.CuratedIssue) bool { return pr.Number == number })
	if i < 0 {
		return nil
	}
	return q.applyEvidence(ctx, prs[i], contested, q.Evidence.Verifications())
}

// contestedPRs is the set of kb_repo PRs a standing 👎 links to. A malformed kb_repo is an
// error rather than an empty set: a contest that cannot be joined must not read as none.
func (q Queue) contestedPRs() (map[int]bool, error) { return contestedPRs(q.Evidence, q.KBRepo) }

func contestedPRs(ev Evidence, kbRepo string) (map[int]bool, error) {
	triggers := ev.ContestedTriggers()
	if len(triggers) == 0 {
		return nil, nil
	}
	owner, repo, ok := strings.Cut(kbRepo, "/")
	if !ok {
		return nil, fmt.Errorf("queue: kb_repo %q must be owner/name", kbRepo)
	}
	contested := map[int]bool{}
	for _, ct := range triggers {
		if n, ok := prNumberInRepo(ct.CuratedURL, owner, repo); ok {
			contested[n] = true
		}
	}
	return contested, nil
}

// applyEvidence labels pr, withdraws its label, or leaves it, from the evidence.
func (q Queue) applyEvidence(ctx context.Context, pr providers.CuratedIssue, contested map[int]bool, verified map[string]bool) error {
	if isEntryEditProposal(pr.Labels) {
		return nil
	}
	flagged := slices.ContainsFunc(Risks(pr, contested, verified), func(r Risk) bool { return r != RiskUnverified })
	held, observed := verified[providers.ParseFingerprintMarker(pr.Body)]
	failed := observed && !held
	accepted := slices.Contains(pr.Labels, acceptedLabel)
	solved := slices.Contains(pr.Labels, solvedLabel)
	switch {
	case slices.Contains(pr.Labels, readyLabel):
		if accepted || !flagged && !failed {
			return nil
		}
		return q.withdraw(ctx, pr.Number)
	case solved && accepted:
		return q.enqueue(ctx, pr.Number, "human-accepted")
	case flagged || failed:
		return nil
	case held:
		return q.enqueue(ctx, pr.Number, "verified")
	case solved && q.Checker != nil:
		resolved, err := q.Checker.IsResolved(ctx, pr)
		if err != nil || !resolved {
			return err
		}
		return q.enqueue(ctx, pr.Number, "resolved")
	}
	return nil
}

func (q Queue) enqueue(ctx context.Context, number int, reason string) error {
	if err := q.Forge.ReplaceLabel(ctx, number, "", readyLabel); err != nil {
		return err
	}
	q.Log.Info("queued ready-to-merge", "pr", number, "reason", reason)
	return nil
}

func (q Queue) withdraw(ctx context.Context, number int) error {
	if err := q.Forge.ReplaceLabel(ctx, number, readyLabel, ""); err != nil {
		return err
	}
	q.Log.Info("withdrew ready-to-merge", "pr", number, "reason", "evidence-withdrawn")
	return nil
}

// runOnResolution gates each solved PR on its resolution or a human's accepted label alone.
func (q Queue) runOnResolution(ctx context.Context) error {
	prs, err := q.Forge.ListPRsByLabel(ctx, solvedLabel)
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if slices.Contains(pr.Labels, readyLabel) {
			continue // already queued
		}
		accepted := slices.Contains(pr.Labels, acceptedLabel)
		resolved := false
		if !accepted {
			resolved, err = q.Checker.IsResolved(ctx, pr)
			if err != nil {
				q.Log.Warn("resolution check failed", "pr", pr.Number, "err", err)
				continue
			}
		}
		if accepted || resolved {
			if err := q.enqueue(ctx, pr.Number, queueReason(accepted)); err != nil {
				q.Log.Warn("queue: relabel failed", "pr", pr.Number, "err", err)
			}
		}
	}
	return nil
}

func queueReason(accepted bool) string {
	if accepted {
		return "human-accepted"
	}
	return "resolved"
}

// LedgerResolutionChecker reports a curated PR's incident has resolved when the
// outcome ledger holds a matching resolved episode. The join is keyed on the
// deterministic dedup fingerprint (resource+cause) carried in the PR body and stamped
// on every ledger open — stable across the LLM's prose, so a reworded re-investigation
// of one incident still resolves the matching PR. A PR filed before the fingerprint
// was wired carries no marker; it falls back to a whitespace-normalized title join.
// Source-agnostic: it reads the ledger's episodes, never a trigger-specific API.
type LedgerResolutionChecker struct {
	Ledger interface {
		Episodes() ([]outcome.Episode, error)
	}
}

// IsResolved implements ResolutionChecker.
func (c LedgerResolutionChecker) IsResolved(_ context.Context, pr providers.CuratedIssue) (bool, error) {
	wantFP := providers.ParseFingerprintMarker(pr.Body) // "" when the PR carries no marker
	title := strings.TrimSpace(strings.TrimPrefix(pr.Title, "KB: "))
	if wantFP == "" && title == "" {
		return false, nil
	}
	eps, err := c.Ledger.Episodes()
	if err != nil {
		return false, err
	}
	for _, e := range eps {
		if !e.Resolved {
			continue
		}
		// Primary: the stable dedup-fingerprint join. An episode with an empty
		// fingerprint never equals a non-empty wantFP, so empty never false-matches.
		if wantFP != "" {
			if e.DupFingerprint == wantFP {
				return true, nil
			}
			// A marker is present: stay fingerprint-only. Falling through to the title
			// here would resurrect the very free-text fragility this join removes.
			continue
		}
		// Legacy fallback (no marker): whitespace-robust title equality on both sides.
		if title != "" && strings.TrimSpace(e.Title) == title {
			return true, nil
		}
	}
	return false, nil
}
