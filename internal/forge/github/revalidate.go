// SPDX-License-Identifier: Apache-2.0

package github

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// Sentinels the revalidation stamp returns instead of an edit. Both mean "no PR
// is warranted here", and the curate pass treats them as done-skips (like
// ErrAlreadyRetired) rather than failures.
var (
	// ErrRecentlyValidated signals the entry's recorded freshness is already
	// within minGap of the candidate date, so restamping it would be noise. This
	// is the anti-spam rule, and it is deliberately evaluated against the FILE on
	// the base branch rather than against anything the pass remembers: the file
	// is where the answer actually lives, so a merged revalidation immediately
	// stops the next sweep from re-proposing, with no store to keep in sync.
	ErrRecentlyValidated = errors.New("entry validated too recently to restamp")
	// ErrEntryInactive signals the entry is retired or draft. Recall never fires
	// such an entry, so it can never earn a confirmation, and proposing to stamp
	// one "still valid" would directly contradict the retirement pass.
	ErrEntryInactive = errors.New("entry is retired or draft")
)

// lastValidatedLayout is the grammar the stamp is written in: a bare UTC date.
// `last_validated` means "the day a human last confirmed this entry works" (see
// okf.Meta), so day granularity IS the field's resolution — and a bare date
// keeps the one-line diff a reviewer reads legible, needs no YAML quoting, and
// parses back through catalog.ParseEntryDate, the single date grammar recall's
// age gate and kbvalidate share.
const lastValidatedLayout = "2006-01-02"

// revalidateLabels mark a PR proposed by the curate revalidation pass — "runlore"
// for the shared forge namespace, "runlore-revalidate" for the pass's idempotency
// and human-veto listings.
var revalidateLabels = []string{"runlore", "runlore-revalidate"}

// setLastValidated stamps `last_validated: <date>` into an OKF entry's YAML
// frontmatter, editing ONLY that line (inserting it when absent) — human
// formatting, key order and comments are preserved, because the entry is a
// human-authored artifact under review and a re-marshal would produce an
// unreadable confirmation diff. Scanning is fence-bounded, so a "last_validated:"
// string in the markdown body is never touched.
//
// It returns ErrRecentlyValidated when the entry's freshness on record is already
// within minGap of at, and ErrEntryInactive for a retired/draft entry. A file
// without a frontmatter block errors: the stamp must never write blind.
func setLastValidated(content []byte, at time.Time, minGap time.Duration) ([]byte, error) {
	lines, rest, err := frontmatterBlock(content)
	if err != nil {
		return nil, err
	}
	stamp := "last_validated: " + at.UTC().Format(lastValidatedLayout)
	validatedAt := -1             // index of the existing last_validated line, -1 when absent
	var validated, stamped string // the two recorded dates, raw scalars
	for i, ln := range lines {
		key, val, ok := strings.Cut(ln, ":")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "status":
			// The same two inactive states recall filters on (investigate's
			// entryActive); an absent or foreign status stays active, per OKF §9
			// tolerance, so pre-status catalogs are revalidated like any other.
			if s := strings.ToLower(strings.TrimSpace(val)); s == "retired" || s == "draft" {
				return nil, ErrEntryInactive
			}
		case "last_validated":
			validatedAt, validated = i, val
		case "timestamp":
			stamped = val
		}
	}
	// Freshness on record is last_validated, else timestamp — recall's own age-gate
	// fallback, so what this pass refreshes is exactly what that gate reads. An
	// absent or unparseable date means nothing is on record: there is a fact to
	// establish, so stamp it (which also repairs the unparseable value).
	recorded := stamped
	if validatedAt >= 0 {
		recorded = validated
	}
	if t, ok := catalog.ParseEntryDate(unquoteScalar(recorded)); ok && at.Sub(t) < minGap {
		// Also the never-write-backwards guard: a date in the FUTURE (a human
		// stamped it by hand) yields a negative gap and is left alone.
		return nil, ErrRecentlyValidated
	}
	if validatedAt >= 0 {
		lines[validatedAt] = stamp
		return []byte("---\n" + strings.Join(lines, "\n") + rest), nil
	}
	return []byte("---\n" + stamp + "\n" + strings.Join(lines, "\n") + rest), nil
}

// unquoteScalar strips the surrounding quotes yaml.Marshal adds to a date-shaped
// scalar (okf.Render emits `last_validated: "2026-08-03T10:00:00Z"`), so the
// hand-parsed value matches what a YAML reader would see.
func unquoteScalar(s string) string {
	s = strings.TrimSpace(s)
	if len(s) >= 2 && (s[0] == '"' || s[0] == '\'') && s[len(s)-1] == s[0] {
		return s[1 : len(s)-1]
	}
	return s
}

// OpenRevalidatePR opens a human-reviewed PR that stamps `last_validated:
// <validated>` into an existing catalog entry's frontmatter — the confirmation
// half of the freshness loop. It never merges: `last_validated` claims HUMAN
// confirmation, and merging this PR is precisely that act, which is why RunLore
// can propose the date but must not write it. body carries the reviewer-facing
// evidence and the hidden idempotency marker (authored by the caller). Returns
// ErrRecentlyValidated / ErrEntryInactive when no PR is warranted (no PR opened).
func (c *Client) OpenRevalidatePR(ctx context.Context, entryPath string, validated time.Time, minGap time.Duration, body string) (providers.Ref, error) {
	return c.openEntryEditPR(ctx, entryEdit{
		path:         entryPath,
		stamp:        func(raw []byte) ([]byte, error) { return setLastValidated(raw, validated, minGap) },
		branchPrefix: "revalidate",
		commitVerb:   "revalidate",
		titlePrefix:  "KB revalidate: ",
		labels:       revalidateLabels,
		body:         body,
	})
}
