// SPDX-License-Identifier: Apache-2.0

package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/Smana/runlore/internal/config"
)

// defaultConfigPath is the config `lore investigate` looks for when --config is not
// given. Its ABSENCE is not an error: the CLI is the laptop-to-value front door, and
// requiring a YAML file before the first answer is the friction this removes.
const defaultConfigPath = "runlore.yaml"

// Env vars the zero-config path reads, in precedence order. They are the names the
// ecosystem already uses, so a user who has run any OpenAI-compatible or Anthropic
// tool already has them exported.
const (
	envOpenAIBaseURL = "OPENAI_BASE_URL"
	envOpenAIKey     = "OPENAI_API_KEY"
	envOpenAIModel   = "OPENAI_MODEL"
	envAnthropicKey  = "ANTHROPIC_API_KEY"

	// defaultOpenAIModel / defaultAnthropicModel are used when the endpoint is known
	// but the model name is not. A local vLLM/Ollama commonly serves one model, and
	// naming it wrongly fails loudly at the first call rather than silently.
	defaultOpenAIModel    = "gpt-4o-mini"
	defaultAnthropicModel = "claude-sonnet-5"
)

// resolveInvestigateConfig loads the config for a one-off investigation.
//
//	explicit=true  (--config was given): a missing file is an ERROR. Falling back
//	               would run against a different model than the user asked for.
//	explicit=false (default path): a missing file falls back to the environment.
func resolveInvestigateConfig(path string, explicit bool) (*config.Config, error) {
	cfg, err := config.Load(path)
	switch {
	case err == nil:
		return cfg, nil
	case explicit:
		return nil, err
	case !errors.Is(err, fs.ErrNotExist):
		// The file exists but is broken — never paper over that with env guesses.
		return nil, err
	}
	return configFromEnv()
}

// configFromEnv synthesizes a minimal, model-only config from the environment. Every
// data source stays unset, so each disables its own tool — no cluster, no Flux, no
// KB repo, no forge and no notifier are required to get an answer.
func configFromEnv() (*config.Config, error) {
	switch {
	case os.Getenv(envOpenAIBaseURL) != "":
		m := config.Model{
			BaseURL: os.Getenv(envOpenAIBaseURL),
			Model:   or(os.Getenv(envOpenAIModel), defaultOpenAIModel),
		}
		// Keyless is a first-class case here: an in-cluster vLLM or a local Ollama
		// needs no credential, and demanding one would break the most private setup.
		if os.Getenv(envOpenAIKey) != "" {
			m.APIKeyEnv = envOpenAIKey
		}
		return &config.Config{Model: m}, nil
	case os.Getenv(envAnthropicKey) != "":
		return &config.Config{Model: config.Model{
			Provider:  "anthropic",
			Model:     defaultAnthropicModel,
			APIKeyEnv: envAnthropicKey,
		}}, nil
	default:
		return nil, fmt.Errorf(
			"no model configured: set %s (+ optional %s / %s) for an OpenAI-compatible endpoint, "+
				"or %s for Anthropic — or write a %s and pass it with --config",
			envOpenAIBaseURL, envOpenAIKey, envOpenAIModel, envAnthropicKey, defaultConfigPath)
	}
}

// or returns a when non-empty, else b.
func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
