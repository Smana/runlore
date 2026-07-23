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
// other, preserving input order. It only decides — the caller reports/writes.
// Skip reasons are checked in order: retired-at-source, destination already an
// existing entry, fuzzy title duplicate, then batch path collision.
func Plan(results []Result, existing []catalog.Entry) []Action {
	existingPaths := map[string]bool{}
	for _, e := range existing {
		existingPaths[e.Path] = true
	}
	batchPaths := map[string]string{} // dest path -> the batch source that first claimed it
	out := make([]Action, 0, len(results))
	for _, r := range results {
		a := Action{Result: r}
		switch {
		case strings.EqualFold(strings.TrimSpace(r.Meta.Status), "retired"):
			a.Skip, a.Reason = true, "source marked retired"
		case existingPaths[r.DestPath]:
			a.Skip, a.Reason = true, fmt.Sprintf("destination exists: %s", r.DestPath)
		default:
			if dup, ok := duplicateOf(r.Entry.Title, existing); ok {
				a.Skip, a.Reason = true, "duplicate of "+dup
			} else if occ, taken := batchPaths[r.DestPath]; taken {
				a.Skip, a.Reason = true, fmt.Sprintf("destination %s collides with %s in this batch", r.DestPath, occ)
			} else {
				batchPaths[r.DestPath] = r.Source
			}
		}
		out = append(out, a)
	}
	return out
}

// duplicateOf finds an existing entry whose title matches: exact after
// whitespace/case normalization, or fuzzy at curate's own Jaccard threshold.
func duplicateOf(title string, existing []catalog.Entry) (string, bool) {
	norm := strings.ToLower(strings.Join(strings.Fields(title), " "))
	for _, e := range existing {
		if strings.ToLower(strings.Join(strings.Fields(e.Title), " ")) == norm {
			return e.Path, true
		}
		if curate.TitleJaccard(title, e.Title) >= dupThreshold {
			return e.Path, true
		}
	}
	return "", false
}
