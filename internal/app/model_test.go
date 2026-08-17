// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"testing"

	"github.com/Smana/runlore/internal/config"
	anthropic "github.com/Smana/runlore/internal/model/anthropic"
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

// TestBuildChatModel mirrors BuildVerifyModel's nil-means-off contract for
// model.chat: an absent block returns nil (the block's presence IS the feature
// switch), and a configured block returns a non-nil provider whose unset fields
// (provider, base_url, api_key_env) inherit from the parent model — the same
// cmp.Or inheritance BuildVerifyModel uses — while the chat-specific max_tokens
// ceiling (chatMaxTokens, not the parent's) is applied.
func TestBuildChatModel(t *testing.T) {
	off := &config.Config{Model: config.Model{Provider: "anthropic", Model: "claude-big"}}
	if got := BuildChatModel(off); got != nil {
		t.Fatalf("BuildChatModel with no model.chat block = %v, want nil (feature off)", got)
	}

	t.Setenv("PARENT_KEY", "shh")
	cfg := &config.Config{Model: config.Model{
		Provider:  "anthropic",
		BaseURL:   "https://parent.example/v1",
		Model:     "claude-big",
		APIKeyEnv: "PARENT_KEY",
		Chat:      &config.ModelOverride{Model: "claude-cheap"}, // everything else inherited
	}}
	got := BuildChatModel(cfg)
	client, ok := got.(*anthropic.Client)
	if !ok {
		t.Fatalf("BuildChatModel returned %T, want *anthropic.Client (provider inherited)", got)
	}
	if client.BaseURL != "https://parent.example/v1" {
		t.Fatalf("base_url not inherited: got %q", client.BaseURL)
	}
	if client.Model != "claude-cheap" {
		t.Fatalf("chat's own model must be used, got %q", client.Model)
	}
	if client.APIKey != "shh" {
		t.Fatalf("api_key_env not inherited: got %q", client.APIKey)
	}
	if client.MaxTokens != chatDefaultMaxTokens {
		t.Fatalf("max_tokens: got %d, want the chat default %d", client.MaxTokens, chatDefaultMaxTokens)
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
