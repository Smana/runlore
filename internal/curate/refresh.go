// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

const (
	// BotLogin is the only commit author a rebuild accepts. An adopter whose App has another name
	// sees every conflict in Decide, the safe side (KP19).
	BotLogin = "runlore[bot]"
	// entryBranchPrefix names the branches OpenPR creates, the only ones a rebuild touches.
	entryBranchPrefix = "runlore/kb-"
	// maxRebuildsPerRun bounds one pass: a rebuild is six writes and a CI run.
	maxRebuildsPerRun = 10
)

// RefreshForge is what rebuilding conflicting entry PRs needs. *github.Client satisfies it, and so
// does Guard.
type RefreshForge interface {
	ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	ReplaceLabel(ctx context.Context, number int, remove, add string) error
	EntryPRState(ctx context.Context, number int) (providers.EntryPRState, error)
	RebuildEntryPR(ctx context.Context, number int, head string) error
}

// RefreshResult is what one pass did: the PRs rebuilt, and the ones a human must look at, with why.
type RefreshResult struct {
	Rebuilt []int
	Refused map[int]string
	Stopped bool          // the forge answered 403 or 429: the rest was not looked at
	Failed  map[int]error // PRs whose rebuild failed for another reason, retried next pass
}

// RefreshEntries rebuilds a conflicting entry PR on the current base, when every commit on it is
// RunLore's own. The first merge conflicts the whole backlog, and a rebuild is six writes, a CI run
// and a fresh updated_at, which the stale sweep reads. So a pass rebuilds only what can use it: a PR
// labelled ready-to-merge or with a held latest observation, at most maxRebuildsPerRun of them, and
// it stops on a 403 or 429. Any other conflicting PR is listed for Decide, and nothing is written to
// it. A human names one with Only, and the same safety checks apply.
type RefreshEntries struct {
	Forge RefreshForge
	// Evidence is what makes an unlabelled PR worth rebuilding: a held latest observation. Nil: only a
	// PR labelled ready-to-merge is.
	Evidence Evidence
	// Only, when set, makes Run rebuild this one PR, a human's request, whatever its label or evidence.
	Only int
	// Promote puts ready-to-merge back on a rebuilt PR on the same evidence; nil when the caller's
	// Queue lists right after.
	Promote func(ctx context.Context, number int) error
	Log     *slog.Logger
}

// Run is a whole pass, or the one PR Only names. Unlike Once it fails when the work did not get done,
// for an operator running it between merges.
func (r RefreshEntries) Run(ctx context.Context) error {
	if r.Only != 0 {
		return r.one(ctx)
	}
	res, err := r.Once(ctx)
	if err != nil {
		return err
	}
	r.Log.Info("refresh-entries: done", "rebuilt", res.Rebuilt, "refused", slices.Sorted(maps.Keys(res.Refused)))
	if res.Stopped {
		return fmt.Errorf("refresh-entries: stopped after rebuilding %v: %w", res.Rebuilt, providers.ErrRateLimited)
	}
	// Some failures among successes are retried next pass; a pass that did nothing but fail did not do its work.
	if len(res.Rebuilt) == 0 && len(res.Failed) > 0 {
		failed := slices.Sorted(maps.Keys(res.Failed))
		return fmt.Errorf("refresh-entries: every attempted rebuild failed, PRs %v; first: PR %d: %w", failed, failed[0], res.Failed[failed[0]])
	}
	return nil
}

// Once runs one pass. One PR's failure is logged and retried next pass, so it never starves the
// rest, unless the forge answered 403 or 429; only a failed listing is returned.
func (r RefreshEntries) Once(ctx context.Context) (RefreshResult, error) {
	res := RefreshResult{Refused: map[int]string{}, Failed: map[int]error{}}
	prs, err := r.Forge.ListPRsByLabel(ctx, runloreLabel)
	if err != nil {
		return res, err
	}
	var held map[string]bool
	if r.Evidence != nil {
		held = r.Evidence.Verifications()
	}
	// The label goes first: past the cap, what a human already sees as ready should not be what waits.
	prs = slices.Clone(prs)
	slices.SortStableFunc(prs, func(a, b providers.CuratedIssue) int {
		return cmp.Compare(labelRank(a), labelRank(b))
	})
	budget := maxRebuildsPerRun
	for _, pr := range prs {
		if isEntryEditProposal(pr.Labels) {
			continue
		}
		err := r.consider(ctx, pr, held, &budget, &res)
		if err == nil {
			continue
		}
		r.Log.Warn("refresh-entries: failed; retried next pass", "pr", pr.Number, "err", err)
		if errors.Is(err, providers.ErrRateLimited) {
			r.Log.Warn("refresh-entries: stopping the pass; the forge answered 403 or 429", "pr", pr.Number, "rebuilt", res.Rebuilt)
			res.Stopped = true
			return res, nil
		}
		res.Failed[pr.Number] = err
	}
	return res, nil
}

func labelRank(pr providers.CuratedIssue) int {
	if slices.Contains(pr.Labels, readyLabel) {
		return 0
	}
	return 1
}

