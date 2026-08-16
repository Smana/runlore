// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

const validEntry = `---
type: Incident
title: KubeContainerOOMKilled for oom-app
description: the container is OOMKilled because its memory limit is too low
resource: runlore-test/oom-app
tags:
  - runlore
  - incident
---

## Symptom

KubeContainerOOMKilled

## Investigate

- pod_status: OOMKilled

## Cause

1. memory limit too low

## Resolution

- raise the limit
`

const invalidEntry = `---
type: Incident
title: broken entry
description: missing the Cause section
resource: ns/name
tags: [runlore]
---

## Symptom

x

## Resolution

- z
`

func writeEntry(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestValidateKBValidPasses(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "ok.md", validEntry)
	var buf bytes.Buffer
	hadError, _, err := validateKB(&buf, dir, "text", nil)
	if err != nil {
		t.Fatal(err)
	}
	if hadError {
		t.Fatalf("valid entry must pass, got output:\n%s", buf.String())
	}
}

func TestValidateKBInvalidFails(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "ok.md", validEntry)
	writeEntry(t, dir, "bad.md", invalidEntry)
	var buf bytes.Buffer
	hadError, _, err := validateKB(&buf, dir, "text", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hadError {
		t.Fatalf("a missing-Cause Incident must fail the gate, got output:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "cause") {
		t.Fatalf("expected a cause issue in output, got:\n%s", buf.String())
	}
}

func TestValidateKBGitHubFormat(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "bad.md", invalidEntry)
	var buf bytes.Buffer
	if _, _, err := validateKB(&buf, dir, "github", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "::error file=bad.md::") {
		t.Fatalf("expected a GitHub error annotation, got:\n%s", buf.String())
	}
}

// countingReviewModel answers every semantic review with a valid submit_review call
// and reports a fixed usage, so a test can assert on the spend the command reports.
type countingReviewModel struct{ calls int }

func (m *countingReviewModel) Complete(context.Context, providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	return providers.CompletionResponse{
		Usage: providers.Usage{InputTokens: 1200, OutputTokens: 80},
		ToolCalls: []providers.ToolCall{{ID: "r", Name: "submit_review",
			Args: `{"cause_explains_symptom":{"ok":true,"rationale":"fits"},"durable":{"ok":true,"rationale":"recurs"}}`}},
	}, nil
}

// TestValidateKBReportsWhatTheSemanticReviewSpent pins the visibility gap:
// `lore validate-kb --semantic` makes ONE model call PER ENTRY over a whole catalog
// directory, and nothing counted them — not an investigation's usage totals, not any
// budget, not a metric. This is a one-shot CLI, so a Prometheus counter would record
// into a no-op meter and never be scraped (telemetry.Setup runs only under
// `lore serve`); what an operator can actually see is the command reporting its own
// spend. Returning the total is what makes that reportable — and testable.
func TestValidateKBReportsWhatTheSemanticReviewSpent(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "ok.md", validEntry)
	writeEntry(t, dir, "ok2.md", validEntry)

	m := &countingReviewModel{}
	var buf bytes.Buffer
	_, usage, err := validateKB(&buf, dir, "text", m)
	if err != nil {
		t.Fatal(err)
	}
	if m.calls != 2 {
		t.Fatalf("the review runs once per entry: %d calls over 2 entries", m.calls)
	}
	if want := 2 * 1200; usage.InputTokens != want {
		t.Errorf("input tokens: got %d, want %d — the semantic review's spend is not being counted",
			usage.InputTokens, want)
	}
	if want := 2 * 80; usage.OutputTokens != want {
		t.Errorf("output tokens: got %d, want %d", usage.OutputTokens, want)
	}
}

// TestValidateKBStructuralOnlyReportsNoSpend pins the control: with no model there
// is no model call, so the reported spend must be zero rather than whatever a
// leftover counter happened to hold.
func TestValidateKBStructuralOnlyReportsNoSpend(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "ok.md", validEntry)
	var buf bytes.Buffer
	_, usage, err := validateKB(&buf, dir, "text", nil)
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 0 || usage.OutputTokens != 0 {
		t.Errorf("structural-only validation makes no model calls, got %+v", usage)
	}
}
