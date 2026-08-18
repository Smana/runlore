// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// recalledOperatorNote is the catalog entry a merged operator note becomes: the
// title and one-line description thread.ConceptEntry files (pinned separately in
// internal/docsguard), and a body whose every interesting string is a sentinel, so
// "did this reach the reviewer?" is answerable exactly rather than by eyeballing.
func recalledOperatorNote() catalog.Entry {
	return catalog.Entry{
		Type:        "Concept",
		Path:        "kb/operator-note-harbor-registry.md",
		Title:       "Operator note: KubePodNotReady: harbor-registry",
		Description: "Operator knowledge captured from a slack thread by @alice.",
		Body: "## Note\n\nSENTINEL-NOTE-TEXT\n\n" +
			"## Symptom\n\nSENTINEL-SYMPTOM\n\n" +
			"## Cause\n\nSENTINEL-CAUSE\n\n" +
			"## Resolution\n\nSENTINEL-RESOLUTION\n",
	}
}

func recalledOperatorNoteRequest() Request {
	return Request{
		Title:    "KubePodNotReady: harbor-registry",
		Reason:   "ContainerCreating for 15m",
		Workload: providers.Workload{Kind: "Deployment", Namespace: "tooling", Name: "harbor-registry"},
	}
}

// TestRecalledEntryShowsTheReviewerNoBody pins the mechanism learning-loop.md §10
// asserts, against the two functions that decide it.
//
// The page used to claim a `Concept` entry and the verify pass were "structurally
// incompatible" — that an `Incident`'s Symptom/Cause/Resolution could satisfy verify
// where a note's could not. The code does not implement that, and this test is the
// proof kept next to it: renderForReview builds the reviewer's whole view from the
// hypothesis SUMMARY (on the recall path, entry title + description), its evidence
// bullets and the confirm transcript. The entry body never appears — not the note's
// text, and not an Incident's evidence sections either, which is why recalledInvestigation
// filling inv.Prior with Cause/Resolution changes nothing about what is reviewed.
//
// Break either half — render Prior into the review, or stop rendering the description
// — and §10's argument is no longer the one the code makes.
func TestRecalledEntryShowsTheReviewerNoBody(t *testing.T) {
	e := recalledOperatorNote()
	req := recalledOperatorNoteRequest()

	rec := recalledInvestigation(req, e, 0.90)
	review := renderForReview(req, rec, nil)

	if !strings.Contains(review, e.Description) {
		t.Errorf("the reviewer is no longer shown the recalled entry's description (%q):\n%s\n"+
			"learning-loop.md §10 rests on that description being what verify judges", e.Description, review)
	}
	for _, sentinel := range []string{
		"SENTINEL-NOTE-TEXT",  // the operator's own words
		"SENTINEL-SYMPTOM",    // and the sections an Incident would carry…
		"SENTINEL-CAUSE",      // …which recalledInvestigation copies into inv.Prior…
		"SENTINEL-RESOLUTION", // …and renderForReview still never shows.
	} {
		if strings.Contains(review, sentinel) {
			t.Errorf("the recalled entry's body now reaches the verify reviewer (%s):\n%s\n"+
				"learning-loop.md §10 says it never does — update the page (and reconsider whether the "+
				"measured rejections still stand) before shipping this", sentinel, review)
		}
	}

	// The Incident half of the same claim, stated positively: the sections ARE parsed
	// and carried, they simply are not part of the review. Without this the test above
	// would also pass if Prior stopped being populated at all, which is a different
	// (and separately breaking) change.
	if rec.Prior == nil || rec.Prior.Cause != "SENTINEL-CAUSE" || rec.Prior.Resolution != "SENTINEL-RESOLUTION" {
		t.Errorf("recalledInvestigation no longer carries the entry's Cause/Resolution as Prior (%+v) — "+
			"the point of the test above is that it carries them and verify still never sees them", rec.Prior)
	}
}

// TestKBSearchShowsTheWholeEntry is the other half of §10's practical conclusion:
// a note pays off through kb_search, which renders the entry BODY, and not through
// instant recall, which (above) never shows it. Same entry, two renderers, opposite
// answers — that contrast is the whole recommendation, so it is pinned rather than
// asserted.
func TestKBSearchShowsTheWholeEntry(t *testing.T) {
	e := recalledOperatorNote()
	hits := renderHits([]catalog.Entry{e})

	for _, want := range []string{e.Title, e.Description, "SENTINEL-NOTE-TEXT"} {
		if !strings.Contains(hits, want) {
			t.Errorf("kb_search no longer surfaces %q:\n%s\n"+
				"learning-loop.md §10 tells operators a note pays off via kb_search precisely because "+
				"this path renders the whole entry", want, hits)
		}
	}
}

// TestVerifyPathIgnoresEntryType pins the correction itself: no decision on the
// review path is a function of the entry's OKF type. The same entry reviewed as a
// Concept and as an Incident must produce a byte-identical review, so nobody can
// re-introduce "a Concept structurally cannot satisfy verify" without this failing.
func TestVerifyPathIgnoresEntryType(t *testing.T) {
	req := recalledOperatorNoteRequest()

	asConcept := recalledOperatorNote()
	asIncident := recalledOperatorNote()
	asIncident.Type = "Incident"

	got := renderForReview(req, recalledInvestigation(req, asConcept, 0.90), nil)
	want := renderForReview(req, recalledInvestigation(req, asIncident, 0.90), nil)
	if got != want {
		t.Errorf("the verify review now differs by entry type:\nConcept:\n%s\nIncident:\n%s\n"+
			"learning-loop.md §10 says the type is not read on this path — if that changed, the page's "+
			"correction (it is what a note SAYS, not what it is typed) is now wrong", got, want)
	}
}
