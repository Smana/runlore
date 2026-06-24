package config

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestLoad(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runlore.yaml")
	doc := `
triggers:
  incidents:
    enabled: true
    match:
      severity: [critical]
      environment: [prod]
      namespaces: ["apps*"]
      labels: { team: platform }
    ignore:
      alertnames: [Watchdog]
    dedup: { window: 30m }
  gitops_failures: { enabled: true }
actions:
  mode: off
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if !c.Triggers.Incidents.Enabled {
		t.Fatal("incidents should be enabled")
	}
	if c.Triggers.Incidents.Dedup.Window.Std() != 30*time.Minute {
		t.Fatalf("window: got %v", c.Triggers.Incidents.Dedup.Window.Std())
	}
	if c.Actions.Enabled() {
		t.Fatal("actions mode off should be disabled")
	}
	// gitops_failures enabled without an explicit debounce → 60s default applied.
	if !c.Triggers.GitOpsFailures.Enabled {
		t.Fatal("gitops_failures should be enabled")
	}
	if c.Triggers.GitOpsFailures.Debounce.Std() != 60*time.Second {
		t.Fatalf("gitops_failures debounce default: got %v, want 60s", c.Triggers.GitOpsFailures.Debounce.Std())
	}
}

func TestLoadGitOpsFailureDebounceExplicit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runlore.yaml")
	doc := `
triggers:
  gitops_failures:
    enabled: true
    debounce: 2m
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Triggers.GitOpsFailures.Debounce.Std() != 2*time.Minute {
		t.Fatalf("explicit debounce: got %v, want 2m", c.Triggers.GitOpsFailures.Debounce.Std())
	}
}
