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

// NewModelClient builds a ModelProvider for a wire protocol + endpoint.
func NewModelClient(provider, baseURL, model, apiKey string) providers.ModelProvider {
	switch provider {
	case "anthropic":
		return anthropic.New(baseURL, model, apiKey)
	case "gemini":
		return gemini.New(baseURL, model, apiKey)
	default:
		return openai.New(baseURL, model, apiKey)
	}
}

// BuildModel builds the ModelProvider for the configured provider.
func BuildModel(cfg *config.Config, apiKey string) providers.ModelProvider {
	return NewModelClient(cfg.Model.Provider, cfg.Model.BaseURL, cfg.Model.Model, apiKey)
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
		or(v.BaseURL, cfg.Model.BaseURL), or(v.Model, cfg.Model.Model), apiKey)
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
	return NewModelClient(provider, baseURL, model, os.Getenv(apiKeyEnv))
}
