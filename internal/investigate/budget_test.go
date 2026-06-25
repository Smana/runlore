package investigate

import (
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
