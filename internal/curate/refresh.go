// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"errors"
	"log/slog"
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
}

// RefreshEntries rebuilds on the current base each open entry PR GitHub reports as conflicting,
// when every commit on it is RunLore's own. Anything else conflicting is refused, for Decide to list.
type RefreshEntries struct {
	Forge RefreshForge
	// Promote puts ready-to-merge back on a rebuilt PR on the same evidence; nil when the caller's
	// Queue lists right after.
	Promote func(ctx context.Context, number int) error
	Log     *slog.Logger
}

// Run is Once for a caller with no use for the result.
func (r RefreshEntries) Run(ctx context.Context) error {
	res, err := r.Once(ctx)
	if err != nil {
		return err
	}
	r.Log.Info("refresh-entries: done", "rebuilt", len(res.Rebuilt), "refused", len(res.Refused))
	return nil
}

// Once runs one pass. One PR's failure is logged and retried next pass, so it never starves the
// rest; only a failed listing is returned.
func (r RefreshEntries) Once(ctx context.Context) (RefreshResult, error) {
	res := RefreshResult{Refused: map[int]string{}}
	prs, err := r.Forge.ListPRsByLabel(ctx, runloreLabel)
	if err != nil {
		return res, err
	}
	for _, pr := range prs {
		if isEntryEditProposal(pr.Labels) {
			continue
		}
		r.refresh(ctx, pr, &res)
	}
	return res, nil
}

func (r RefreshEntries) refresh(ctx context.Context, pr providers.CuratedIssue, res *RefreshResult) {
	st, err := r.Forge.EntryPRState(ctx, pr.Number)
	if err != nil {
		r.Log.Warn("refresh-entries: reading the PR failed", "pr", pr.Number, "err", err)
		return
	}
	if !strings.HasPrefix(st.Branch, entryBranchPrefix) || st.Mergeable == nil || *st.Mergeable {
		return
	}
	// Before the refusal too: removing a label touches no branch, and a labelled PR is missing from
	// Decide. Before the push: a label on the old head must not stand on the new one (KP20).
	if slices.Contains(pr.Labels, readyLabel) {
		if err := r.Forge.ReplaceLabel(ctx, pr.Number, readyLabel, ""); err != nil {
			r.Log.Warn("refresh-entries: removing ready-to-merge failed; no rebuild this pass", "pr", pr.Number, "err", err)
			return
		}
	}
	if !rebuildable(st) {
		r.refuse(pr.Number, res, "not all of it is RunLore's own, or not only an entry")
		return
	}
	switch err := r.Forge.RebuildEntryPR(ctx, pr.Number, st.HeadSHA); {
	case errors.Is(err, providers.ErrEntryOnBase):
		r.refuse(pr.Number, res, "the base already has its entry")
		return
	case err != nil:
		r.Log.Warn("refresh-entries: rebuild failed; retried next pass", "pr", pr.Number, "err", err)
		return
	}
	r.Log.Info("refresh-entries: rebuilt on the current base", "pr", pr.Number)
	res.Rebuilt = append(res.Rebuilt, pr.Number)
	if r.Promote != nil {
		if err := r.Promote(ctx, pr.Number); err != nil {
			r.Log.Warn("refresh-entries: promoting again failed; the next Queue pass retries", "pr", pr.Number, "err", err)
		}
	}
}

func (r RefreshEntries) refuse(number int, res *RefreshResult, why string) {
	r.Log.Info("refresh-entries: refused, left for a human", "pr", number, "why", why)
	res.Refused[number] = RiskConflict.String()
}

// rebuildable reports whether a rebuild would lose nothing: every commit is the bot's, on a head in
// this repository, and the whole change is one entry and the two files a rebuild writes again.
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
