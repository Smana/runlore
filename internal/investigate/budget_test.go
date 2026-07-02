package investigate

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestEstimateTokens(t *testing.T) {
	msgs := []providers.Message{
		{Role: "user", Content: "12345678"},  // 8 chars
		{Role: "assistant", Content: "1234"}, // 4 chars
	}
	// system(4) + content(8+4) = 16; no tools, no tool-calls. 16/4 = 4.
	if got := estimateTokens("sys!", msgs, nil); got != 4 {
		t.Fatalf("estimateTokens (content only): got %d, want 4", got)
	}
}

// TestEstimateTokensCountsToolCallArgsAndSpecs proves the estimate now includes
// the assistant tool-call JSON (m.ToolCalls[].Args) and the full tool schemas
// (re-sent every step) — bytes the old content-only sum ignored, which let the
// hard-kill guard fire late or never on a tool-heavy investigation.
func TestEstimateTokensCountsToolCallArgsAndSpecs(t *testing.T) {
	msgs := []providers.Message{
		{Role: "assistant", Content: "ab", ToolCalls: []providers.ToolCall{
			{Name: "kube", Args: `{"k":"v"}`}, // 9 chars of args
		}},
	}
	tools := []providers.ToolSpec{
		{Name: "kube", Description: "desc", Schema: `{"type":"object"}`}, // 4+4+17 = 25 chars
	}

	// Old behaviour summed only system + content.
	oldSum := (len("sys") + len("ab")) / 4 // (3 + 2)/4 = 1

	got := estimateTokens("sys", msgs, tools)

	// New behaviour adds tool-call args (9) and tool-spec bytes (25):
	// (3 + 25 + 2 + 9) / 4 = 39/4 = 9.
	if got != 9 {
		t.Fatalf("estimateTokens with tool-calls+specs: got %d, want 9", got)
	}
	if got <= oldSum {
		t.Fatalf("estimate (%d) should exceed old content-only sum (%d)", got, oldSum)
	}
}

// TestTokenCalibrationEstimate proves the budget estimate is anchored to
// provider-reported usage: once a completion reports its real InputTokens, the
// next estimate is the chars/4 heuristic scaled by actual/heuristic. Zero usage
// (provider didn't report) falls back to the pure heuristic, and the ratio is
// floored at 1 so the anchored estimate is never BELOW the raw heuristic — the
// budget guard can only fire earlier than uncalibrated, never later.
func TestTokenCalibrationEstimate(t *testing.T) {
	sys := "sys!"                                                                  // 4 chars
	msgs := []providers.Message{{Role: "user", Content: strings.Repeat("x", 396)}} // 396 chars
	const raw = 100                                                                // (4+396)/4
	if got := estimateTokens(sys, msgs, nil); got != raw {
		t.Fatalf("raw heuristic sanity: got %d, want %d", got, raw)
	}

	type obs struct {
		heuristic int
		usage     providers.Usage
	}
	tests := []struct {
		name     string
		observed []obs
		want     int
	}{
		{name: "uncalibrated falls back to the pure heuristic", observed: nil, want: raw},
		{name: "zero usage (provider did not report) leaves the heuristic",
			observed: []obs{{heuristic: 100, usage: providers.Usage{}}}, want: raw},
		{name: "zero heuristic is ignored (no divide-by-zero, no calibration)",
			observed: []obs{{heuristic: 0, usage: providers.Usage{InputTokens: 500}}}, want: raw},
		{name: "reported usage scales the next estimate (ratio 1.5)",
			observed: []obs{{heuristic: 100, usage: providers.Usage{InputTokens: 150}}}, want: 150},
		{name: "fractional ratio rounds up, never down",
			observed: []obs{{heuristic: 100, usage: providers.Usage{InputTokens: 101}}}, want: 101},
		{name: "ratio floored at 1: anchored estimate is never below the heuristic",
			observed: []obs{{heuristic: 200, usage: providers.Usage{InputTokens: 100}}}, want: raw},
		{name: "latest observation wins (recalibrates each completion)",
			observed: []obs{
				{heuristic: 100, usage: providers.Usage{InputTokens: 300}},
				{heuristic: 100, usage: providers.Usage{InputTokens: 150}},
			}, want: 150},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var c tokenCalibration
			for _, o := range tt.observed {
				c.observe(o.heuristic, o.usage)
			}
			if got := c.estimate(sys, msgs, nil); got != tt.want {
				t.Fatalf("estimate: got %d, want %d", got, tt.want)
			}
		})
	}
}

// TestTokenCalibrationHeuristicTarget proves the compaction target is converted
// into raw-heuristic space (compactHistory measures with estimateTokens), so a
// calibrated loop compacts down to a REAL 0.7×budget, not a heuristic one.
func TestTokenCalibrationHeuristicTarget(t *testing.T) {
	var c tokenCalibration
	if got := c.heuristicTarget(700); got != 700 {
		t.Fatalf("uncalibrated target must pass through: got %d, want 700", got)
	}
	c.observe(100, providers.Usage{InputTokens: 200}) // ratio 2
	if got := c.heuristicTarget(700); got != 350 {
		t.Fatalf("ratio-2 target: got %d, want 350", got)
	}
	c.observe(100, providers.Usage{InputTokens: 50}) // ratio floored at 1
	if got := c.heuristicTarget(700); got != 700 {
		t.Fatalf("floored-ratio target must pass through: got %d, want 700", got)
	}
}

func TestOverBudget(t *testing.T) {
	if overBudget(100, 50) != true {
		t.Fatal("100>50 should be over budget")
	}
	if overBudget(10, 50) != false {
		t.Fatal("10<50 should be under budget")
	}
	if overBudget(100, 0) != false {
		t.Fatal("budget 0 means unlimited")
	}
}
