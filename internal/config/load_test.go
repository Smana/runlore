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
sources:
  alertmanager: {}
  gitops: { enabled: true }
triggers:
  incidents:
    match:
      severity: [critical]
      environment: [prod]
      namespaces: ["apps*"]
      labels: { team: platform }
    ignore:
      alertnames: [Watchdog]
    dedup: { window: 30m }
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
	if _, ok := c.Sources["alertmanager"]; !ok {
		t.Fatal("sources.alertmanager should be present (alertmanager enabled)")
	}
	if _, ok := c.Sources["gitops"]; !ok {
		t.Fatal("sources.gitops should be present")
	}
	if c.Triggers.Incidents.Dedup.Window.Std() != 30*time.Minute {
		t.Fatalf("window: got %v", c.Triggers.Incidents.Dedup.Window.Std())
	}
	if c.Actions.Enabled() {
		t.Fatal("actions mode off should be disabled")
	}
	// Debounce default (60s) is applied unconditionally when the window is unset.
	if c.Triggers.GitOpsFailures.Debounce.Std() != 60*time.Second {
		t.Fatalf("gitops_failures debounce default: got %v, want 60s", c.Triggers.GitOpsFailures.Debounce.Std())
	}
}

func TestLoadGitOpsFailureDebounceExplicit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runlore.yaml")
	doc := `
sources:
  gitops: { enabled: true }
triggers:
  gitops_failures:
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

func TestLoadEndpointAuth(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runlore.yaml")
	// Both backends carry optional auth: a bearer token (by env-var name) and static
	// headers. The strict KnownFields(true) decoder must accept token_env/headers.
	doc := `
metrics:
  url: https://vm.example.com
  token_env: VM_TOKEN
  headers:
    X-Scope-OrgID: tenant-a
logs:
  url: https://vl.example.com
  token_env: VL_TOKEN
  headers:
    X-Scope-OrgID: tenant-b
    X-Extra: v
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Metrics.TokenEnv != "VM_TOKEN" {
		t.Fatalf("metrics.token_env: got %q, want VM_TOKEN", c.Metrics.TokenEnv)
	}
	if c.Metrics.Headers["X-Scope-OrgID"] != "tenant-a" {
		t.Fatalf("metrics.headers[X-Scope-OrgID]: got %q, want tenant-a", c.Metrics.Headers["X-Scope-OrgID"])
	}
	if c.Logs.TokenEnv != "VL_TOKEN" {
		t.Fatalf("logs.token_env: got %q, want VL_TOKEN", c.Logs.TokenEnv)
	}
	if c.Logs.Headers["X-Scope-OrgID"] != "tenant-b" || c.Logs.Headers["X-Extra"] != "v" {
		t.Fatalf("logs.headers: got %v", c.Logs.Headers)
	}
}

func TestLoadEndpointAuthOptional(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runlore.yaml")
	// URL-only endpoints (the pre-auth shape) must still decode, with empty auth.
	doc := `
metrics:
  url: https://vm.example.com
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Metrics.URL != "https://vm.example.com" {
		t.Fatalf("metrics.url: got %q", c.Metrics.URL)
	}
	if c.Metrics.TokenEnv != "" || len(c.Metrics.Headers) != 0 {
		t.Fatalf("url-only endpoint should have empty auth, got token_env=%q headers=%v", c.Metrics.TokenEnv, c.Metrics.Headers)
	}
}

func TestLoadGitOpsFailureDebounceZeroFiresImmediately(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runlore.yaml")
	doc := `
sources:
  gitops: { enabled: true }
triggers:
  gitops_failures:
    debounce: 0
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// An explicit `debounce: 0` must survive applyDefaults (not be clobbered to the
	// 60s default for an unset window), so the trigger fires immediately.
	if c.Triggers.GitOpsFailures.Debounce == nil {
		t.Fatal("explicit debounce:0 should be non-nil (distinguishable from unset)")
	}
	if c.Triggers.GitOpsFailures.DebounceWindow() != 0 {
		t.Fatalf("explicit debounce:0 should fire immediately, got %v", c.Triggers.GitOpsFailures.DebounceWindow())
	}
}
