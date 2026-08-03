// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

// TestResolveFromOpenAIEnv: with no runlore.yaml, an OpenAI-compatible endpoint in the
// environment is enough to investigate. This is the laptop-to-value path — no config
// ceremony before the first answer.
func TestResolveFromOpenAIEnv(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("OPENAI_BASE_URL", "http://localhost:8000/v1")
	t.Setenv("OPENAI_MODEL", "qwen3-30b")
	t.Setenv("ANTHROPIC_API_KEY", "")

	cfg, err := resolveInvestigateConfig(defaultConfigPath, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Model.BaseURL != "http://localhost:8000/v1" {
		t.Errorf("base_url = %q, want the env value", cfg.Model.BaseURL)
	}
	if cfg.Model.Model != "qwen3-30b" {
		t.Errorf("model = %q, want the env value", cfg.Model.Model)
	}
	// Keyless is legitimate here: a local vLLM/Ollama needs no key.
	if cfg.Model.APIKeyEnv != "" && os.Getenv(cfg.Model.APIKeyEnv) != "" {
		t.Errorf("expected keyless, got api_key_env=%q", cfg.Model.APIKeyEnv)
	}
}

// TestResolveFromAnthropicEnv: the other zero-config path.
func TestResolveFromAnthropicEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	cfg, err := resolveInvestigateConfig(defaultConfigPath, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Model.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", cfg.Model.Provider)
	}
	if cfg.Model.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("api_key_env = %q, want ANTHROPIC_API_KEY", cfg.Model.APIKeyEnv)
	}
}

// TestResolveWithNoEnvExplainsBothPaths: the error a first-time user hits must tell
// them exactly what to set, not just that something is missing.
func TestResolveWithNoEnvExplainsBothPaths(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	_, err := resolveInvestigateConfig(defaultConfigPath, false)
	if err == nil {
		t.Fatal("expected an error with no config and no env")
	}
	for _, want := range []string{"OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "runlore.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q, got: %v", want, err)
		}
	}
}

// TestExplicitMissingConfigStillErrors: silence here would hide a typo'd --config
// path behind a surprise zero-config run against the wrong model.
func TestExplicitMissingConfigStillErrors(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	_, err := resolveInvestigateConfig(filepath.Join(t.TempDir(), "typo.yaml"), true)
	if err == nil {
		t.Fatal("an explicitly named missing config must be an error")
	}
}

