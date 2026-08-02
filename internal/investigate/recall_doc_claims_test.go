// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// docClaimsPath is the published page whose numbers this test pins.
const docClaimsPath = "../../website/content/docs/concepts/learning-loop.md"

// TestRecallDocClaimsMatchMeasurement is a DRIFT GUARD over the real parse target:
// it reads the numbers published in learning-loop.md and compares them to the values
// the measurement actually produces right now. Editing the prose without re-measuring
// (or changing the engine without updating the prose) fails here.
//
// It also asserts the METHODOLOGY caveat is present. The rerank-on row is measured
// with a scripted reranker that accepts the correct target by construction, so the
// row measures the fire gate, not model judgment — a page that quotes the number
// without saying so overstates it.
//
// WHAT THIS GUARD DOES NOT PROTECT. The caveat check is keyword presence, not meaning.
// A reviewer demonstrated the gap: prose that keeps these words while asserting the
// opposite — that real models reach the same fire-rate, "confirming the reranker's
// real-world accuracy" — still passes. Semantic checking of prose is not achievable
// here, so treat this half as a reminder to a future editor, not a proof. The NUMBERS
// above are genuinely pinned; the framing around them still needs a human to read it.
func TestRecallDocClaimsMatchMeasurement(t *testing.T) {
	raw, err := os.ReadFile(docClaimsPath)
	if err != nil {
		t.Fatalf("read %s: %v", docClaimsPath, err)
	}
	doc := string(raw)

	cat := writeEvalCatalog(t)
	cases := evalCases()
	off := computeFire(t, cat, cases)
	on := computeFireReranked(t, cat, cases, &scriptedReranker{accept: rerankTargets(cases), conf: 0.9})

	// | rerank **off** | 0/11 (0.00) | — | 0/2 |
	offRow := regexp.MustCompile(`rerank \*\*off\*\* \| (\d+)/(\d+)`)
	m := offRow.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("could not find the rerank-off row in %s — did the table change shape?", docClaimsPath)
	}
	assertInt(t, "rerank-off fired", m[1], off.fired)
	assertInt(t, "label positives", m[2], off.labelPositives)

	// | rerank **on** | **11/11 (1.00)** | **1.00** | **0/2** |
	onRow := regexp.MustCompile(`rerank \*\*on\*\* \| \*\*(\d+)/(\d+)`)
	m = onRow.FindStringSubmatch(doc)
	if m == nil {
		t.Fatalf("could not find the rerank-on row in %s", docClaimsPath)
	}
	assertInt(t, "rerank-on fired", m[1], on.fired)
	assertInt(t, "label positives (on)", m[2], on.labelPositives)

	// The methodology caveat must accompany the number.
	for _, want := range []string{"scripted", "not a model"} {
		if !contains(doc, want) {
			t.Errorf("learning-loop.md must state the methodology caveat containing %q, "+
				"otherwise the rerank-on row reads as measured model accuracy", want)
		}
	}
}

func assertInt(t *testing.T, what, got string, want int) {
	t.Helper()
	n, err := strconv.Atoi(got)
	if err != nil {
		t.Fatalf("%s: %q is not a number", what, got)
	}
	if n != want {
		t.Errorf("%s: doc says %d, measurement says %d — the published number is stale", what, n, want)
	}
}

func contains(hay, needle string) bool {
	return len(hay) >= len(needle) && regexp.MustCompile(`(?i)`+regexp.QuoteMeta(needle)).MatchString(hay)
}
