// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"bytes"
	"fmt"
	"os"
	"testing"
)

// The docs deploy does not overwrite website/content/eval.md — it splices the
// nightly scorecard between two HTML-comment markers so the hand-written framing
// above them (what a scenario is, what the pass-rate does not mean) survives every
// republish. The splice is an awk program comparing `$0 == "<marker>"`, so it
// matches WHOLE LINES, and every way of getting that shape wrong fails SILENTLY on
// a deploy that reports success:
//
//   - a stray trailing space on the begin marker and awk never injects, so /eval
//     keeps telling readers no run has been published — indefinitely;
//   - the same on the end marker and awk never leaves skip mode, dropping
//     everything from the begin marker to the end of the page;
//   - a doubled marker injects the scorecard twice, a reversed pair duplicates the
//     page body instead of replacing the placeholder.
//
// docs.yml asserts this shape too, but it only runs on push to main — after merge,
// where the symptom is the whole docs site quietly ceasing to update. This runs in
// `go test ./...` on every PR, so a reflow of eval.md that costs it a marker fails
// in review, while the fix is still a one-line edit on the branch.
const (
	evalPagePath     = "../../website/content/eval.md"
	docsWorkflowPath = "../../.github/workflows/docs.yml"
	scorecardBegin   = "<!-- scorecard:begin -->"
	scorecardEnd     = "<!-- scorecard:end -->"
)

// scorecardMarkerShape reports whether page carries exactly the marker shape the
// awk splice needs: one begin line and one end line, compared whole, begin first.
// Whole-line comparison is the point — matching a substring here would accept the
// pages the splice silently mangles, which is the bug this guard exists to catch.
func scorecardMarkerShape(page []byte, begin, end string) error {
	var begins, ends []int
	for i, line := range bytes.Split(page, []byte("\n")) {
		switch string(line) {
		case begin:
			begins = append(begins, i+1)
		case end:
			ends = append(ends, i+1)
		}
	}
	if len(begins) != 1 || len(ends) != 1 {
		return fmt.Errorf("want exactly one %q line and one %q line, got %d and %d — "+
			"the splice compares whole lines, so a marker carrying leading or trailing "+
			"whitespace (or a stray second copy) does not count",
			begin, end, len(begins), len(ends))
	}
	if begins[0] > ends[0] {
		return fmt.Errorf("%q is on line %d, after %q on line %d — in that order the "+
			"splice duplicates the page body instead of replacing the block",
			begin, begins[0], end, ends[0])
	}
	return nil
}

func TestEvalPageCarriesTheScorecardMarkerShape(t *testing.T) {
	page, err := os.ReadFile(evalPagePath)
	if err != nil {
		t.Fatalf("read eval.md: %v", err)
	}
	if err := scorecardMarkerShape(page, scorecardBegin, scorecardEnd); err != nil {
		t.Errorf("website/content/eval.md: %v", err)
	}

	// Guard the guard: the markers above matter only because docs.yml splices on
	// these exact literals with these exact comparisons. Renaming them there would
	// leave this test pinning a string nothing reads, reporting success over a
	// deploy that no longer works.
	workflow, err := os.ReadFile(docsWorkflowPath)
	if err != nil {
		t.Fatalf("read docs.yml: %v", err)
	}
	for _, marker := range []string{scorecardBegin, scorecardEnd} {
		cmp := fmt.Sprintf("$0 == %q", marker)
		if !bytes.Contains(workflow, []byte(cmp)) {
			t.Errorf("docs.yml no longer splices on `%s` — the marker was renamed there "+
				"and this guard is now inert; rename it on both sides", cmp)
		}
	}
}

func TestScorecardMarkerShapeRejectsMalformedPages(t *testing.T) {
	const b, e = "<!-- b -->", "<!-- e -->"
	tests := []struct {
		name string
		page string
		want bool // true: the splice would work on this page
	}{
		{"well formed", "intro\n" + b + "\nblock\n" + e + "\ntail\n", true},
		{"a marker also named in prose", "see " + b + " below\n" + b + "\nblock\n" + e + "\n", true},
		{"trailing space on begin", "intro\n" + b + " \nblock\n" + e + "\n", false},
		{"trailing space on end", "intro\n" + b + "\nblock\n" + e + " \n", false},
		{"leading space on begin", "intro\n  " + b + "\nblock\n" + e + "\n", false},
		{"carriage return on begin", "intro\n" + b + "\r\nblock\n" + e + "\n", false},
		{"begin missing", "intro\nblock\n" + e + "\n", false},
		{"end missing", "intro\n" + b + "\nblock\n", false},
		{"both missing", "intro\nblock\n", false},
		{"reversed", "intro\n" + e + "\nblock\n" + b + "\n", false},
		{"doubled begin", "intro\n" + b + "\n" + b + "\nblock\n" + e + "\n", false},
		{"doubled end", "intro\n" + b + "\nblock\n" + e + "\n" + e + "\n", false},
		{"marker only inline in prose", "intro\nsee " + b + " here\nblock\n" + e + "\n", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := scorecardMarkerShape([]byte(tt.page), b, e)
			if tt.want && err != nil {
				t.Errorf("want the page accepted, got error: %v", err)
			}
			if !tt.want && err == nil {
				t.Error("want the page rejected, got no error — the splice would mangle it silently")
			}
		})
	}
}
