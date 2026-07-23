// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestSweepsDefaults(t *testing.T) {
	// Zero value ⇒ sweeps enabled in dry-run at 6h: safe-by-default, no required config.
	var c Config
	applyDefaults(&c)
	if got := c.Curate.Sweeps.Interval.Std(); got != 6*time.Hour {
		t.Fatalf("sweeps.interval default: want 6h, got %v", got)
	}
	if !c.Curate.Sweeps.Enabled() || !c.Curate.Sweeps.DryRun() {
		t.Fatalf("zero-value sweeps must be enabled in dry-run, got mode=%q", c.Curate.Sweeps.Mode)
	}
}

func TestSweepsModeParsesAndGates(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("curate:\n  sweeps:\n    mode: apply\n    interval: 1h\n"), &c); err != nil {
		t.Fatal(err)
	}
	if c.Curate.Sweeps.DryRun() || !c.Curate.Sweeps.Enabled() {
		t.Fatalf("mode: apply must be enabled and not dry-run, got %q", c.Curate.Sweeps.Mode)
	}
	if got := c.Curate.Sweeps.Interval.Std(); got != time.Hour {
		t.Fatalf("interval: want 1h, got %v", got)
	}
	var off Config
	if err := yaml.Unmarshal([]byte("curate:\n  sweeps:\n    mode: \"off\"\n"), &off); err != nil {
		t.Fatal(err)
	}
	if off.Curate.Sweeps.Enabled() {
		t.Fatal("mode: off must disable sweeps")
	}
}

func TestSweepsValidation(t *testing.T) {
	bad := Config{Curate: Curate{Sweeps: Sweeps{Mode: "sometimes"}}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "curate.sweeps.mode") {
		t.Fatalf("unknown mode must error on curate.sweeps.mode, got %v", err)
	}
	fast := Config{Curate: Curate{Sweeps: Sweeps{Interval: Duration(time.Minute)}}}
	if err := fast.Validate(); err == nil || !strings.Contains(err.Error(), "curate.sweeps.interval") {
		t.Fatalf("sub-10m interval must error on curate.sweeps.interval, got %v", err)
	}
}
