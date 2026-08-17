// SPDX-License-Identifier: Apache-2.0

// Package app holds the dependency-injection builders and config predicates that
// assemble the RunLore agent. They live here (instead of in cmd/lore behind
// main()) so the wiring that decides what ships is unit-testable.
package app

import (
	"cmp"
	"os"

	"github.com/Smana/runlore/internal/config"
	anthropic "github.com/Smana/runlore/internal/model/anthropic"
	gemini "github.com/Smana/runlore/internal/model/gemini"
	openai "github.com/Smana/runlore/internal/model/openai"
	"github.com/Smana/runlore/internal/providers"
)

// defaultMaxTokens is the output-token ceiling used when model.max_tokens is unset
// (0). It bounds a single completion's generated tokens across every provider.
const defaultMaxTokens = 8192

// effectiveMaxTokens resolves a configured max_tokens to the value sent on the wire:
// an unset (0) value becomes the defaultMaxTokens; an explicit value is used as-is.
func effectiveMaxTokens(configured int) int {
	if configured <= 0 {
		return defaultMaxTokens
	}
	return configured
}

// verifyMaxTokens resolves the verify pass's effective output-token cap: its own
// override when set (>0), otherwise the parent model's effective value (so a bare
// `verify: {model: <cheap>}` inherits the parent's cap, defaulted or explicit).
func verifyMaxTokens(cfg *config.Config) int {
	parent := effectiveMaxTokens(cfg.Model.MaxTokens)
	if v := cfg.Model.Verify; v != nil && v.MaxTokens > 0 {
		return v.MaxTokens
	}
	return parent
}

// chatDefaultMaxTokens bounds a chat reply's generated tokens when
// model.chat.max_tokens is left unset. A thread reply is a sentence or two, not
// an investigation, so this is deliberately NOT the (possibly much larger)
// investigation ceiling: unlike verifyMaxTokens, which inherits the parent's
// effective cap, chatMaxTokens falls back to this fixed, small default instead —
// a member-triggerable path staying cheap must not depend on how generously the
// investigation model happens to be capped. 1024 tokens is well over an order of
// magnitude of headroom for a short conversational answer.
const chatDefaultMaxTokens = 1024

// chatMaxTokens resolves model.chat's effective output-token cap: its own
// override when set (>0), otherwise chatDefaultMaxTokens.
func chatMaxTokens(cfg *config.Config) int {
	if c := cfg.Model.Chat; c != nil && c.MaxTokens > 0 {
		return c.MaxTokens
	}
	return chatDefaultMaxTokens
}

// NewModelClient builds a ModelProvider for a wire protocol + endpoint. maxTokens
// is the per-request output-token ceiling passed through to the provider. effort
// is the opt-in reasoning knob (config-validated per provider; "" = omitted);
// gemini does not take it — config.Validate rejects effort for that provider.
// thinking is the opt-in adaptive-thinking knob (anthropic-only, config-validated;
// "" = omitted); only the anthropic client consumes it.
func NewModelClient(provider, baseURL, model, apiKey string, maxTokens int, effort, thinking string) providers.ModelProvider {
	switch provider {
	case "anthropic":
		return anthropic.New(baseURL, model, apiKey, maxTokens, effort, thinking)
	case "gemini":
		return gemini.New(baseURL, model, apiKey, maxTokens)
	default:
		return openai.New(baseURL, model, apiKey, maxTokens, effort)
	}
}

// BuildModel builds the ModelProvider for the configured provider, applying the
// effective output-token cap (model.max_tokens, defaulted when unset) and the
// opt-in effort and thinking knobs.
func BuildModel(cfg *config.Config, apiKey string) providers.ModelProvider {
	return NewModelClient(cfg.Model.Provider, cfg.Model.BaseURL, cfg.Model.Model, apiKey,
		effectiveMaxTokens(cfg.Model.MaxTokens), cfg.Model.Effort, cfg.Model.Thinking)
}

// BuildVerifyModel builds the optional cheaper model for the adversarial verify
// pass, inheriting any unset field from the main model. Returns nil when no
// model.verify override is configured (verify then runs on the main model).
func BuildVerifyModel(cfg *config.Config) providers.ModelProvider {
	v := cfg.Model.Verify
	if v == nil {
		return nil
	}
	// os.Getenv("") is "", so an unset key env on both sides is the keyless case.
	apiKey := os.Getenv(cmp.Or(v.APIKeyEnv, cfg.Model.APIKeyEnv))
	return NewModelClient(cmp.Or(v.Provider, cfg.Model.Provider),
		cmp.Or(v.BaseURL, cfg.Model.BaseURL), cmp.Or(v.Model, cfg.Model.Model), apiKey,
		verifyMaxTokens(cfg), cmp.Or(v.Effort, cfg.Model.Effort), cmp.Or(v.Thinking, cfg.Model.Thinking))
}

// BuildChatModel builds the thread-conversation model from model.chat, or nil
// when the block is absent — the presence of the block IS the feature switch,
// the same contract BuildVerifyModel follows for model.verify.
//
// Unlike verify, config.Validate requires model.chat.model to be named
// explicitly, so this never silently resolves to the investigation model.
func BuildChatModel(cfg *config.Config) providers.ModelProvider {
	c := cfg.Model.Chat
	if c == nil {
		return nil
	}
	apiKey := os.Getenv(cmp.Or(c.APIKeyEnv, cfg.Model.APIKeyEnv))
	return NewModelClient(cmp.Or(c.Provider, cfg.Model.Provider),
		cmp.Or(c.BaseURL, cfg.Model.BaseURL), c.Model, apiKey,
		chatMaxTokens(cfg), cmp.Or(c.Effort, cfg.Model.Effort), cmp.Or(c.Thinking, cfg.Model.Thinking))
}

// BuildJudgeModel builds the (stronger) grader model from --judge-* flags, falling
// back to the configured investigation model when unset.
func BuildJudgeModel(cfg *config.Config, provider, baseURL, model, apiKeyEnv string) providers.ModelProvider {
	if provider == "" && model == "" {
		apiKey := ""
		if cfg.Model.APIKeyEnv != "" {
			apiKey = os.Getenv(cfg.Model.APIKeyEnv)
		}
		return BuildModel(cfg, apiKey)
	}
	// A judge gets the same effective output cap as the main model (no separate
	// knob), but NOT the configured effort or thinking: those were validated against
	// the configured provider, and a --judge-* flag set may target a different one.
	return NewModelClient(provider, baseURL, model, os.Getenv(apiKeyEnv), effectiveMaxTokens(cfg.Model.MaxTokens), "", "")
}
