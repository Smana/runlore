// SPDX-License-Identifier: Apache-2.0

package kbresolve

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// resolveLabel marks the entry-edit PRs Resolve opens for a merged entry.
const resolveLabel = "runlore-resolve"

// ErrClosedUnmerged means the target PR was closed without merging: its entry
// never reached the knowledge base, so there is nothing to record a fix on.
var ErrClosedUnmerged = errors.New("the pull request was closed without merging")

// Forge is what Resolve asks of the knowledge-base forge.
type Forge interface {
	PREntry(ctx context.Context, number int) (providers.PREntry, error)
	EditEntryOnPR(ctx context.Context, number int, message string, edit func([]byte) ([]byte, bool, error)) (bool, error)
	ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	OpenResolvePR(ctx context.Context, path string, edit func([]byte) ([]byte, bool, error), body string) (providers.Ref, error)
}

// Target names the entry to record on: the PR that carries it, or its path in the
// knowledge base. PR wins when both are set.
type Target struct {
	PR   int
	Path string
}

// Result says how a resolution was written: "appended" (a commit on the open PR),
// "edit-pr" (an entry-edit PR on a merged entry, at URL), or "unchanged" (it was
// already recorded, or its edit PR is already open).
type Result struct{ Route, URL string }

// resolveSum is the idempotency key of an entry-edit PR: one per entry, fix and
// verdict.
func resolveSum(path string, r providers.AppliedResolution) string {
	sum := sha256.Sum256([]byte(path + "|" + r.FixPR + "|" + r.Verdict))
	return hex.EncodeToString(sum[:])[:8]
}

// Resolve records r on the entry t names. On an open PR it adds a commit to the
// PR's own branch. On a merged entry it opens an entry-edit PR, once: the PR's
// body carries a marker that a later call finds through the runlore-resolve
// label, and an edit that merged since is a no-op because Apply changes nothing.
// A PR closed without merging is ErrClosedUnmerged. The same call is what the
// deferred path (phase 2b) makes, so both write one way.
func Resolve(ctx context.Context, f Forge, t Target, r providers.AppliedResolution) (Result, error) {
	if err := Validate(r); err != nil {
		return Result{}, err
	}
	edit := func(raw []byte) ([]byte, bool, error) { return Apply(raw, r) }
	path := t.Path
	if t.PR != 0 {
		pe, err := f.PREntry(ctx, t.PR)
		if err != nil {
			return Result{}, fmt.Errorf("PR #%d: %w", t.PR, err)
		}
		switch pe.State {
		case "open":
			changed, err := f.EditEntryOnPR(ctx, t.PR, "runlore: record resolution on "+pe.Path, edit)
			switch {
			case err != nil:
				return Result{}, err
			case changed:
				return Result{Route: "appended"}, nil
			}
			return Result{Route: "unchanged"}, nil
		case "merged":
			path = pe.Path
		default:
			return Result{}, fmt.Errorf("PR #%d: %w", t.PR, ErrClosedUnmerged)
		}
	}
	if path == "" {
		return Result{}, errors.New("a target names a pull request or an entry path")
	}

	marker := "<!-- runlore:resolve:" + resolveSum(path, r) + " -->"
	open, err := f.ListPRsByLabel(ctx, resolveLabel)
	if err != nil {
		return Result{}, err
	}
	for _, pr := range open {
		if strings.Contains(pr.Body, marker) {
			return Result{Route: "unchanged"}, nil
		}
	}
	body := fmt.Sprintf("RunLore records that the fix %s %s on `%s`.\n\n%s", r.FixPR, verdictWords(r.Verdict), path, marker)
	ref, err := f.OpenResolvePR(ctx, path, edit, body)
	switch {
	case errors.Is(err, providers.ErrAlreadyResolved):
		return Result{Route: "unchanged"}, nil
	case err != nil:
		return Result{}, err
	}
	return Result{Route: "edit-pr", URL: ref.URL}, nil
}

func verdictWords(verdict string) string {
	if verdict == "persisting" {
		return "did not hold"
	}
	return "resolved the incident"
}
