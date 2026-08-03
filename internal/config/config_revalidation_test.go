// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"
)

func TestApplyDefaultsRevalidationEnabled(t *testing.T) {
	// Only revalidation.enabled set — every tuning knob gets its default, and the
	// decay knobs mirror recall's outcome gate (and retirement's) so all three agree.
	var c Config
	c.Curate.Revalidation.Enabled = true
	ApplyDefaults(&c)
	r := c.Curate.Revalidation
	if r.MinInterval.Std() != 720*time.Hour {
		t.Errorf("default MinInterval: got %v, want 720h", r.MinInterval.Std())
	}
	if r.MaxOpen != 5 {
		t.Errorf("default MaxOpen: got %d, want 5", r.MaxOpen)
	}
	if r.Floor != 0.5 {
		t.Errorf("default Floor: got %g, want 0.5", r.Floor)
	}
	if r.Prior != 2.0 {
		t.Errorf("default Prior: got %g, want 2.0", r.Prior)
	}

	// Explicit values are respected, not overwritten.
	var c2 Config
	c2.Curate.Revalidation = Revalidation{
		Enabled: true, MinInterval: Duration(48 * time.Hour), MaxOpen: 1, Floor: 0.3, Prior: 4.0,
	}
	ApplyDefaults(&c2)
	if r2 := c2.Curate.Revalidation; r2.MinInterval.Std() != 48*time.Hour || r2.MaxOpen != 1 || r2.Floor != 0.3 || r2.Prior != 4.0 {
		t.Fatalf("explicit revalidation tuning overwritten: %+v", r2)
	}
}

func TestApplyDefaultsRevalidationDisabled(t *testing.T) {
	// Disabled (the default): the pass is opt-in, so no defaults are filled — the
	// block stays at its zero value and never wires the pass in.
	var c Config
	ApplyDefaults(&c)
	if r := c.Curate.Revalidation; r.MinInterval != 0 || r.MaxOpen != 0 || r.Floor != 0 || r.Prior != 0 {
		t.Fatalf("defaults must not be applied while revalidation is disabled: %+v", r)
	}
}

func TestValidateRevalidation(t *testing.T) {
	valid := func() *Config {
		c := &Config{}
		c.Curate.Revalidation = Revalidation{
			Enabled: true, MinInterval: Duration(720 * time.Hour), MaxOpen: 5, Floor: 0.5, Prior: 2.0,
		}
		return c
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("a valid revalidation config must pass: %v", err)
	}

	cases := []struct {
		name string
		set  func(*Revalidation)
		want string
	}{
		{"floor above 1", func(r *Revalidation) { r.Floor = 1.5 }, "curate.revalidation.floor"},
		{"floor at or below 0", func(r *Revalidation) { r.Floor = -0.1 }, "curate.revalidation.floor"},
		{"negative interval", func(r *Revalidation) { r.MinInterval = Duration(-time.Hour) }, "curate.revalidation.min_interval"},
		{"max_open below 1", func(r *Revalidation) { r.MaxOpen = 0 }, "curate.revalidation.max_open"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := valid()
			tc.set(&c.Curate.Revalidation)
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error naming %s, got %v", tc.want, err)
			}
		})
	}

	// Disabled: an opt-out config is never validated, however nonsensical its knobs.
	off := valid()
	off.Curate.Revalidation = Revalidation{Enabled: false, Floor: 99, MaxOpen: -5}
	if err := off.Validate(); err != nil {
		t.Fatalf("a disabled revalidation block must not be validated: %v", err)
	}
}

// TestRevalidationLoadsFromYAML pins the YAML key names — the pass is configured
// entirely through them, and a renamed field would silently leave it disabled.
func TestRevalidationLoadsFromYAML(t *testing.T) {
	c := loadDoc(t, `
curate:
  revalidation:
    enabled: true
    min_interval: 168h
    max_open: 3
    floor: 0.6
    prior: 3.0
`)
	r := c.Curate.Revalidation
	if !r.Enabled || r.MinInterval.Std() != 168*time.Hour || r.MaxOpen != 3 || r.Floor != 0.6 || r.Prior != 3.0 {
		t.Fatalf("revalidation block not decoded: %+v", r)
	}
}
