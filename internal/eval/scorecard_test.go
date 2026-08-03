// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func scorecardFixtureReport() Report {
	cost := 0.16
	return Report{
		At: "2026-07-23T06:00:00Z", Model: "anthropic/claude-haiku-4-5-20251001",
		N: 5, PassRate: 0.5, Reached: 1, Total: 2,
		InputTokens: 120000, OutputTokens: 9000, CostUSD: &cost,
		Cases: []ReportCase{
			{Name: "harbor-chart-bump", Runs: 5, PassRate: 1, Reached: true, Confidence: 0.82},
			{Name: "poisoned-recall-verify", Runs: 5, PassRate: 0.4, Flaky: true, Confidence: 0.75,
				HasRecall: true, ExpectRecall: "withdrawn", RecallFired: 5,
				Missing: []string{"expect_recall=withdrawn but recall short_circuit"}},
		},
	}
}

// erroredFixtureReport is the shape the 2026-08-02 published run actually took: the
// provider answered nothing, so every case failed at the model boundary, no repeat
// was ever scored and no tokens were billed. Its 0% is an artefact of the outage.
func erroredFixtureReport() Report {
	zero := 0.0
	return Report{
		At: "2026-08-02T08:12:29Z", Model: "anthropic/claude-haiku-4-5-20251001",
		N: 5, PassRate: 0, Reached: 0, Total: 2, CostUSD: &zero,
		Cases: []ReportCase{
			{Name: "harbor-chart-bump", Runs: 5,
				Missing: []string{"investigation error: 401 authentication_error: invalid x-api-key"}},
			{Name: "poisoned-recall-verify", Runs: 5, HasRecall: true, ExpectRecall: "withdrawn",
				Missing: []string{"investigation error: 401 authentication_error: invalid x-api-key"}},
		},
	}
}

// genuineZeroReport is a real 0%: every case ran, the model answered, every answer
// was wrong. It must never be labelled errored — the run IS a measurement.
func genuineZeroReport() Report {
	cost := 0.14
	return Report{
		At: "2026-08-01T06:00:00Z", Model: "anthropic/claude-haiku-4-5-20251001",
		N: 5, PassRate: 0, Reached: 0, Total: 2,
		InputTokens: 118000, OutputTokens: 7400, CostUSD: &cost,
		Cases: []ReportCase{
			{Name: "harbor-chart-bump", Runs: 5, Confidence: 0.81,
				Missing: []string{"ImagePullBackOff"}, InputTokens: 59000, OutputTokens: 3700},
			{Name: "poisoned-recall-verify", Runs: 5, Confidence: 0.64,
				Missing: []string{"over-claimed: apps/web"}, InputTokens: 59000, OutputTokens: 3700},
		},
	}
}

