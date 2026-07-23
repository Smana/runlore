// SPDX-License-Identifier: Apache-2.0

// Scorecard rendering: turns the nightly replay report (Report) into the public
// artifacts published on the eval-scorecard branch — a browsable scorecard.md, a
// shields.io endpoint badge.json, and an append-only history.jsonl. Pure functions
// over bytes so the whole pipeline is testable without CI.
package eval

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	maxHistory   = 365 // history.jsonl cap: one nightly line/day ≈ one year
	historyShown = 30  // history rows rendered in scorecard.md (newest first)

	// Calibration bands for the scorecard summary. confidentWrongFloor deliberately
	// matches evalMinPassRate's spirit: a missed case the model was ≥70% sure about
	// is the "confident and wrong" failure mode published benchmarks care about.
	confidentWrongFloor = 0.70
	underConfidentCeil  = 0.50
)

// HistoryEntry is one nightly run in the scorecard history (one JSONL line).
type HistoryEntry struct {
	At           string   `json:"at"`
	Model        string   `json:"model,omitempty"`
	N            int      `json:"n"`
	PassRate     float64  `json:"pass_rate"`
	Reached      int      `json:"reached"`
	Total        int      `json:"total"`
	InputTokens  int      `json:"input_tokens,omitempty"`
	OutputTokens int      `json:"output_tokens,omitempty"`
	CostUSD      *float64 `json:"cost_usd,omitempty"`
}

// HistoryFromReport projects a replay report onto its one-line history record.
func HistoryFromReport(rep Report) HistoryEntry {
	return HistoryEntry{
		At: rep.At, Model: rep.Model, N: rep.N,
		PassRate: rep.PassRate, Reached: rep.Reached, Total: rep.Total,
		InputTokens: rep.InputTokens, OutputTokens: rep.OutputTokens, CostUSD: rep.CostUSD,
	}
}

// AppendHistory appends e to the JSONL history, replacing any line with the same
// At (so re-publishing one run is idempotent) and capping the log at maxHistory
// entries (oldest dropped). Returns the new JSONL bytes and the entries oldest-first.
func AppendHistory(existing []byte, e HistoryEntry) ([]byte, []HistoryEntry, error) {
	var entries []HistoryEntry
	for _, line := range bytes.Split(existing, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		var h HistoryEntry
		if err := json.Unmarshal(line, &h); err != nil {
			return nil, nil, fmt.Errorf("parse history line %q: %w", line, err)
		}
		if h.At == e.At {
			continue // same run re-rendered: its fresh line replaces the old one
		}
		entries = append(entries, h)
	}
	entries = append(entries, e)
	if len(entries) > maxHistory {
		entries = entries[len(entries)-maxHistory:]
	}
	var out bytes.Buffer
	for _, h := range entries {
		b, err := json.Marshal(h)
		if err != nil {
			return nil, nil, err
		}
		out.Write(b)
		out.WriteByte('\n')
	}
	return out.Bytes(), entries, nil
}

// BadgeJSON renders the shields.io "endpoint" badge document
// (https://shields.io/badges/endpoint-badge) for the README pass-rate badge.
// Color bands: ≥90% brightgreen, ≥ the 70% CI gate green, ≥50% yellow, else red.
func BadgeJSON(rep Report) []byte {
	color := "red"
	switch {
	case rep.PassRate >= 0.9:
		color = "brightgreen"
	case rep.PassRate >= evalMinPassRate:
		color = "green"
	case rep.PassRate >= 0.5:
		color = "yellow"
	}
	b, _ := json.Marshal(map[string]any{
		"schemaVersion": 1,
		"label":         "nightly eval",
		"message":       fmt.Sprintf("%d/%d scenarios · %.0f%%", rep.Reached, rep.Total, rep.PassRate*100),
		"color":         color,
	})
	return b
}

