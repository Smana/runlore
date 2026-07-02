package app

import (
	"fmt"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

// TestEffectiveMaxTokens locks in the output-token defaulting: an unset (0)
// model.max_tokens resolves to the 8192 default; an explicit value is used as-is.
func TestEffectiveMaxTokens(t *testing.T) {
	tests := []struct {
		name string
		in   int
		want int
	}{
		{"unset uses default", 0, defaultMaxTokens},
		{"explicit value", 16384, 16384},
		{"small explicit value", 256, 256},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := effectiveMaxTokens(tc.in); got != tc.want {
				t.Fatalf("effectiveMaxTokens(%d) = %d, want %d", tc.in, got, tc.want)
			}
		})
	}
}

// TestVerifyMaxTokensInherits verifies the verify pass inherits the parent's
// EFFECTIVE max_tokens when its own override is unset (0), but uses its own value
// when set.
func TestVerifyMaxTokensInherits(t *testing.T) {
	// Parent 16384, verify unset ⇒ verify inherits 16384.
	cfg := &config.Config{Model: config.Model{
		Provider: "anthropic", Model: "claude-x", MaxTokens: 16384,
		Verify: &config.ModelOverride{Model: "claude-cheap"},
	}}
	if got := verifyMaxTokens(cfg); got != 16384 {
		t.Fatalf("verify inherits parent effective: got %d, want 16384", got)
	}

	// Parent unset (⇒ default 8192), verify unset ⇒ verify inherits the default.
	cfgDefault := &config.Config{Model: config.Model{
		Provider: "anthropic", Model: "claude-x",
		Verify: &config.ModelOverride{Model: "claude-cheap"},
	}}
	if got := verifyMaxTokens(cfgDefault); got != defaultMaxTokens {
		t.Fatalf("verify inherits parent default: got %d, want %d", got, defaultMaxTokens)
	}

	// Verify override set ⇒ used as-is regardless of the parent.
	cfgOverride := &config.Config{Model: config.Model{
		Provider: "anthropic", Model: "claude-x", MaxTokens: 16384,
		Verify: &config.ModelOverride{Model: "claude-cheap", MaxTokens: 2048},
	}}
	if got := verifyMaxTokens(cfgOverride); got != 2048 {
		t.Fatalf("verify override: got %d, want 2048", got)
	}
}

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
			client := NewModelClient(tc.provider, "http://endpoint/v1", "test-model", "key", defaultMaxTokens, "", "")
			if client == nil {
				t.Fatalf("NewModelClient(%q) returned nil", tc.provider)
			}
			if got := fmt.Sprintf("%T", client); got != tc.wantType {
				t.Fatalf("NewModelClient(%q) type = %s, want %s", tc.provider, got, tc.wantType)
			}
		})
	}
}
