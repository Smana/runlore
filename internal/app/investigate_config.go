// SPDX-License-Identifier: Apache-2.0

package app

import (
	"cmp"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/Smana/runlore/internal/config"
)

// defaultConfigPath is the config `lore investigate` looks for when --config is not
// given. Its ABSENCE is not an error: the CLI is the laptop-to-value front door, and
// requiring a YAML file before the first answer is the friction this removes.
const defaultConfigPath = "runlore.yaml"

// Env vars the zero-config path reads, in precedence order. They are the names the
// ecosystem already uses, so a user who has run any OpenAI-compatible, Anthropic,
// or Gemini tool already has them exported.
const (
	envOpenAIBaseURL  = "OPENAI_BASE_URL"
	envOpenAIKey      = "OPENAI_API_KEY"
	envOpenAIModel    = "OPENAI_MODEL"
	envAnthropicKey   = "ANTHROPIC_API_KEY"
	envAnthropicModel = "ANTHROPIC_MODEL"
	envGeminiKey      = "GEMINI_API_KEY"
	envGeminiModel    = "GEMINI_MODEL"

	// Default models and endpoints used when a provider credential is present
	// without an explicit model override.
	defaultOpenAIModel    = "gpt-4o-mini"
	defaultOpenAIEndpoint = "https://api.openai.com/v1"
	defaultAnthropicModel = "claude-sonnet-5"
	defaultGeminiModel    = "gemini-4-neon"
)

// resolveInvestigateConfig loads the config for a one-off investigation and returns
// both the resolved config and the source it came from (the config file path or the
// environment variable that selected the provider).
//
//	explicit=true  (--config was given): a missing file is an ERROR. Falling back
//	               would run against a different model than the user asked for.
//	explicit=false (default path): a missing file falls back to the environment.
func resolveInvestigateConfig(path string, explicit bool, providerOverride string) (*config.Config, string, error) {
	cfg, err := config.Load(path)
	switch {
	case err == nil:
		return cfg, path, nil
	case explicit:
		return nil, "", err
	case !errors.Is(err, fs.ErrNotExist):
		// The file exists but is broken — never paper over that with env guesses.
		return nil, "", err
	}
	return configFromEnv(providerOverride)
}

// configFromEnv synthesizes a minimal, model-only config from the environment. Every
// data source stays unset, so each disables its own tool — no cluster, no Flux, no
// KB repo, no forge and no notifier are required to get an answer.
//
// When providerOverride is empty, providers are checked in deterministic order:
// OPENAI_BASE_URL → ANTHROPIC_API_KEY → GEMINI_API_KEY → bare OPENAI_API_KEY.
// When providerOverride is set ("openai", "anthropic", or "gemini"), that provider's
// branch is selected directly and a missing credential returns an explicit error.
//
// The returned config is run through config.ApplyDefaults and Validate before it is
// handed back, same as one read by config.Load — otherwise fields like
// Investigation.Timeout would stay at their zero value and cleartext key endpoints
// would bypass checkSecureKeyEndpoint.
func configFromEnv(providerOverride string) (*config.Config, string, error) {
	var (
		cfg    *config.Config
		source string
	)
	providerOverride = strings.ToLower(strings.TrimSpace(providerOverride))
	switch providerOverride {
	case "":
		switch {
		case os.Getenv(envOpenAIBaseURL) != "":
			cfg, source = openAICompatibleConfigFromEnv(), envOpenAIBaseURL
		case os.Getenv(envAnthropicKey) != "":
			cfg, source = anthropicConfigFromEnv(), envAnthropicKey
		case os.Getenv(envGeminiKey) != "":
			cfg, source = geminiConfigFromEnv(), envGeminiKey
		case os.Getenv(envOpenAIKey) != "":
			cfg, source = bareOpenAIConfigFromEnv(), envOpenAIKey
		default:
			return nil, "", fmt.Errorf(
				"no model configured: set %s (+ optional %s / %s) for an OpenAI-compatible endpoint, "+
					"%s for Anthropic, %s for Gemini, or %s for OpenAI — or write a %s and pass it with --config",
				envOpenAIBaseURL, envOpenAIKey, envOpenAIModel,
				envAnthropicKey, envGeminiKey, envOpenAIKey, defaultConfigPath)
		}
	case "openai":
		switch {
		case os.Getenv(envOpenAIBaseURL) != "":
			cfg, source = openAICompatibleConfigFromEnv(), envOpenAIBaseURL
		case os.Getenv(envOpenAIKey) != "":
			cfg, source = bareOpenAIConfigFromEnv(), envOpenAIKey
		default:
			return nil, "", fmt.Errorf("--provider openai requires %s or %s to be set", envOpenAIBaseURL, envOpenAIKey)
		}
	case "anthropic":
		if os.Getenv(envAnthropicKey) == "" {
			return nil, "", fmt.Errorf("--provider anthropic requires %s to be set", envAnthropicKey)
		}
		cfg, source = anthropicConfigFromEnv(), envAnthropicKey
	case "gemini":
		if os.Getenv(envGeminiKey) == "" {
			return nil, "", fmt.Errorf("--provider gemini requires %s to be set", envGeminiKey)
		}
		cfg, source = geminiConfigFromEnv(), envGeminiKey
	default:
		return nil, "", fmt.Errorf("unknown --provider %q (want openai, anthropic, or gemini)", providerOverride)
	}
	config.ApplyDefaults(cfg)
	if err := cfg.Validate(); err != nil {
		return nil, "", fmt.Errorf("environment-derived config is invalid: %w", err)
	}
	return cfg, source, nil
}

func openAICompatibleConfigFromEnv() *config.Config {
	m := config.Model{
		BaseURL: os.Getenv(envOpenAIBaseURL),
		Model:   cmp.Or(os.Getenv(envOpenAIModel), defaultOpenAIModel),
	}
	// Keyless is a first-class case here: an in-cluster vLLM or a local Ollama
	// needs no credential, and demanding one would break the most private setup.
	if os.Getenv(envOpenAIKey) != "" {
		m.APIKeyEnv = envOpenAIKey
	}
	return &config.Config{Model: m}
}

func bareOpenAIConfigFromEnv() *config.Config {
	return &config.Config{Model: config.Model{
		Provider:  "openai",
		BaseURL:   defaultOpenAIEndpoint,
		Model:     cmp.Or(os.Getenv(envOpenAIModel), defaultOpenAIModel),
		APIKeyEnv: envOpenAIKey,
	}}
}

func anthropicConfigFromEnv() *config.Config {
	return &config.Config{Model: config.Model{
		Provider:  "anthropic",
		Model:     cmp.Or(os.Getenv(envAnthropicModel), defaultAnthropicModel),
		APIKeyEnv: envAnthropicKey,
	}}
}

func geminiConfigFromEnv() *config.Config {
	return &config.Config{Model: config.Model{
		Provider:  "gemini",
		Model:     cmp.Or(os.Getenv(envGeminiModel), defaultGeminiModel),
		APIKeyEnv: envGeminiKey,
	}}
}
