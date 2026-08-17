// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// TestOperatorNotesAreNotClaimedToFireInstantRecall guards a claim the docs are tempting
// to make and that is false.
//
// An operator note captured from a thread is written as a `Concept` entry. `Concept` is
// evidence-free by design — chosen so a bare note clears the merge gate without
// fabricating Symptom/Cause/Resolution sections nobody wrote — and an evidence chain is
// exactly what the verify pass requires before a recall may short-circuit the loop. The
// two are structurally incompatible: measured on a live cluster, six recalls of operator
// notes at reranker confidence 0.82–0.92 produced six rejections, including one entry
// with clean, specific metadata.
//
// So a note widens kb_search and never fires instant recall. Any page saying otherwise
// promises a saving that does not exist, and — worse — would suggest a WRONG note gets
// served back with confidence, when in fact verify is the gate that stops exactly that
// (an adversarial test: a false note fooled the reranker at 0.92 and was caught here).
//
// This is a NEGATIVE guard: it cannot prove the docs describe the behaviour well, only
// that no page asserts the specific falsehood. Stated so a passing run is not mistaken
// for more than it is.
func TestOperatorNotesAreNotClaimedToFireInstantRecall(t *testing.T) {
	// A sentence tying operator notes to instant recall FIRING. Checked per SENTENCE,
	// and a sentence carrying a negation is skipped — the honest pages are made of exactly
	// such sentences ("a note widens kb_search; it does not enable instant recall"), and an
	// earlier version of this guard stripped negation WORDS and then fired on the very
	// paragraph documenting the limitation correctly. Matching the whole sentence and
	// asking whether it is negated is the robust form.
	claim := regexp.MustCompile(`(?i)(operator note|thread capture|@runlore note)`)
	recall := regexp.MustCompile(`(?i)instant[- ]recall`)
	affirm := regexp.MustCompile(`(?i)\b(fires?|firing|triggers?|enables?|short-circuits?|allows?)\b`)
	negated := regexp.MustCompile(`(?i)\b(not|never|cannot|can't|without|nor|no)\b`)

	pages := threadDocPages(t)
	for _, p := range pages {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("read %s: %v", p, err)
		}
		for _, sentence := range splitSentences(string(b)) {
			if !claim.MatchString(sentence) || !recall.MatchString(sentence) {
				continue
			}
			if !affirm.MatchString(sentence) || negated.MatchString(sentence) {
				continue
			}
			t.Errorf("%s claims operator notes fire instant recall, which they never do:\n  %q\n"+
				"Notes widen kb_search only; verify rejects Concept entries (6/6 measured), and that "+
				"rejection is also what stops a WRONG note being served with confidence.",
				p, strings.TrimSpace(sentence))
		}
	}
}

// splitSentences breaks markdown into rough sentences for the guard above. Rough is
// sufficient: the guard asks whether ONE claim and its negation co-occur, and both live in
// the same sentence in every phrasing that matters.
func splitSentences(src string) []string {
	src = strings.ReplaceAll(src, "\n", " ")
	return strings.FieldsFunc(src, func(r rune) bool { return r == '.' || r == ';' || r == '!' })
}

// TestLearningLoopDocumentsTheRecallLimit is the positive half: the limitation must be
// written down somewhere, not merely absent from the pages that would get it wrong.
// Without this, deleting the paragraph would leave the negative guard above passing
// vacuously.
func TestLearningLoopDocumentsTheRecallLimit(t *testing.T) {
	const page = "website/content/docs/concepts/learning-loop.md"
	b, err := os.ReadFile("../../" + page)
	if err != nil {
		t.Fatalf("read %s: %v", page, err)
	}
	src := strings.ToLower(string(b))
	for _, want := range []string{
		"widens `kb_search`", // what a note DOES do
		"instant recall",     // what it does not
		"concept",            // the entry type that makes it structural
		"verify",             // the gate that rejects it
	} {
		if !strings.Contains(src, strings.ToLower(want)) {
			t.Errorf("%s no longer explains the operator-note recall limit: missing %q", page, want)
		}
	}
}