// consider is one PR of a whole pass: it decides whether the PR is worth a rebuild.
func (r RefreshEntries) consider(ctx context.Context, pr providers.CuratedIssue, held map[string]bool, budget *int, res *RefreshResult) error {
	st, err := r.Forge.EntryPRState(ctx, pr.Number)
	if err != nil {
		return err
	}
	if st.Mergeable == nil || *st.Mergeable {
		return nil
	}
	switch {
	case !strings.HasPrefix(st.Branch, entryBranchPrefix):
		r.refuse(pr.Number, res, "not an entry branch")
		return nil
	case !slices.Contains(pr.Labels, readyLabel) && !held[providers.ParseFingerprintMarker(pr.Body)]:
		r.refuse(pr.Number, res, "no label and no held evidence; refresh-entries --pr rebuilds it")
		return nil
	}
	if rebuildable(st) {
		// Listed for now, with no write: a label stays, since an accepted PR has no evidence to put it
		// back, and the next pass comes back to it.
		if *budget == 0 {
			r.refuse(pr.Number, res, "past this pass's cap; the next pass comes back to it")
			return nil
		}
		*budget--
	}
	return r.refresh(ctx, pr, st, res)
}

// one is what `lore curate refresh-entries --pr <n>` runs. A human asked, so neither the label nor the
// evidence nor the cap applies, and every other check does. It fails when the PR was not rebuilt and
// was not simply fine as it was.
func (r RefreshEntries) one(ctx context.Context) error {
	prs, err := r.Forge.ListPRsByLabel(ctx, runloreLabel)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(prs, func(pr providers.CuratedIssue) bool { return pr.Number == r.Only })
	if i < 0 {
		return fmt.Errorf("refresh-entries: PR %d is not an open PR labelled %s", r.Only, runloreLabel)
	}
	pr := prs[i]
	if isEntryEditProposal(pr.Labels) {
		return fmt.Errorf("refresh-entries: PR %d is an entry edit, which stays a human's merge", r.Only)
	}
	st, err := r.Forge.EntryPRState(ctx, pr.Number)
	if err != nil {
		return err
	}
	switch {
	case st.Mergeable == nil:
		return fmt.Errorf("refresh-entries: GitHub is still computing whether PR %d conflicts; try again", r.Only)
	case *st.Mergeable:
		r.Log.Info("refresh-entries: not conflicting, nothing to do", "pr", r.Only)
		return nil
	case !strings.HasPrefix(st.Branch, entryBranchPrefix):
		return fmt.Errorf("refresh-entries: PR %d refused: branch %q is not an entry branch", r.Only, st.Branch)
	}
	res := RefreshResult{Refused: map[int]string{}}
	if err := r.refresh(ctx, pr, st, &res); err != nil {
		return fmt.Errorf("refresh-entries: PR %d: %w", r.Only, err)
	}
	if _, refused := res.Refused[r.Only]; refused {
		return fmt.Errorf("refresh-entries: PR %d refused: it is not all RunLore's own, or the base already has its entry; it stays in Decide as %s", r.Only, RiskConflict)
	}
	return nil
}

// refresh strips the label, then refuses or rebuilds a conflicting PR on an entry branch. A failure
// is returned for the caller to log or report; a refusal is not a failure.
func (r RefreshEntries) refresh(ctx context.Context, pr providers.CuratedIssue, st providers.EntryPRState, res *RefreshResult) error {
	// Before the refusal too: removing a label touches no branch, and a labelled PR is missing from
	// Decide. Before the push: a label on the old head must not stand on the new one (KP20).
	if slices.Contains(pr.Labels, readyLabel) {
		if err := r.Forge.ReplaceLabel(ctx, pr.Number, readyLabel, ""); err != nil {
			return fmt.Errorf("removing ready-to-merge, so no rebuild: %w", err)
		}
	}
	if !rebuildable(st) {
		r.refuse(pr.Number, res, "not all of it is RunLore's own, or not only an entry")
		return nil
	}
	switch err := r.Forge.RebuildEntryPR(ctx, pr.Number, st.HeadSHA); {
	case errors.Is(err, providers.ErrEntryOnBase):
		r.refuse(pr.Number, res, "the base already has its entry")
		return nil
	case errors.Is(err, providers.ErrNotRunLorePR):
		r.refuse(pr.Number, res, "its branch name is not one RunLore makes")
		return nil
	case err != nil:
		return fmt.Errorf("rebuild: %w", err)
	}
	r.Log.Info("refresh-entries: rebuilt on the current base", "pr", pr.Number)
	res.Rebuilt = append(res.Rebuilt, pr.Number)
	if r.Promote != nil {
		if err := r.Promote(ctx, pr.Number); err != nil {
			return fmt.Errorf("rebuilt, but promoting again failed, which the next Queue pass retries: %w", err)
		}
	}
	return nil
}

func (r RefreshEntries) refuse(number int, res *RefreshResult, why string) {
	r.Log.Info("refresh-entries: refused, left for a human", "pr", number, "why", why)
	res.Refused[number] = RiskConflict.String()
}

// rebuildable reports whether a rebuild would lose nothing: every commit is the bot's (the forge
// reports no login for a commit it did not write and sign itself), on a head in this repository,
// and the whole change is one entry and the two files a rebuild writes again.
func rebuildable(st providers.EntryPRState) bool {
	if st.Fork || st.Truncated || len(st.CommitAuthors) == 0 {
		return false
	}
	if slices.ContainsFunc(st.CommitAuthors, func(a string) bool { return a != BotLogin }) {
		return false
	}
	entries := slices.DeleteFunc(slices.Clone(st.Files), func(p string) bool { return p == "index.md" || p == "log.md" })
	return len(entries) == 1 && strings.HasSuffix(entries[0], ".md")
}

// refreshFirst runs a pass when the caller has one, and returns the PRs it refused.
func refreshFirst(ctx context.Context, r *RefreshEntries) (map[int]string, error) {
	if r == nil {
		return nil, nil
	}
	res, err := r.Once(ctx)
	return res.Refused, err
}
