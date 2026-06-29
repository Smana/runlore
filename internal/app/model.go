// Package app holds the dependency-injection builders and config predicates that
// assemble the RunLore agent. They live here (instead of in cmd/lore behind
// main()) so the wiring that decides what ships is unit-testable.
package app

import (
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

// NewModelClient builds a ModelProvider for a wire protocol + endpoint. maxTokens
// is the per-request output-token ceiling passed through to the provider.
func NewModelClient(provider, baseURL, model, apiKey string, maxTokens int) providers.ModelProvider {
	switch provider {
	case "anthropic":
		return anthropic.New(baseURL, model, apiKey, maxTokens)
	case "gemini":
		return gemini.New(baseURL, model, apiKey, maxTokens)
	default:
		return openai.New(baseURL, model, apiKey, maxTokens)
	}
}

// BuildModel builds the ModelProvider for the configured provider, applying the
// effective output-token cap (model.max_tokens, defaulted when unset).
func BuildModel(cfg *config.Config, apiKey string) providers.ModelProvider {
	return NewModelClient(cfg.Model.Provider, cfg.Model.BaseURL, cfg.Model.Model, apiKey, effectiveMaxTokens(cfg.Model.MaxTokens))
}

// BuildVerifyModel builds the optional cheaper model for the adversarial verify
// pass, inheriting any unset field from the main model. Returns nil when no
// model.verify override is configured (verify then runs on the main model).
func BuildVerifyModel(cfg *config.Config) providers.ModelProvider {
	v := cfg.Model.Verify
	if v == nil {
		return nil
	}
	or := func(a, b string) string {
		if a != "" {
			return a
		}
		return b
	}
	apiKey := ""
	if keyEnv := or(v.APIKeyEnv, cfg.Model.APIKeyEnv); keyEnv != "" {
		apiKey = os.Getenv(keyEnv)
	}
	return NewModelClient(or(v.Provider, cfg.Model.Provider),
		or(v.BaseURL, cfg.Model.BaseURL), or(v.Model, cfg.Model.Model), apiKey, verifyMaxTokens(cfg))
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
	// A judge gets the same effective output cap as the main model (no separate knob).
	return NewModelClient(provider, baseURL, model, os.Getenv(apiKeyEnv), effectiveMaxTokens(cfg.Model.MaxTokens))
}