// TestExistingConfigWins: a real runlore.yaml is an explicit statement of intent and
// must never be silently replaced by environment guesses.
func TestExistingConfigWins(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	if err := os.WriteFile(filepath.Join(dir, defaultConfigPath), []byte(
		"model:\n  base_url: http://from-file:8000/v1\n  model: from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := resolveInvestigateConfig(defaultConfigPath, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Model.Model != "from-file" {
		t.Errorf("model = %q, want the file's value", cfg.Model.Model)
	}
}

// TestMalformedDefaultConfigIsError: a runlore.yaml that EXISTS at the default path
// but fails to parse (here: an unknown key, rejected by config.Load's
// KnownFields(true)) must be a hard error — never papered over by falling back to
// environment synthesis. Silently synthesizing here would run the investigation
// against a guessed model while hiding the operator's broken file from them. Valid
// model env vars are set precisely so a bug that ignores the parse error and falls
// through to configFromEnv would otherwise succeed unnoticed.
func TestMalformedDefaultConfigIsError(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	if err := os.WriteFile(filepath.Join(dir, defaultConfigPath), []byte(
		"model:\n  not_a_real_field: true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := resolveInvestigateConfig(defaultConfigPath, false)
	if err == nil {
		t.Fatal("a malformed default config must be an error, not silently replaced by env synthesis")
	}
}

// TestConfigFromEnvGetsInvestigationDefaults: the synthesized (zero-config) path
// must reach config.ApplyDefaults exactly as a loaded one does. Without this, a
// zero-config investigation runs with Investigation.Timeout == 0, and
// LoopInvestigator.Investigate only applies a deadline when Timeout > 0 — i.e. no
// deadline at all, while the identical command with a minimal runlore.yaml gets the
// defaulted 10m. This test pins that the two construction paths cannot diverge.
func TestConfigFromEnvGetsInvestigationDefaults(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	envCfg, err := resolveInvestigateConfig(defaultConfigPath, false)
	if err != nil {
		t.Fatalf("resolve (env): %v", err)
	}
	if envCfg.Investigation.Timeout == 0 {
		t.Fatal("zero-config path: Investigation.Timeout must be defaulted (non-zero), got 0 (no per-investigation deadline)")
	}

	// A minimal, real runlore.yaml with the same model settings must produce the
	// SAME defaulted timeout — the two paths are not allowed to disagree.
	fileDir := t.TempDir()
	filePath := filepath.Join(fileDir, "runlore.yaml")
	if err := os.WriteFile(filePath, []byte(
		"model:\n  provider: anthropic\n  model: claude-sonnet-5\n  api_key_env: ANTHROPIC_API_KEY\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fileCfg, err := resolveInvestigateConfig(filePath, true)
	if err != nil {
		t.Fatalf("resolve (file): %v", err)
	}
	if envCfg.Investigation.Timeout != fileCfg.Investigation.Timeout {
		t.Errorf("env-synthesized timeout = %v, want the loaded-config default %v",
			envCfg.Investigation.Timeout, fileCfg.Investigation.Timeout)
	}
}

// TestDisabledToolsNamesWhatIsOff: a thin answer must be explainable. When metrics,
// logs and the catalog are unset, the command says so once on stderr rather than
// leaving the user wondering why the agent never looked at their dashboards.
func TestDisabledToolsNamesWhatIsOff(t *testing.T) {
	cfg := &config.Config{Model: config.Model{Model: "m", BaseURL: "http://x/v1"}}
	got := disabledTools(cfg)
	joined := strings.Join(got, " ")
	for _, want := range []string{"metrics", "logs", "knowledge catalog"} {
		if !strings.Contains(joined, want) {
			t.Errorf("disabledTools() = %v, must mention %q", got, want)
		}
	}
}

// TestDisabledToolsSilentWhenWired: nothing to warn about when everything is set.
// Note the shapes: config.MetricsConfig and config.LogsConfig embed config.Endpoint
// inline (config.go:116, :131, :76), so URL is set through the embedded struct.
func TestDisabledToolsSilentWhenWired(t *testing.T) {
	cfg := &config.Config{
		Model:   config.Model{Model: "m", BaseURL: "http://x/v1"},
		Metrics: config.MetricsConfig{Endpoint: config.Endpoint{URL: "http://vm:8429"}},
		Logs:    config.LogsConfig{Endpoint: config.Endpoint{URL: "http://vl:9428"}},
		Catalog: config.Catalog{Dir: "/kb"},
	}
	if got := disabledTools(cfg); len(got) != 0 {
		t.Errorf("disabledTools() = %v, want none", got)
	}
}

// TestConfigFromEnvIsValidated: ApplyDefaults and Validate are both halves of
// Load's contract, and the env path only ran the first. checkSecureKeyEndpoint
// lives in Validate, so an API key aimed at a plaintext public endpoint was
// rejected from a YAML file and accepted from the environment — sending the
// credential in cleartext over the public internet.
func TestConfigFromEnvIsValidated(t *testing.T) {
	t.Setenv(envOpenAIBaseURL, "http://gpu.public.example/v1")
	t.Setenv(envOpenAIKey, "sk-secret")
	t.Setenv(envOpenAIModel, "gpt-4o")
	if _, err := configFromEnv(); err == nil {
		t.Fatal("an API key sent to a plaintext public endpoint must be rejected on the env path, " +
			"exactly as it is when the same settings come from runlore.yaml")
	}

	// Control: the same shape over TLS is fine, so the assertion above is not just
	// "configFromEnv always errors".
	t.Setenv(envOpenAIBaseURL, "https://gpu.public.example/v1")
	if _, err := configFromEnv(); err != nil {
		t.Fatalf("a TLS endpoint must still be accepted: %v", err)
	}
}
