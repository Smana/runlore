// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"fmt"
	"os"

	"github.com/Smana/runlore/internal/config"
	"gopkg.in/yaml.v3"
)

// CompareSpec is the multi-model benchmark description: the model entries to
// benchmark against the replay suite, and (optionally) the judge that grades
// every entry. Keeping the judge in the spec makes a published comparison
// self-describing — the judge disclosure travels with the results.
type CompareSpec struct {
	Judge  *JudgeSpec   `yaml:"judge,omitempty"`
	Models []ModelEntry `yaml:"models"`
}

// JudgeSpec identifies the (single, fixed) judge model used for every entry.
// Grading is blind: the judge never sees which entry produced an investigation.
type JudgeSpec struct {
	Provider  string `yaml:"provider"`
	BaseURL   string `yaml:"base_url"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env"`
}

// ModelEntry is one model configuration to benchmark.
type ModelEntry struct {
	Name      string `yaml:"name"`     // report label; must be unique
	Provider  string `yaml:"provider"` // "openai" (default) | "anthropic" | "gemini"
	BaseURL   string `yaml:"base_url"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env"` // env var holding the API key (empty = keyless)
	// Effort is the OpenAI-compatible reasoning_effort request field (e.g. "low",
	// "medium", "high"). Only valid for the openai provider; other providers reject it.
	Effort string `yaml:"effort,omitempty"`
	// Prices enables the estimated-cost column; omit it to omit the column.
	Prices *Prices `yaml:"prices,omitempty"`
}

// Prices is optional per-million-token (MTok) pricing for cost estimation.
type Prices struct {
	InputUSD  float64 `yaml:"input_usd" json:"input_usd"`
	OutputUSD float64 `yaml:"output_usd" json:"output_usd"`
}

// LoadCompareSpec reads and validates a comparison spec. Unknown keys are
// rejected so a typo (e.g. "pricess") fails loudly instead of silently skewing
// a published benchmark.
func LoadCompareSpec(path string) (CompareSpec, error) {
	f, err := os.Open(path) //nolint:gosec // G304: path is the operator-supplied compare file
	if err != nil {
		return CompareSpec{}, fmt.Errorf("open compare spec: %w", err)
	}
	defer func() { _ = f.Close() }()
	var spec CompareSpec
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&spec); err != nil {
		return CompareSpec{}, fmt.Errorf("parse compare spec %s: %w", path, err)
	}
	if err := spec.validate(); err != nil {
		return CompareSpec{}, fmt.Errorf("invalid compare spec %s: %w", path, err)
	}
	return spec, nil
}

func (s CompareSpec) validate() error {
	if len(s.Models) == 0 {
		return fmt.Errorf("models: at least one entry is required")
	}
	seen := map[string]bool{}
	for i, e := range s.Models {
		if e.Name == "" {
			return fmt.Errorf("models[%d]: name is required", i)
		}
		if e.Model == "" {
			return fmt.Errorf("models[%d] (%s): model is required", i, e.Name)
		}
		if seen[e.Name] {
			return fmt.Errorf("models[%d]: duplicate name %q", i, e.Name)
		}
		seen[e.Name] = true
		// Effort is validated against the provider it will actually be sent to, using
		// config's single source of truth (anthropic low|medium|high|max, openai
		// minimal|low|medium|high, rejected for gemini). An empty provider defaults to
		// the OpenAI-compatible wire protocol, mirroring NewModelClient.
		field := fmt.Sprintf("models[%d] (%s).effort", i, e.Name)
		if err := config.ValidateEffort(field, e.Provider, e.Effort); err != nil {
			return err
		}
	}
	if s.Judge != nil && s.Judge.Model == "" {
		return fmt.Errorf("judge: model is required when a judge block is present")
	}
	return nil
}
