// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/Smana/runlore/internal/providers"
)

// Recorder decorates a live model, capturing every completion so the run can be
// replayed later with no key. It is the only writer of transcripts — regenerating a
// demo fixture is `--record`, never hand-editing JSON.
type Recorder struct {
	inner    providers.ModelProvider
	meta     Recorded
	scenario string

	mu    sync.Mutex
	turns []Turn
}

// NewRecorder wraps inner, tagging the capture with the model that produced it.
func NewRecorder(inner providers.ModelProvider, meta Recorded, scenario string) *Recorder {
	return &Recorder{inner: inner, meta: meta, scenario: scenario}
}

// Complete delegates and captures the response. A failed call is NOT captured: a
// transcript must contain only turns that really happened, or replay would diverge
// from the live run it claims to reproduce.
func (r *Recorder) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	resp, err := r.inner.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	r.mu.Lock()
	r.turns = append(r.turns, Turn{Text: resp.Text, ToolCalls: resp.ToolCalls, Usage: resp.Usage})
	r.mu.Unlock()
	return resp, nil
}

// Write serializes the captured turns to path. recordedAt is passed in rather than
// read from the clock so callers stay testable.
func (r *Recorder) Write(path, recordedAt string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.turns) == 0 {
		return fmt.Errorf("nothing recorded — refusing to write an empty transcript to %s", path)
	}
	t := Transcript{
		Version: 1, Scenario: r.scenario, RecordedAt: recordedAt,
		RecordedWith: r.meta, Turns: r.turns,
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal transcript: %w", err)
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
