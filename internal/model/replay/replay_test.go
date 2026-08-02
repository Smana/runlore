// SPDX-License-Identifier: Apache-2.0

package replay_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/model/replay"
	"github.com/Smana/runlore/internal/providers"
)

// TestReplayReturnsTurnsInOrder is the core contract: the provider ignores the
// request and hands back the recorded assistant turns in the order they were
// recorded, usage included (the demo's cost footer reads it).
func TestReplayReturnsTurnsInOrder(t *testing.T) {
	tr, err := replay.Load("testdata/two-turns.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := replay.New(tr)

	first, err := p.Complete(context.Background(), providers.CompletionRequest{})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != "what_changed" {
		t.Fatalf("turn 1 tool calls = %+v, want one what_changed", first.ToolCalls)
	}
	if first.Usage.InputTokens != 4211 || first.Usage.OutputTokens != 96 {
		t.Errorf("turn 1 usage = %+v, want 4211 in / 96 out", first.Usage)
	}

	second, err := p.Complete(context.Background(), providers.CompletionRequest{})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if len(second.ToolCalls) != 1 || second.ToolCalls[0].Name != "submit_findings" {
		t.Fatalf("turn 2 tool calls = %+v, want one submit_findings", second.ToolCalls)
	}
}

// TestReplayExhaustionIsLoud: running past the recorded turns must fail with an
// actionable error naming the fix. Returning an empty completion would produce a
// demo that silently ends with no findings — the exact failure mode a first-time
// user cannot diagnose.
func TestReplayExhaustionIsLoud(t *testing.T) {
	tr, err := replay.Load("testdata/two-turns.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := replay.New(tr)
	for i := 0; i < 2; i++ {
		if _, err := p.Complete(context.Background(), providers.CompletionRequest{}); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
	}
	_, err = p.Complete(context.Background(), providers.CompletionRequest{})
	if err == nil {
		t.Fatal("expected an error past the end of the transcript")
	}
	if !strings.Contains(err.Error(), "exhausted") || !strings.Contains(err.Error(), "--record") {
		t.Errorf("error must say it is exhausted and how to fix it, got: %v", err)
	}
}

// TestToolNames exposes the recorded tool names so the demo wiring can assert they
// all still exist (the drift guard in Task 3).
func TestToolNames(t *testing.T) {
	tr, err := replay.Load("testdata/two-turns.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := tr.ToolNames()
	want := map[string]bool{"what_changed": true, "submit_findings": true}
	if len(got) != len(want) {
		t.Fatalf("ToolNames() = %v, want %d distinct names", got, len(want))
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected tool name %q", n)
		}
	}
}

// TestLoadRejectsMissingFile: a missing file fails at load so the error can name
// the file, rather than later midway through a demo.
func TestLoadRejectsMissingFile(t *testing.T) {
	if _, err := replay.Load("testdata/does-not-exist.json"); err == nil {
		t.Fatal("expected an error for a missing transcript")
	}
}

// TestLoadRejectsEmptyTranscript: a transcript with no turns would fail later,
// mid-demo, with a confusing error. Fail at load, naming the file.
func TestLoadRejectsEmptyTranscript(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.json")
	if err := os.WriteFile(path, []byte(`{"version":1,"scenario":"x","turns":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := replay.Load(path)
	if err == nil {
		t.Fatal("expected an error for a transcript with no turns")
	}
	if !strings.Contains(err.Error(), "no turns") {
		t.Errorf("error should say the transcript has no turns, got: %v", err)
	}
}
