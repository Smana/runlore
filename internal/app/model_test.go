package app

import (
	"fmt"
	"testing"
)

// TestNewModelClient locks in provider selection: each provider name maps to its
// concrete client type, and any unknown/empty provider falls back to the OpenAI
// (vLLM-compatible) client.
func TestNewModelClient(t *testing.T) {
	tests := []struct {
		provider string
		wantType string
	}{
		{"anthropic", "*anthropic.Client"},
		{"gemini", "*gemini.Client"},
		{"openai", "*openai.Client"},
		{"", "*openai.Client"},
		{"vllm", "*openai.Client"},
	}
	for _, tc := range tests {
		t.Run(tc.provider, func(t *testing.T) {
			client := NewModelClient(tc.provider, "http://endpoint/v1", "test-model", "key")
			if client == nil {
				t.Fatalf("NewModelClient(%q) returned nil", tc.provider)
			}
			if got := fmt.Sprintf("%T", client); got != tc.wantType {
				t.Fatalf("NewModelClient(%q) type = %s, want %s", tc.provider, got, tc.wantType)
			}
		})
	}
}
