// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// progressScript drives a multi-step investigation: two tool-calling turns then a
// submit_findings. Interim text on the second turn carries a secret so redaction
// at the egress boundary can be asserted.
func progressScript() []providers.CompletionResponse {
	return []providers.CompletionResponse{
		{Text: "starting", ToolCalls: []providers.ToolCall{{ID: "1", Name: "what_changed", Args: `{}`}}},
		{Text: "found token ghp_abcdefghijklmnopqrstuvwx in the logs", ToolCalls: []providers.ToolCall{{ID: "2", Name: "kb_search", Args: `{}`}}},
		{Text: "one more look", ToolCalls: []providers.ToolCall{{ID: "3", Name: "pod_status", Args: `{}`}}},
		{ToolCalls: []providers.ToolCall{{ID: "4", Name: submitFindingsName, Args: `{"confidence":0.7,"root_causes":[{"summary":"done"}]}`}}},
	}
}

func TestProgressFiresEveryNSteps(t *testing.T) {
	var got []providers.ProgressUpdate
	li := &LoopInvestigator{
		Model:              &scriptModel{responses: progressScript()},
		Log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProgressEverySteps: 2,
		OnProgress:         func(up providers.ProgressUpdate) { got = append(got, up) },
		OnComplete:         func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "HarborDown"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// Cadence 2 over steps 0..3 (1-based 1..4): fires at step 2 and step 4.
	if len(got) != 2 {
		t.Fatalf("expected 2 progress pings at cadence 2, got %d: %+v", len(got), got)
	}
	first := got[0]
	if first.Step != 2 || first.MaxSteps != 20 {
		t.Fatalf("first ping step/max = %d/%d, want 2/20", first.Step, first.MaxSteps)
	}
	if first.Title != "HarborDown" {
		t.Fatalf("ping title = %q, want HarborDown", first.Title)
	}
	// Tools requested so far this run are reflected (counts by name).
	if first.ToolsUsed["what_changed"] != 1 || first.ToolsUsed["kb_search"] != 1 {
		t.Fatalf("ping tools-used = %+v, want what_changed:1 kb_search:1", first.ToolsUsed)
	}
	// The model interim text is secret-redacted before it leaves the loop.
	if strings.Contains(first.Interim, "ghp_abcdefghijklmnopqrstuvwx") {
		t.Fatalf("interim text leaked a secret: %q", first.Interim)
	}
	if !strings.Contains(first.Interim, "[REDACTED]") {
		t.Fatalf("interim text was not redacted: %q", first.Interim)
	}
}

func TestProgressDisabledByDefault(t *testing.T) {
	var fired int
	li := &LoopInvestigator{
		Model:              &scriptModel{responses: progressScript()},
		Log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProgressEverySteps: 0, // disabled even though OnProgress is set
		OnProgress:         func(providers.ProgressUpdate) { fired++ },
		OnComplete:         func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if fired != 0 {
		t.Fatalf("progress must not fire when cadence <= 0, fired %d times", fired)
	}
}

// captured records that the returned callback also copies the tools map (so a
// consumer can't race/observe later loop mutations of the same map).
func TestProgressCopiesToolsMap(t *testing.T) {
	var snapshots []map[string]int
	li := &LoopInvestigator{
		Model:              &scriptModel{responses: progressScript()},
		Log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		ProgressEverySteps: 2,
		OnProgress:         func(up providers.ProgressUpdate) { snapshots = append(snapshots, up.ToolsUsed) },
		OnComplete:         func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(snapshots) < 2 {
		t.Fatalf("expected >=2 snapshots, got %d", len(snapshots))
	}
	// Mutating an earlier snapshot must not affect a later one — each ping carries
	// its own copy, not the live loop map aliased across pings.
	snapshots[0]["injected"] = 99
	if _, ok := snapshots[1]["injected"]; ok {
		t.Fatal("progress tools-used map is aliased across pings; it must be copied")
	}
}
