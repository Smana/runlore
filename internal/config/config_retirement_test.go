// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
)

func TestApplyDefaultsRetirementEnabled(t *testing.T) {
	// Only retirement.enabled set — the three tuning knobs get the recall-gate defaults.
	var c Config
	c.Curate.Retirement.Enabled = true
	applyDefaults(&c)
	r := c.Curate.Retirement
	if r.MinObservations != 3 {
		t.Errorf("default MinObservations: got %d, want 3", r.MinObservations)
	}
	if r.Floor != 0.5 {
		t.Errorf("default Floor: got %g, want 0.5", r.Floor)
	}
	if r.Prior != 2.0 {
		t.Errorf("default Prior: got %g, want 2.0", r.Prior)
	}

	// Explicit values are respected, not overwritten.
	var c2 Config
	c2.Curate.Retirement = Retirement{Enabled: true, MinObservations: 5, Floor: 0.3, Prior: 4.0}
	applyDefaults(&c2)
	if r2 := c2.Curate.Retirement; r2.MinObservations != 5 || r2.Floor != 0.3 || r2.Prior != 4.0 {
		t.Fatalf("explicit retirement tuning overwritten: %+v", r2)
	}
}

func TestApplyDefaultsRetirementDisabled(t *testing.T) {
	// Disabled (the default): the pass is opt-in, so no defaults are filled — the
	// block stays at its zero value and never wires the pass in.
	var c Config
	applyDefaults(&c)
	if r := c.Curate.Retirement; r.MinObservations != 0 || r.Floor != 0 || r.Prior != 0 {
		t.Fatalf("defaults must not be applied while retirement is disabled: %+v", r)
	}
}

func TestValidateRetirement(t *testing.T) {
	valid := func() *Config {
		c := &Config{}
		c.Curate.Retirement = Retirement{Enabled: true, MinObservations: 3, Floor: 0.5, Prior: 2.0}
		return c
	}
	if err := valid().Validate(); err != nil {
		t.Fatalf("a valid retirement config must pass: %v", err)
	}

	floorHigh := valid()
	floorHigh.Curate.Retirement.Floor = 1.5
	if err := floorHigh.Validate(); err == nil || !strings.Contains(err.Error(), "curate.retirement.floor") {
		t.Fatalf("floor > 1 must error on curate.retirement.floor, got %v", err)
	}

	floorLow := valid()
	floorLow.Curate.Retirement.Floor = -0.1
	if err := floorLow.Validate(); err == nil || !strings.Contains(err.Error(), "curate.retirement.floor") {
		t.Fatalf("floor <= 0 must error on curate.retirement.floor, got %v", err)
	}

	minObs := valid()
	minObs.Curate.Retirement.MinObservations = -1
	if err := minObs.Validate(); err == nil || !strings.Contains(err.Error(), "curate.retirement.min_observations") {
		t.Fatalf("min_observations < 1 must error, got %v", err)
	}

	// Disabled: an opt-out config is never validated, however nonsensical its knobs.
	off := valid()
	off.Curate.Retirement = Retirement{Enabled: false, Floor: 99, MinObservations: -5}
	if err := off.Validate(); err != nil {
		t.Fatalf("a disabled retirement block must not be validated: %v", err)
	}
}
