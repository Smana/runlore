// SPDX-License-Identifier: Apache-2.0

package app

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"

	"github.com/Smana/runlore/internal/eval"
)

// RunEvalScorecard renders the public scorecard artifacts (scorecard.md,
// badge.json, history.jsonl) from a replay report. Keyless and config-free: it
// only reads the report JSON and the output dir's existing history, so CI can run
// it after the eval and anyone can run it locally on a downloaded report artifact.
func RunEvalScorecard(args []string) error {
	fs := flag.NewFlagSet("eval scorecard", flag.ContinueOnError)
	report := fs.String("report", "", "path to a *-replay.json report (required)")
	dir := fs.String("dir", "scorecard", "output directory (scorecard.md, badge.json, history.jsonl)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *report == "" {
		return fmt.Errorf("eval scorecard: -report <replay.json> is required")
	}
	data, err := os.ReadFile(*report) //nolint:gosec // G304: operator-supplied report path
	if err != nil {
		return err
	}
	var rep eval.Report
	if err := json.Unmarshal(data, &rep); err != nil {
		return fmt.Errorf("parse report %s: %w", *report, err)
	}
	if rep.Total == 0 {
		return fmt.Errorf("report %s has no cases — refusing to publish an empty scorecard", *report)
	}
	// An ERRORED run (eval.Report.Errored: every case failed before anything could be
	// scored) is PUBLISHED, labelled — not refused like the empty report above. The
	// two look alike and are not. A report with no cases is malformed and carries no
	// information, so writing it would replace the last real numbers with a blank. An
	// errored run is well-formed and does carry information — the provider was
	// unavailable on this date — and refusing it would leave the branch showing the
	// previous green run, making a broken provider indistinguishable from a nightly
	// that never fired. That silence is the failure mode the scorecard exists to
	// prevent, so the run publishes with a grey badge, an "ERRORED" summary and an
	// `errored` history line instead of as a 0% score.
	if rep.Errored() {
		fmt.Printf("scorecard: run %s ERRORED — no case produced a scoreable result; publishing it labelled, not as a 0%% score\n", rep.At)
	}
	if err := os.MkdirAll(*dir, 0o750); err != nil {
		return err
	}
	histPath := filepath.Join(*dir, "history.jsonl")
	existing, err := os.ReadFile(histPath) //nolint:gosec // G304: path derived from operator-supplied -dir
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	hist, entries, err := eval.AppendHistory(existing, eval.HistoryFromReport(rep))
	if err != nil {
		return err
	}
	if err := os.WriteFile(histPath, hist, 0o644); err != nil { //nolint:gosec // published artifact, world-readable
		return err
	}
	md := eval.ScorecardMarkdown(rep, entries, rep.InputUSDPerMTok, rep.CachedInputUSDPerMTok, rep.OutputUSDPerMTok)
	if err := os.WriteFile(filepath.Join(*dir, "scorecard.md"), []byte(md), 0o644); err != nil { //nolint:gosec
		return err
	}
	if err := os.WriteFile(filepath.Join(*dir, "badge.json"), eval.BadgeJSON(rep), 0o644); err != nil { //nolint:gosec
		return err
	}
	fmt.Printf("scorecard: %s (badge.json, history.jsonl alongside)\n", filepath.Join(*dir, "scorecard.md"))
	return nil
}