func TestReportErrored(t *testing.T) {
	// Same genuine 0%, but from a provider that reports no usage at all. This is the
	// case a "spent nothing" predicate would mislabel as an outage.
	silentUsage := genuineZeroReport()
	silentUsage.InputTokens, silentUsage.OutputTokens = 0, 0
	silentUsage.CostUSD = nil
	for i := range silentUsage.Cases {
		silentUsage.Cases[i].InputTokens, silentUsage.Cases[i].OutputTokens = 0, 0
	}

	// A partial outage: some repeats did reach the model, so a real (if bad) score
	// exists underneath and the run is not "no result".
	partialOutage := erroredFixtureReport()
	partialOutage.InputTokens, partialOutage.OutputTokens = 61000, 3200

	// Same, but the survivor is visible only per-case (silent-usage provider): one
	// case had passing repeats, so something WAS scored.
	somePassed := erroredFixtureReport()
	somePassed.Cases[0].PassRate = 0.4

	// The model answered but never called submit — a model failure, not an outage.
	noSubmit := erroredFixtureReport()
	for i := range noSubmit.Cases {
		noSubmit.Cases[i].Missing = []string{"no findings (loop did not submit)"}
	}

	// One case errored, the other genuinely missed: the run still measured something.
	oneCaseOnly := erroredFixtureReport()
	oneCaseOnly.Cases[1].Missing = []string{"ImagePullBackOff"}

	// A case whose catalog fixture would not load never reached the model either.
	fixtureBroken := erroredFixtureReport()
	for i := range fixtureBroken.Cases {
		fixtureBroken.Cases[i].Missing = []string{"catalog fixture load error: open fixtures: no such file or directory"}
	}

	empty := erroredFixtureReport()
	empty.Cases = nil

	tests := []struct {
		name string
		rep  Report
		want bool
	}{
		{"provider answered nothing", erroredFixtureReport(), true},
		{"catalog fixture never loaded", fixtureBroken, true},
		{"genuine 0% with tokens", genuineZeroReport(), false},
		{"genuine 0% from a silent-usage provider", silentUsage, false},
		{"partial outage (tokens were spent)", partialOutage, false},
		{"partial outage (a case had passing repeats)", somePassed, false},
		{"model answered but never submitted", noSubmit, false},
		{"only one case errored", oneCaseOnly, false},
		{"green run", scorecardFixtureReport(), false},
		{"no cases at all", empty, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.rep.Errored(); got != tc.want {
				t.Fatalf("Errored() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestErroredRunFromRealRunner pins the predicate to the real failure path rather
// than to a hand-written fixture: it replays the SHIPPED example cases through the
// real Runner against a model that always errors (a provider outage), and asserts
// the projected report is recognised as errored. If Runner.runOne ever changes the
// note it records for a failed investigation, this fails — the fixture-based table
// above would not.
func TestErroredRunFromRealRunner(t *testing.T) {
	cases, err := Load(filepath.Join("..", "..", "examples", "eval"))
	if err != nil {
		t.Fatalf("Load examples/eval: %v", err)
	}
	r := &Runner{
		Model: &alwaysErrModel{err: errors.New("401 authentication_error: invalid x-api-key")},
		Log:   discardLog(),
	}
	camp := r.RunN(context.Background(), cases, 1)
	rep := camp.Report("2026-08-02T08:12:29Z", "anthropic/claude-haiku-4-5-20251001", providers.Usage{}, nil)
	if rep.Total == 0 {
		t.Fatal("expected the shipped cases to load")
	}
	if !rep.Errored() {
		t.Fatalf("a campaign where every case failed at the model boundary must be errored; report: %+v", rep)
	}
}

func TestBadgeJSONErrored(t *testing.T) {
	b := string(BadgeJSON(erroredFixtureReport()))
	if !strings.Contains(b, `"color":"lightgrey"`) {
		t.Fatalf("errored run must not wear a score colour: %s", b)
	}
	if !strings.Contains(b, "errored") {
		t.Fatalf("badge must say the run errored: %s", b)
	}
	for _, forbidden := range []string{"0/2", "0%"} {
		if strings.Contains(b, forbidden) {
			t.Fatalf("errored badge must not publish a pass-rate (%q): %s", forbidden, b)
		}
	}
	// A genuine 0% keeps reading as a genuine 0%.
	g := string(BadgeJSON(genuineZeroReport()))
	if !strings.Contains(g, `"message":"0/2 scenarios · 0%"`) || !strings.Contains(g, `"color":"red"`) {
		t.Fatalf("a real 0%% must still render as a red 0/2: %s", g)
	}
}

func TestScorecardMarkdownErrored(t *testing.T) {
	rep := erroredFixtureReport()
	_, entries, err := AppendHistory(nil, HistoryFromReport(rep))
	if err != nil {
		t.Fatal(err)
	}
	md := ScorecardMarkdown(rep, entries, 3, 0.3, 15)
	for _, want := range []string{
		"ERRORED",
		"401 authentication_error", // the error itself is disclosed
		"| 2026-08-02T08:12:29Z | anthropic/claude-haiku-4-5-20251001 | — | ⚠️ errored | — |", // history row
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("errored scorecard missing %q in:\n%s", want, md)
		}
	}
	for _, forbidden := range []string{
		"scenarios reached (0%)", // summary must not publish a pass-rate
		"❌ MISS",                 // a case that never ran did not "miss"
		"est. cost $0.00",        // a zero cost here means "nothing ran", not "it was free"
	} {
		if strings.Contains(md, forbidden) {
			t.Fatalf("errored scorecard must not render %q in:\n%s", forbidden, md)
		}
	}
}

func TestScorecardMarkdownGenuineZeroStillRendersAsFailure(t *testing.T) {
	rep := genuineZeroReport()
	_, entries, err := AppendHistory(nil, HistoryFromReport(rep))
	if err != nil {
		t.Fatal(err)
	}
	md := ScorecardMarkdown(rep, entries, 0, 0, 0)
	for _, want := range []string{
		"**0/2 scenarios reached (0%)**",
		"| harbor-chart-bump | ❌ MISS |",
		"| 2026-08-01T06:00:00Z | anthropic/claude-haiku-4-5-20251001 | 0/2 | 0% | $0.14 |",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("a real 0%% must still render as a real failure, missing %q in:\n%s", want, md)
		}
	}
	// The History section explains the `errored` label unconditionally, so assert on
	// the labels themselves rather than on the word.
	if strings.Contains(md, "ERRORED") || strings.Contains(md, "⚠️ errored") {
		t.Fatalf("a real 0%% must not be labelled errored:\n%s", md)
	}
}

// TestHistoryErroredIsDurable: the flag must survive a round-trip through
// history.jsonl (so past errored runs stay labelled on every future re-render), and
// lines written before the field existed must degrade to "not errored" rather than
// to a guess.
func TestHistoryErroredIsDurable(t *testing.T) {
	legacy := []byte(`{"at":"2026-07-23T06:00:00Z","model":"anthropic/claude-haiku-4-5-20251001","n":5,"pass_rate":0.5,"reached":1,"total":2,"cost_usd":0.16}` + "\n")
	out, entries, err := AppendHistory(legacy, HistoryFromReport(erroredFixtureReport()))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 entries, got %d", len(entries))
	}
	if entries[0].Errored {
		t.Fatal("a legacy line with no errored field must degrade to not-errored")
	}
	if !entries[1].Errored {
		t.Fatal("the errored run must be recorded as errored")
	}
	if !strings.Contains(string(out), `"errored":true`) {
		t.Fatalf("the flag must be persisted to history.jsonl:\n%s", out)
	}
	// Absence is the default: the legacy line must not grow a false flag.
	if strings.Contains(string(out), `"errored":false`) {
		t.Fatalf("non-errored lines must stay clean:\n%s", out)
	}
	// Re-parsing the written log keeps both labels — this is what "durable" means.
	_, reparsed, err := AppendHistory(out, HistoryFromReport(genuineZeroReport()))
	if err != nil {
		t.Fatal(err)
	}
	if reparsed[0].Errored || !reparsed[1].Errored || reparsed[2].Errored {
		t.Fatalf("labels did not survive the round-trip: %+v", reparsed)
	}
}

// TestNotesCellEscapesTableBreakers: Missing carries freeform provider error text on
// the errored path, and a raw pipe or newline would split the markdown row into
// phantom columns (or end it early), silently corrupting the published table.
func TestNotesCellEscapesTableBreakers(t *testing.T) {
	got := notesCell(ReportCase{Missing: []string{"investigation error: 429 rate_limit | retry\nafter 60s"}})
	if strings.Contains(got, "|") && !strings.Contains(got, `\|`) {
		t.Fatalf("pipe not escaped: %q", got)
	}
	if strings.ContainsAny(got, "\n\r") {
		t.Fatalf("newline not flattened: %q", got)
	}
}

func TestBadgeJSON(t *testing.T) {
	b := string(BadgeJSON(scorecardFixtureReport()))
	if !strings.Contains(b, `"schemaVersion":1`) {
		t.Fatalf("not a shields endpoint doc: %s", b)
	}
	if !strings.Contains(b, `"message":"1/2 scenarios · 50%"`) {
		t.Fatalf("badge message wrong: %s", b)
	}
	if !strings.Contains(b, `"color":"yellow"`) { // 0.5 is in [0.5, 0.7) ⇒ yellow
		t.Fatalf("badge color wrong: %s", b)
	}
	green := scorecardFixtureReport()
	green.PassRate, green.Reached = 1.0, 2
	if g := string(BadgeJSON(green)); !strings.Contains(g, `"color":"brightgreen"`) {
		t.Fatalf("1.0 should be brightgreen: %s", g)
	}
}

func TestAppendHistoryDedupesAndCaps(t *testing.T) {
	e := HistoryFromReport(scorecardFixtureReport())
	out, entries, err := AppendHistory(nil, e)
	if err != nil || len(entries) != 1 {
		t.Fatalf("first append: %v / %d entries", err, len(entries))
	}
	// Re-appending the same run (same At) must be idempotent.
	out2, entries2, err := AppendHistory(out, e)
	if err != nil || len(entries2) != 1 || string(out2) != string(out) {
		t.Fatalf("dedupe on At failed: %v / %d entries", err, len(entries2))
	}
	// Cap: appending beyond maxHistory drops the oldest.
	long := out
	for i := 0; i < maxHistory+10; i++ {
		e.At = "2026-07-23T06:00:00Z" + strings.Repeat("x", i+1) // unique At per line
		long, entries, err = AppendHistory(long, e)
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(entries) != maxHistory {
		t.Fatalf("want cap %d, got %d", maxHistory, len(entries))
	}
}

func TestScorecardMarkdown(t *testing.T) {
	rep := scorecardFixtureReport()
	_, entries, err := AppendHistory(nil, HistoryFromReport(rep))
	if err != nil {
		t.Fatal(err)
	}
	// This report predates per-case token attribution, so 0,0,0 exercises the "no
	// cost section" path — this test asserts on the sections the cost block doesn't touch.
	md := ScorecardMarkdown(rep, entries, 0, 0, 0)
	for _, want := range []string{
		"# RunLore nightly eval scorecard",
		"lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7", // reproduce command
		"anthropic/claude-haiku-4-5-20251001",                                              // model disclosure
		"**1/2 scenarios reached (50%)**",
		"est. cost $0.16",
		"| harbor-chart-bump | ✅ PASS |",
		"| poisoned-recall-verify | ⚠️ FLAKY |",
		"fired 5/5 · short-circuit 0/5 (expect: withdrawn)", // recall outcome column
		"## Confidence calibration",
		"poisoned-recall-verify", // 0.75 ≥ 0.70 and not reached ⇒ confidently wrong
		"## History",
	} {
		if !strings.Contains(md, want) {
			t.Fatalf("scorecard missing %q in:\n%s", want, md)
		}
	}
}
