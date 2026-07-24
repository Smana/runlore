// SPDX-License-Identifier: Apache-2.0

package kbimport

import (
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/curate"
)

// Action is Plan's verdict on one inferred result.
type Action struct {
	Result
	Skip   bool
	Reason string // set when Skip
}

// dupThreshold matches curate.Dedup's default title-Jaccard threshold: a
// duplicate at import time is the same notion as a duplicate at curation
// time. Conservative both ways — a missed dup lands in review as a visible
// extra file; a wrong skip loses knowledge silently, so we require ≥0.6.
const dupThreshold = 0.6

// Plan dedups inferred results against the existing catalog and against each
// other, preserving input order (first occurrence wins). It only decides — the
// caller reports/writes. Skip reasons are checked in order: retired-at-source,
// destination already an existing entry, fuzzy title duplicate of an existing
// entry, batch path collision, then fuzzy title duplicate of an entry already
// accepted earlier in this same batch.
func Plan(results []Result, existing []catalog.Entry) []Action {
	existingPaths := map[string]bool{}
	for _, e := range existing {
		existingPaths[e.Path] = true
	}
	existingNorm := normEntries(existing) // normalize existing titles ONCE, not per result
	batchPaths := map[string]string{}     // dest path -> the batch source that first claimed it
	var accepted []normEntry              // entries accepted so far this batch, for intra-batch title dedup
	out := make([]Action, 0, len(results))
	for _, r := range results {
		a := Action{Result: r}
		switch {
		case strings.EqualFold(strings.TrimSpace(r.Meta.Status), "retired"):
			a.Skip, a.Reason = true, "source marked retired"
		case existingPaths[r.DestPath]:
			a.Skip, a.Reason = true, fmt.Sprintf("destination exists: %s", r.DestPath)
		default:
			if dup, ok := duplicateOf(r.Entry.Title, existingNorm); ok {
				a.Skip, a.Reason = true, "duplicate of "+dup
			} else if occ, taken := batchPaths[r.DestPath]; taken {
				a.Skip, a.Reason = true, fmt.Sprintf("destination %s collides with %s in this batch", r.DestPath, occ)
			} else if dup, ok := duplicateOf(r.Entry.Title, accepted); ok {
				a.Skip, a.Reason = true, fmt.Sprintf("duplicate of %s (imported earlier in this batch)", dup)
			} else {
				batchPaths[r.DestPath] = r.Source
				accepted = append(accepted, normEntry{title: r.Entry.Title, norm: normTitle(r.Entry.Title), path: r.DestPath})
			}
		}
		out = append(out, a)
	}
	return out
}

// normEntry is a catalog entry with its title pre-normalized for exact-match
// dedup, so Plan normalizes each title once instead of once per compared result.
type normEntry struct {
	title string // original, for the fuzzy TitleJaccard comparison
	norm  string // lowercased, whitespace-collapsed, for exact match
	path  string
}

func normTitle(s string) string { return strings.ToLower(strings.Join(strings.Fields(s), " ")) }

func normEntries(es []catalog.Entry) []normEntry {
	out := make([]normEntry, len(es))
	for i, e := range es {
		out[i] = normEntry{title: e.Title, norm: normTitle(e.Title), path: e.Path}
	}
	return out
}

// duplicateOf finds an entry whose title matches: exact after whitespace/case
// normalization, or fuzzy at curate's own Jaccard threshold.
func duplicateOf(title string, entries []normEntry) (string, bool) {
	norm := normTitle(title)
	for _, e := range entries {
		if e.norm == norm {
			return e.path, true
		}
		if curate.TitleJaccard(title, e.title) >= dupThreshold {
			return e.path, true
		}
	}
	return "", false
}
