// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"encoding/json"

	"github.com/Smana/runlore/internal/providers"
)

// Report is the serializable replay-campaign report — the `*-replay.json` schema
// the nightly eval writes and `lore eval scorecard` consumes. It is a strict
// superset of the pre-v0.11 Campaign.JSON schema: every existing key is
// unchanged; provenance (at/model/tokens/cost) and per-case recall telemetry
// are additive, so old reports still parse.
type Report struct {
	At           string       `json:"at,omitempty"`    // run timestamp (RFC3339, UTC)
	Model        string       `json:"model,omitempty"` // "<provider>/<model>" disclosure
	N            int          `json:"n"`
	PassRate     float64      `json:"pass_rate"`
	Reached      int          `json:"reached"`
	Total        int          `json:"total"`
	InputTokens  int          `json:"input_tokens,omitempty"`
	OutputTokens int          `json:"output_tokens,omitempty"`
	CostUSD      *float64     `json:"cost_usd,omitempty"` // present only when config.model.pricing is set
	Cases        []ReportCase `json:"cases"`
}

// ReportCase is one case's k-of-n verdict in the report.
type ReportCase struct {
	Name        string   `json:"name"`
	Runs        int      `json:"runs"`
	PassRate    float64  `json:"pass_rate"`
	Reached     bool     `json:"reached"`
	Flaky       bool     `json:"flaky"`
	Confidence  float64  `json:"confidence"`
	Missing     []string `json:"missing,omitempty"`
	OverClaimed []string `json:"over_claimed,omitempty"`

	// Recall telemetry (cases with a catalog fixture only).
	HasRecall          bool   `json:"has_recall,omitempty"`
	ExpectRecall       string `json:"expect_recall,omitempty"`
	RecallFired        int    `json:"recall_fired_runs,omitempty"`
	RecallShortCircuit int    `json:"recall_short_circuit_runs,omitempty"`
}

// Report projects the campaign plus its provenance into the serializable report.
func (c Campaign) Report(at, model string, usage providers.Usage, costUSD *float64) Report {
	rep := Report{
		At: at, Model: model, N: c.N,
		PassRate: c.PassRate(), Reached: c.ReachedCases(), Total: len(c.Aggregates),
		InputTokens: usage.InputTokens, OutputTokens: usage.OutputTokens, CostUSD: costUSD,
	}
	for _, a := range c.Aggregates {
		rep.Cases = append(rep.Cases, ReportCase{
			Name: a.Name, Runs: a.Runs, PassRate: a.PassRate, Reached: a.Reached, Flaky: a.Flaky,
			Confidence: a.Confidence, Missing: a.Missing, OverClaimed: a.OverClaimed,
			HasRecall: a.HasRecall, ExpectRecall: a.ExpectRecall,
			RecallFired: a.RecallFired, RecallShortCircuit: a.RecallShortCircuit,
		})
	}
	return rep
}

// JSON renders the indented machine-readable report.
func (rep Report) JSON() ([]byte, error) {
	return json.MarshalIndent(rep, "", "  ")
}

// EstimateCostUSD prices a usage total: cached input tokens bill at the cached
// rate and the non-cached remainder at the input rate (InputTokens INCLUDES
// cached — mirrors internal/investigate's cost()). Rates are USD per MILLION tokens.
func EstimateCostUSD(u providers.Usage, inputUSDPerMTok, cachedUSDPerMTok, outputUSDPerMTok float64) float64 {
	uncached := u.InputTokens - u.CachedInputTokens
	if uncached < 0 {
		uncached = 0
	}
	return float64(uncached)/1e6*inputUSDPerMTok +
		float64(u.CachedInputTokens)/1e6*cachedUSDPerMTok +
		float64(u.OutputTokens)/1e6*outputUSDPerMTok
}
