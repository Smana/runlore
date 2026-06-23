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
	// (len(system)=4 + 8 + 4) / 4 = 4
	if got := estimateTokens("sys!", msgs); got != 4 {
		t.Fatalf("estimateTokens: got %d, want 4", got)
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
