// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"

	"github.com/Smana/runlore/internal/outcome"
)

// ContestedForge is the forge surface the Contested pass needs. Kept separate
// from Forge: comment listing and PR-state lookup exist for this pass alone, and
// widening the shared interface would force every other pass's test fake to grow
// two irrelevant methods.
type ContestedForge interface {
	Comment(ctx context.Context, number int, body string) error
	ListIssueCommentBodies(ctx context.Context, number int) ([]string, error)
	IsPROpen(ctx context.Context, number int) (bool, error)
}

// ContestedSource yields the triggers whose delivered conclusion has standing 👎
// votes and a KB link (the outcome ledger's ContestedTriggers view).
type ContestedSource interface {
	ContestedTriggers() []outcome.ContestedTrigger
}

// Contested surfaces standing 👎 votes on the OPEN KB PR they relate to. The gap
// it closes: a 👎 on a FRESH investigation weighs nothing in recall trust (there
// is no catalog entry yet to decay), so without this pass the one human signal
// that says "this diagnosis is contested" never reaches the person about to make
// it permanent knowledge — the KB PR reviewer. Ledger-driven and idempotent like
// the other passes: votes are recomputed from the ledger every run, and a hidden
// per-trigger marker in the posted comment is the "already surfaced" record — no
// mutable store. It only ever comments; labels, closes and merges stay with the
// other passes and with humans.
type Contested struct {
	Forge  ContestedForge
	Ledger ContestedSource
	KBRepo string // configured forge.kb_repo ("owner/name"); KB links outside it are foreign and skipped
	Log    *slog.Logger
}

// Run posts one warning comment per (open KB PR, contested trigger). Per-item
// failures are logged and skipped so one flaky PR never starves the rest.
func (p Contested) Run(ctx context.Context) error {
	triggers := p.Ledger.ContestedTriggers()
	if len(triggers) == 0 {
		return nil // disabled ledger or nothing contested: zero forge calls
	}
	owner, repo, ok := strings.Cut(p.KBRepo, "/")
	if !ok {
		return fmt.Errorf("contested: kb_repo %q must be owner/name", p.KBRepo)
	}
	// Per-PR caches: several contested triggers can coalesce onto one PR, and the
	// state/comment lookups are per-PR facts — fetch each once per run.
	openState := map[int]bool{}
	commentBodies := map[int][]string{}
	for _, ct := range triggers {
		num, ok := prNumberInRepo(ct.CuratedURL, owner, repo)
		if !ok {
			// Foreign or non-PR link (another repo, a plain issue, a malformed URL):
			// nothing here is reviewable in kb_repo, so skip — visibly, not silently.
			p.Log.Debug("contested: KB link is not a kb_repo pull request; skipping", "trigger", ct.TriggerKey, "url", ct.CuratedURL)
			continue
		}
		open, cached := openState[num]
		if !cached {
			var err error
			if open, err = p.Forge.IsPROpen(ctx, num); err != nil {
				p.Log.Warn("contested: PR state check failed", "pr", num, "err", err)
				continue
			}
			openState[num] = open
		}
		if !open {
			// Merged or closed: review is over. The votes keep weighing recall trust
			// (and the recurrence cooldown) — there is just no reviewer left to warn.
			p.Log.Debug("contested: KB PR is not open; nothing to warn on", "trigger", ct.TriggerKey, "pr", num)
			continue
		}
		marker := contestedMarker(ct.TriggerKey)
		bodies, cached := commentBodies[num]
		if !cached {
			var err error
			if bodies, err = p.Forge.ListIssueCommentBodies(ctx, num); err != nil {
				p.Log.Warn("contested: list PR comments failed", "pr", num, "err", err)
				continue
			}
			commentBodies[num] = bodies
		}
		if anyContains(bodies, marker) {
			continue // already surfaced on this PR — the marker is the idempotency record
		}
		if err := p.Forge.Comment(ctx, num, contestedComment(ct, marker)); err != nil {
			p.Log.Warn("contested: comment failed", "pr", num, "err", err)
			continue
		}
		p.Log.Info("contested: surfaced standing downvotes on open KB PR", "pr", num, "trigger", ct.TriggerKey, "downs", ct.Downs)
	}
	return nil
}

// contestedComment renders the reviewer-facing warning. It must be actionable
// (what to re-check before merging) and honest about the side effect a standing
// 👎 already has: it re-arms re-investigation, so a fresher conclusion may
// supersede this entry. The invisible marker makes re-runs idempotent.
func contestedComment(ct outcome.ContestedTrigger, marker string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**On-call feedback: %d standing 👎 %s** on the investigation behind this entry (trigger `%s`).\n\n",
		ct.Downs, pluralVote(ct.Downs), ct.TriggerKey)
	b.WriteString("Responders contested the delivered diagnosis. Please review the Cause against the evidence before merging — merging makes it permanent, recallable knowledge.\n\n")
	b.WriteString("Note: a standing 👎 also re-arms re-investigation (the recurrence cooldown no longer suppresses this trigger), so a fresher conclusion may follow on the next occurrence.\n\n")
	if ct.Confirms > 0 {
		fmt.Fprintf(&b, "Since the contest, %d fresh re-investigation%s independently reached this same conclusion (recorded as recovery evidence in the outcome ledger).\n\n",
			ct.Confirms, pluralS(ct.Confirms))
	}
	b.WriteString(marker)
	return b.String()
}

func pluralVote(n int) string {
	if n == 1 {
		return "vote"
	}
	return "votes"
}

func pluralS(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// contestedMarker is the hidden idempotency marker embedded in the posted
// comment: one per trigger key, so a second contested trigger coalesced onto the
// same PR still gets its own warning. The key is hashed rather than embedded
// verbatim — trigger keys are free-form source identifiers and could otherwise
// break out of the HTML comment ("--"/">" sequences).
func contestedMarker(triggerKey string) string {
	sum := sha256.Sum256([]byte(triggerKey))
	return fmt.Sprintf("<!-- runlore:contested:%x -->", sum[:8])
}

// prNumberInRepo parses rawURL as a web link to a pull request of owner/repo,
// returning (0, false) for anything else — a plain issue, a foreign repo, or a
// malformed URL. The match is on the URL path (/{owner}/{repo}/pull/{n}), not
// the host: the pass only knows the API base, and the web host differs from it
// on both github.com and GHES, while the owner/repo path identifies the repo on
// both.
func prNumberInRepo(rawURL, owner, repo string) (int, bool) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return 0, false
	}
	segs := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(segs) != 4 || segs[0] != owner || segs[1] != repo || segs[2] != "pull" {
		return 0, false
	}
	n, err := strconv.Atoi(segs[3])
	if err != nil || n <= 0 {
		return 0, false
	}
	return n, true
}

// anyContains reports whether any body carries the marker.
func anyContains(bodies []string, marker string) bool {
	for _, b := range bodies {
		if strings.Contains(b, marker) {
			return true
		}
	}
	return false
}
