// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// loadDoc writes doc to a temp runlore.yaml and Loads it — i.e. it exercises the real
// entry point, defaults included. Tests that assert a DEFAULT must go through Load:
// a bare &Config{} skips applyDefaults and would silently pin the zero value instead.
func loadDoc(t *testing.T, doc string) *Config {
	t.Helper()
	p := filepath.Join(t.TempDir(), "runlore.yaml")
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return c
}

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

// TestLoadCancelQueuedOnResolve pins the yaml key spelling and that an explicit
// `true` parses.
func TestLoadCancelQueuedOnResolve(t *testing.T) {
	c := loadDoc(t, `
sources:
  alertmanager: {}
triggers:
  incidents:
    cancel_queued_on_resolve: true
`)
	if !c.Triggers.Incidents.CancelQueuedOnResolveEnabled() {
		t.Fatal("explicit cancel_queued_on_resolve: true should parse")
	}
}

// TestLoadCancelQueuedOnResolveDefaultsTrue pins the default flip: unset ⇒ TRUE.
//
// It used to default to false, on the reasoning that the debounce hold was the
// self-resolving filter and this merely extended it. That reasoning does not survive
// the critical carve-out: the debounce deliberately never holds a CRITICAL alert (a
// debounce must never delay the first look at a critical page), and the shipped chart
// trigger matches `severity: [critical]` EXCLUSIVELY — so on a default install the
// hold filters nothing at all. Cancelling a QUEUED-but-not-yet-started investigation
// when the resolve lands is the only filter criticals get, and it costs ZERO added
// latency: nothing is ever waited on.
func TestLoadCancelQueuedOnResolveDefaultsTrue(t *testing.T) {
	c := loadDoc(t, `
sources:
  alertmanager: {}
`)
	if c.Triggers.Incidents.CancelQueuedOnResolve == nil {
		t.Fatal("unset cancel_queued_on_resolve must be defaulted (non-nil) by applyDefaults")
	}
	if !c.Triggers.Incidents.CancelQueuedOnResolveEnabled() {
		t.Fatal("cancel_queued_on_resolve must default to TRUE")
	}
}

// TestLoadCancelQueuedOnResolveExplicitFalse keeps the escape hatch honest: a team that
// wants the post-hoc "why did it fire?" investigation even after self-resolution must be
// able to say so. That requires distinguishing "unset" from "explicitly false" — hence
// the *bool, mirroring Debounce.
func TestLoadCancelQueuedOnResolveExplicitFalse(t *testing.T) {
	c := loadDoc(t, `
sources:
  alertmanager: {}
triggers:
  incidents:
    cancel_queued_on_resolve: false
`)
	if c.Triggers.Incidents.CancelQueuedOnResolve == nil {
		t.Fatal("explicit false should be non-nil (distinguishable from unset)")
	}
	if c.Triggers.Incidents.CancelQueuedOnResolveEnabled() {
		t.Fatal("an explicit cancel_queued_on_resolve: false must be honoured, not overwritten by the true default")
	}
}

// TestLoadIncidentDebounceDefault pins the incident debounce default. It was 0
// (disabled) while the GitOps-failure debounce defaulted to 60s — an asymmetry with no
// justification: both filters exist to keep a transient, self-resolving failure from
// burning a full paid investigation. The incident side matters more, because a resolve
// for a self-healed alert also lands in the outcome ledger and credits the recalled
// entry's resolve rate on evidence unrelated to the diagnosis. A default install was
// exactly that configuration.
func TestLoadIncidentDebounceDefault(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runlore.yaml")
	doc := `
sources:
  alertmanager: {}
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Triggers.Incidents.Debounce.Std() != 60*time.Second {
		t.Fatalf("incidents debounce default: got %v, want 60s", c.Triggers.Incidents.Debounce.Std())
	}
}

// TestLoadIncidentDebounceExplicitZeroDisables keeps the escape hatch honest: a
// deployment that wants the pre-debounce behaviour back (investigate on every fire,
// including self-resolving ones) must be able to say so. That requires distinguishing
// "unset" from "explicitly 0", hence the pointer — mirroring gitops_failures.debounce.
func TestLoadIncidentDebounceExplicitZeroDisables(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runlore.yaml")
	doc := `
sources:
  alertmanager: {}
triggers:
  incidents:
    debounce: 0s
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Triggers.Incidents.Debounce.Std() != 0 {
		t.Fatalf("explicit debounce 0 must disable the hold: got %v, want 0", c.Triggers.Incidents.Debounce.Std())
	}
}

// TestLoadIncidentDebounceExplicit checks an explicit window survives defaulting.
func TestLoadIncidentDebounceExplicit(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "runlore.yaml")
	doc := `
sources:
  alertmanager: {}
triggers:
  incidents:
    debounce: 5m
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Triggers.Incidents.Debounce.Std() != 5*time.Minute {
		t.Fatalf("explicit debounce: got %v, want 5m", c.Triggers.Incidents.Debounce.Std())
	}
}

// TestRateLimitDefaultsOn pins the cost-DoS default: an UNSET
// investigation.rate_limit.max_per_window defaults to 30 per 1h window. An
// unbounded default let any token-holding caller (or a misfiring Alertmanager)
// run up the model bill — per-incident cost was capped, count was not.
func TestRateLimitDefaultsOn(t *testing.T) {
	c := loadDoc(t, `
sources:
  alertmanager: {}
`)
	if c.Investigation.RateLimit.MaxPerWindow == nil {
		t.Fatal("unset max_per_window must be defaulted (non-nil) by applyDefaults")
	}
	if got := *c.Investigation.RateLimit.MaxPerWindow; got != 30 {
		t.Fatalf("unset max_per_window must default to 30, got %d", got)
	}
	if c.Investigation.RateLimit.Window != Duration(time.Hour) {
		t.Fatalf("defaulted budget must also default window to 1h, got %v", c.Investigation.RateLimit.Window)
	}
}

// TestRateLimitExplicitZeroStaysUnlimited pins backward compatibility: a deployed
// `max_per_window: 0` keeps its documented unlimited meaning after the default flip.
func TestRateLimitExplicitZeroStaysUnlimited(t *testing.T) {
	c := loadDoc(t, `
sources:
  alertmanager: {}
investigation:
  rate_limit:
    max_per_window: 0
`)
	if c.Investigation.RateLimit.MaxPerWindow == nil {
		t.Fatal("explicit 0 should be non-nil (distinguishable from unset)")
	}
	if got := *c.Investigation.RateLimit.MaxPerWindow; got != 0 {
		t.Fatalf("explicit max_per_window: 0 must stay 0 (unlimited), got %d", got)
	}
}

// TestRateLimitExplicitValueKept pins that an operator's value survives defaulting.
func TestRateLimitExplicitValueKept(t *testing.T) {
	c := loadDoc(t, `
sources:
  alertmanager: {}
investigation:
  rate_limit:
    max_per_window: 7
`)
	if got := *c.Investigation.RateLimit.MaxPerWindow; got != 7 {
		t.Fatalf("explicit max_per_window: 7 must be kept, got %d", got)
	}
}

// TestRateLimitNegativeRejected pins fail-loud validation: a negative budget is a
// misconfiguration, not a request for unlimited.
func TestRateLimitNegativeRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "runlore.yaml")
	doc := `
sources:
  alertmanager: {}
investigation:
  rate_limit:
    max_per_window: -1
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "max_per_window") {
		t.Fatalf("negative max_per_window must fail Load, got err=%v", err)
	}
}
