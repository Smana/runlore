package config

import (
	"os"
	"path/filepath"
	"testing"

	"gopkg.in/yaml.v3"
)

// TestMinimalValuesConfigPassesStrictLoader pins the quickstart template to the
// real config schema: the `config:` block of deploy/helm/runlore/values-minimal.yaml
// must survive the strict (KnownFields) loader AND Validate. The chart renders that
// block verbatim into the agent's config file, so a schema change that breaks the
// quickstart fails here instead of on a new user's first install.
func TestMinimalValuesConfigPassesStrictLoader(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/helm/runlore/values-minimal.yaml")
	if err != nil {
		t.Fatalf("read values-minimal.yaml: %v", err)
	}
	var v struct {
		Config map[string]any `yaml:"config"`
	}
	if err := yaml.Unmarshal(raw, &v); err != nil {
		t.Fatalf("values yaml: %v", err)
	}
	if len(v.Config) == 0 {
		t.Fatal("values-minimal.yaml carries no config block")
	}
	blob, err := yaml.Marshal(v.Config)
	if err != nil {
		t.Fatalf("marshal config block: %v", err)
	}
	p := filepath.Join(t.TempDir(), "c.yaml")
	if err := os.WriteFile(p, blob, 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("strict Load rejected the quickstart config: %v", err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatalf("Validate rejected the quickstart config: %v", err)
	}
}