// ScorecardMarkdown renders the browsable public scorecard: reproduce command,
// provenance (model, date, cost), per-scenario table with recall outcomes, a
// confidence-calibration summary, and the run history.
func ScorecardMarkdown(rep Report, history []HistoryEntry) string {
	var b strings.Builder
	b.WriteString("# RunLore nightly eval scorecard\n\n")
	b.WriteString("Auto-published by [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml) — ")
	b.WriteString("the replay eval scores the model+loop over recorded incident evidence (no live cluster), so anyone can reproduce it:\n\n")
	b.WriteString("```\nlore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7\n```\n\n")

	fmt.Fprintf(&b, "**Latest run:** %s", rep.At)
	if rep.Model != "" {
		fmt.Fprintf(&b, " · model `%s`", rep.Model)
	}
	fmt.Fprintf(&b, " · **%d/%d scenarios reached (%.0f%%)** · n=%d runs/case, k-of-n bar %.0f%%",
		rep.Reached, rep.Total, rep.PassRate*100, rep.N, evalMinPassRate*100)
	if rep.CostUSD != nil {
		fmt.Fprintf(&b, " · est. cost $%.2f (%s in / %s out tokens)",
			*rep.CostUSD, compactTokens(rep.InputTokens), compactTokens(rep.OutputTokens))
	} else if rep.InputTokens+rep.OutputTokens > 0 {
		fmt.Fprintf(&b, " · %s in / %s out tokens", compactTokens(rep.InputTokens), compactTokens(rep.OutputTokens))
	}
	b.WriteString("\n\n## Scenarios (latest run)\n\n")
	b.WriteString("| scenario | result | pass-rate | median confidence | recall | notes |\n")
	b.WriteString("|---|---|---|---|---|---|\n")
	for _, c := range rep.Cases {
		fmt.Fprintf(&b, "| %s | %s | %.0f%% (n=%d) | %.2f | %s | %s |\n",
			c.Name, resultCell(c), c.PassRate*100, c.Runs, c.Confidence, recallCell(c), notesCell(c))
	}

	b.WriteString("\n## Confidence calibration\n\n")
	var confidentWrong, underConfident []string
	for _, c := range rep.Cases {
		if !c.Reached && c.Confidence >= confidentWrongFloor {
			confidentWrong = append(confidentWrong, c.Name)
		}
		if c.Reached && c.Confidence < underConfidentCeil {
			underConfident = append(underConfident, c.Name)
		}
	}
	fmt.Fprintf(&b, "- **Confidently wrong** (missed with median confidence ≥ %.2f): %s\n", confidentWrongFloor, nameList(confidentWrong))
	fmt.Fprintf(&b, "- **Underconfident** (reached with median confidence < %.2f): %s\n", underConfidentCeil, nameList(underConfident))

	b.WriteString("\n## History\n\n")
	fmt.Fprintf(&b, "Newest first, last %d shown — the full log is [`history.jsonl`](history.jsonl). ", historyShown)
	b.WriteString("Runs below the CI gate publish here exactly like green ones.\n\n")
	b.WriteString("| date | model | reached | pass-rate | est. cost |\n|---|---|---|---|---|\n")
	shown := history
	if len(shown) > historyShown {
		shown = shown[len(shown)-historyShown:]
	}
	for i := len(shown) - 1; i >= 0; i-- {
		h := shown[i]
		cost := "—"
		if h.CostUSD != nil {
			cost = fmt.Sprintf("$%.2f", *h.CostUSD)
		}
		fmt.Fprintf(&b, "| %s | %s | %d/%d | %.0f%% | %s |\n", h.At, h.Model, h.Reached, h.Total, h.PassRate*100, cost)
	}
	return b.String()
}

func resultCell(c ReportCase) string {
	switch {
	case c.Reached:
		return "✅ PASS"
	case c.Flaky:
		return "⚠️ FLAKY"
	default:
		return "❌ MISS"
	}
}

func recallCell(c ReportCase) string {
	if !c.HasRecall {
		return "—"
	}
	s := fmt.Sprintf("fired %d/%d · short-circuit %d/%d", c.RecallFired, c.Runs, c.RecallShortCircuit, c.Runs)
	if c.ExpectRecall != "" {
		s += fmt.Sprintf(" (expect: %s)", c.ExpectRecall)
	}
	return s
}

func notesCell(c ReportCase) string {
	if len(c.Missing) == 0 {
		return "—"
	}
	return strings.Join(c.Missing, ", ")
}

func nameList(names []string) string {
	if len(names) == 0 {
		return "none"
	}
	return fmt.Sprintf("%d — %s", len(names), strings.Join(names, ", "))
}

// compactTokens renders a token count as 1.2M / 84.0k / 512 for the summary line.
func compactTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000:
		return fmt.Sprintf("%.1fk", float64(n)/1e3)
	default:
		return fmt.Sprintf("%d", n)
	}
}
