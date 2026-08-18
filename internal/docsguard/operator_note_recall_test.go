// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/kbvalidate"
	"github.com/Smana/runlore/internal/thread"
)

// operatorNotePage is the page whose §10 operator-note paragraph these guards pin.
const operatorNotePage = "../../website/content/docs/concepts/learning-loop.md"

// filedOperatorNote runs the REAL filer — thread.ConceptEntry, the function the
// standalone note route calls — over a realistic thread context, and returns the
// entry it produces. Everything asserted below is read out of that entry rather than
// restated, which is what package docsguard promises: a guard reflects over a runtime
// source of truth, not over a hand-copied fact.
func filedOperatorNote(t *testing.T) (description, body string) {
	t.Helper()
	tc := thread.Context{
		Transport:  "slack",
		Root:       "1723900000.000100",
		Channel:    "C0DOCSGUARD",
		Title:      "KubePodNotReady: harbor-registry",
		Resource:   "tooling/harbor-registry",
		TriggerKey: "KubePodNotReady|tooling/harbor-registry",
	}
	e := thread.ConceptEntry(
		tc,
		thread.HumanNote("alice", "this recurs after every spot-node reclaim, not a CNI fault"),
		time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC),
		0,
	)
	return e.Description, e.Body
}

// noteDescriptionRE is the shape conceptDescription actually renders for a
// human-written note. The page QUOTES this sentence as the thing the adversarial
// reviewer is asked to accept as a causal chain, so if the filer stops saying it — or
// starts saying something that reads as evidence — the quotation is stale and the
// paragraph's whole argument has to be re-derived.
var noteDescriptionRE = regexp.MustCompile(`^Operator knowledge from @\S+ via \w+(, on the finding ".*")?(: .+|\.)$`)

// TestOperatorNoteFilesNoCausalEvidence derives the two facts learning-loop.md §10
// rests on, from the code that decides them:
//
//  1. the body a note files carries NO Symptom/Cause/Resolution — checked with
//     kbvalidate.HasIncidentSections, the very predicate the merge gate uses, so a
//     future NoteBody that starts rendering those sections fails here rather than
//     silently making the page wrong;
//  2. the one-line description it files says where the note came from, in the exact
//     words the page quotes.
//
// Both are content properties of the filer, and that is the point: the page must NOT
// claim verify is structurally barred from admitting a note. Nothing on the verify
// path reads an entry's type — see TestRecalledEntryShowsTheReviewerNoBody in
// internal/investigate — so the only true statement is that a note as filed today
// carries nothing admissible.
func TestOperatorNoteFilesNoCausalEvidence(t *testing.T) {
	description, body := filedOperatorNote(t)

	if kbvalidate.HasIncidentSections(body) {
		t.Errorf("a filed operator note now carries the full Symptom/Cause/Resolution chain:\n%s\n"+
			"learning-loop.md §10 says it carries no causal evidence — re-measure the verify behaviour "+
			"and rewrite the paragraph before shipping this", body)
	}
	if !noteDescriptionRE.MatchString(description) {
		t.Errorf("the filed description is %q, which no longer matches %v — learning-loop.md §10 quotes "+
			"this sentence as what the reviewer is shown; update both together", description, noteDescriptionRE)
	}

	// The page quotes the same sentence, elided the way prose elides it (and wrapped,
	// so the line break is part of the match). Anchored on both ends of the real
	// sentence, so a rewritten description leaves a quotation that no longer exists.
	if doc := readDoc(t, operatorNotePage); !strings.Contains(doc, "Operator knowledge from @… via …, on the finding \"…\": …") {
		t.Errorf("learning-loop.md §10 no longer quotes the description an operator note actually files (%q) — "+
			"without it the paragraph asserts a rejection reason the reader cannot check", description)
	}
}

// TestOperatorNoteRecallCountIsTheMeasuredOne pins the one part of the paragraph that
// is neither derivable from code nor checkable by a reader: the live-cluster
// measurement behind #504. Four recalls, four rejections, at recall confidence
// 0.82 / 0.85 / 0.90 / 0.92 — the issue's own evidence. An earlier draft of the page
// said six, and attributed one of them to a hand-written entry that was never an
// operator note at all.
//
// THIS GUARD CANNOT FAIL FROM THE CODE SIDE, and that is stated here rather than left
// to be discovered: nothing in this repo reproduces those four rejections, and a
// change to verify would not trip it. It is a consistency check between the page and a
// recorded observation — enough to stop the count drifting again silently, and not
// enough for a green run to mean the number is still true.
func TestOperatorNoteRecallCountIsTheMeasuredOne(t *testing.T) {
	doc := readDoc(t, operatorNotePage)

	measured := regexp.MustCompile(`\*\*(\w+) recalls of operator notes across two\s+incidents, at recall confidence 0\.82–0\.92, and (\w+) rejections\*\*`)
	m := measured.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("learning-loop.md §10 no longer states the #504 measurement in the pinned form (%v) — "+
			"if the sentence was reworded, reword this guard with it", measured)
	}
	if m[1] != "four" || m[2] != "four" {
		t.Errorf("learning-loop.md §10 says %s recalls / %s rejections; issue #504 measured four and four "+
			"(0.82, 0.85, 0.90, 0.92) — re-measure before changing the number", m[1], m[2])
	}

	// Four-out-of-four is an observation, not a rule. Without these the page could keep
	// the right number and still assert the impossibility claim the code does not make.
	for _, want := range []string{
		"not a rule in the code",
		"Nothing on the\n  verify path reads an entry's `type`",
	} {
		if !strings.Contains(doc, want) {
			t.Errorf("learning-loop.md §10 must keep the caveat containing %q — otherwise the measurement "+
				"reads as a structural guarantee that the code does not make", want)
		}
	}
}
