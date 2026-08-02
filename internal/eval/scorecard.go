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

	"github.com/Smana/runlore/internal/providers"
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
// cost-per-investigation breakdown, a confidence-calibration summary, and the run
// history.
//
// inUSD/cachedUSD/outUSD are the token rates (USD per MTok) this run was priced at.
// They travel as explicit floats rather than a *config.Pricing so this package does
// not take a dependency on internal/config; RunEvalScorecard reads a report from
// disk with no config in hand and forwards the rates carried on Report instead.
func ScorecardMarkdown(rep Report, history []HistoryEntry, inUSD, cachedUSD, outUSD float64) string {
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

	b.WriteString(costSection(rep, inUSD, cachedUSD, outUSD))

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

// costSection renders the cost-per-investigation comparison: what a full
// investigation costs against what an instant recall costs, on this run's model at
// this run's prices. It is the single most concrete claim the learning loop makes —
// recall is roughly an order of magnitude cheaper — and publishing it turns an
// assertion into a measurement.
//
// Returns "" when prices are unset or the report carries no per-case token data;
// a fabricated or zeroed cost would be worse than no cost at all.
//
// inUSD and outUSD are each required (OR, not AND): config.Pricing's rates are
// independently optional and validatePricing only rejects negatives, so a config
// setting one and omitting the other passes validation. If we only checked
// inUSD==0 && outUSD==0, a lone rate would slip through and EstimateCostUSD would
// silently multiply the other token type by zero — understating the published
// cost with no signal anything is wrong.
func costSection(rep Report, inUSD, cachedUSD, outUSD float64) string {
	if inUSD == 0 || outUSD == 0 {
		return ""
	}
	var full, recall []ReportCase
	var mixed int
	for _, c := range rep.Cases {
		if c.InputTokens == 0 && c.OutputTokens == 0 {
			continue // no usage reported for this case
		}
		// RecallShortCircuit is a COUNT of repeats that short-circuited, not a
		// bool: a case's Runs repeats can disagree (e.g. 1 of 5 recalled, 4 ran the
		// full loop). Bucketing "> 0" into recall would label that case an instant
		// recall while its rendered token figure is the median across all Runs —
		// mostly full-investigation tokens. Only the unanimous ends of the split are
		// trustworthy; anything in between is excluded (see below), not guessed at.
		switch c.RecallShortCircuit {
		case c.Runs:
			recall = append(recall, c)
		case 0:
			full = append(full, c)
		default:
			mixed++
		}
	}
	if len(full) == 0 && len(recall) == 0 {
		return ""
	}

	var b strings.Builder
	b.WriteString("\n## Cost per investigation\n\n")
	fmt.Fprintf(&b, "Median provider-reported tokens per case on `%s`, priced at $%.2f/MTok in · $%.2f/MTok out. ",
		rep.Model, inUSD, outUSD)
	b.WriteString("Replay evidence, so tool latency and live-cluster variance are excluded.\n\n")
	if mixed > 0 {
		// Disclosed, not dropped silently: a mixed case's median token count would
		// blend recall and full-loop paths, understating whichever row it got
		// stuffed into. Naming the count is what keeps this table honest.
		fmt.Fprintf(&b, "%d case(s) whose repeats disagreed about recall are excluded from this table — "+
			"their median token count would mix both paths.\n\n", mixed)
	}
	b.WriteString("| path | cases | median in tok | median out tok | est. cost |\n|---|---|---|---|---|\n")
	row := func(label string, cs []ReportCase) {
		if len(cs) == 0 {
			return
		}
		ins := make([]float64, 0, len(cs))
		outs := make([]float64, 0, len(cs))
		for _, c := range cs {
			ins = append(ins, float64(c.InputTokens))
			outs = append(outs, float64(c.OutputTokens))
		}
		mi, mo := medianFloat(ins), medianFloat(outs)
		// CachedInputTokens is left at its zero value: ReportCase carries no
		// per-case cached-token field today, so cachedUSD is accepted here only
		// for signature completeness against EstimateCostUSD. It starts actually
		// discounting anything once ReportCase gains that field.
		cost := EstimateCostUSD(
			providers.Usage{InputTokens: int(mi), OutputTokens: int(mo)},
			inUSD, cachedUSD, outUSD)
		fmt.Fprintf(&b, "| %s | %d | %s | %s | $%.3f |\n",
			label, len(cs), compactTokens(int(mi)), compactTokens(int(mo)), cost)
	}
	row("full investigation", full)
	row("instant recall", recall)
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
