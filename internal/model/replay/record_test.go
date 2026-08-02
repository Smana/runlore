// SPDX-License-Identifier: Apache-2.0

package replay_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/model/replay"
	"github.com/Smana/runlore/internal/providers"
)

// fakeModel returns a fixed sequence of completions, standing in for a live model
// during the recorder test (no network, no key).
type fakeModel struct {
	responses []providers.CompletionResponse
	i         int
}

func (f *fakeModel) Complete(context.Context, providers.CompletionRequest) (providers.CompletionResponse, error) {
	r := f.responses[f.i]
	f.i++
	return r, nil
}

// TestRecordRoundTrip: what the recorder writes must replay identically. This is the
// guarantee that makes `--record` trustworthy — a re-recorded fixture behaves exactly
// like the live run it captured.
func TestRecordRoundTrip(t *testing.T) {
	live := &fakeModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "what_changed", Args: `{"namespace":"harbor"}`}},
			Usage: providers.Usage{InputTokens: 100, OutputTokens: 20}},
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: "submit_findings", Args: `{"confidence":0.9}`}},
			Usage: providers.Usage{InputTokens: 300, OutputTokens: 80}},
	}}

	rec := replay.NewRecorder(live, replay.Recorded{Provider: "anthropic", Model: "claude-sonnet-5"}, "harbor-chart-bump")
	for i := 0; i < 2; i++ {
		if _, err := rec.Complete(context.Background(), providers.CompletionRequest{}); err != nil {
			t.Fatalf("record turn %d: %v", i+1, err)
		}
	}

	path := filepath.Join(t.TempDir(), "transcript.json")
	if err := rec.Write(path, "2026-08-02T09:14:00Z"); err != nil {
		t.Fatalf("write: %v", err)
	}

	tr, err := replay.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if tr.Scenario != "harbor-chart-bump" || tr.RecordedWith.Model != "claude-sonnet-5" || tr.RecordedAt != "2026-08-02T09:14:00Z" {
		t.Errorf("provenance lost: %+v", tr)
	}
	if len(tr.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(tr.Turns))
	}

	p := replay.New(tr)
	for i, want := range []string{"what_changed", "submit_findings"} {
		got, err := p.Complete(context.Background(), providers.CompletionRequest{})
		if err != nil {
			t.Fatalf("replay turn %d: %v", i+1, err)
		}
		if got.ToolCalls[0].Name != want {
			t.Errorf("replay turn %d = %q, want %q", i+1, got.ToolCalls[0].Name, want)
		}
	}
}

// TestWriteRefusesEmpty: writing a transcript with no captured turns would ship a
// fixture that breaks the demo on first use. Fail at write time instead.
func TestWriteRefusesEmpty(t *testing.T) {
	rec := replay.NewRecorder(&fakeModel{}, replay.Recorded{}, "none")
	err := rec.Write(filepath.Join(t.TempDir(), "empty.json"), "2026-08-02T09:14:00Z")
	if err == nil {
		t.Fatal("expected an error writing a transcript with no turns")
	}
}
