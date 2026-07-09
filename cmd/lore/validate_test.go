// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
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
	hadError, err := validateKB(&buf, dir, "text", nil)
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
	hadError, err := validateKB(&buf, dir, "text", nil)
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
	if _, err := validateKB(&buf, dir, "github", nil); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "::error file=bad.md::") {
		t.Fatalf("expected a GitHub error annotation, got:\n%s", buf.String())
	}
}
