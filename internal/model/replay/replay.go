// SPDX-License-Identifier: Apache-2.0

// Package replay serves a recorded LLM transcript as a providers.ModelProvider.
//
// It exists so `lore demo investigate --offline` can show a REAL root cause with no
// API key and no network: the model turns are replayed from a transcript recorded
// once against a live model, while the tools, the investigation loop and the
// rendered verdict card remain the production code paths. Only the model is canned.
package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/Smana/runlore/internal/providers"
)

// Transcript is a recorded investigation: the ordered assistant turns a live model
// produced for one scenario, plus the provenance a reader needs to judge them.
type Transcript struct {
	Version      int      `json:"version"`
	Scenario     string   `json:"scenario"`
	RecordedAt   string   `json:"recorded_at"`
	RecordedWith Recorded `json:"recorded_with"`
	Turns        []Turn   `json:"turns"`
}

// Recorded names the model that produced the transcript. It is rendered with the
// demo output so a replayed card never passes itself off as a fresh live run.
type Recorded struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Turn is one recorded assistant completion.
type Turn struct {
	Text      string               `json:"text,omitempty"`
	ToolCalls []providers.ToolCall `json:"tool_calls,omitempty"`
	Usage     providers.Usage      `json:"usage"`
}

// Load reads and validates a transcript. A missing file, malformed JSON, or an
// empty turn list fails here — where the error can name the file — rather than
// midway through a demo the user is watching.
func Load(path string) (*Transcript, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied transcript path
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	var t Transcript
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse transcript %s: %w", path, err)
	}
	if len(t.Turns) == 0 {
		return nil, fmt.Errorf("transcript %s has no turns", path)
	}
	return &t, nil
}

// ToolNames returns the distinct tool names the transcript calls, in first-seen
// order. The demo wiring asserts every one still exists, so renaming a tool fails
// CI instead of shipping a broken demo.
func (t *Transcript) ToolNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, turn := range t.Turns {
		for _, tc := range turn.ToolCalls {
			if !seen[tc.Name] {
				seen[tc.Name] = true
				names = append(names, tc.Name)
			}
		}
	}
	return names
}

// Provider replays a transcript's turns in order. Safe for concurrent use because
// the loop may call from more than one goroutine, though today it does not.
type Provider struct {
	t  *Transcript
	mu sync.Mutex
	i  int
}

// New wraps a loaded transcript as a model provider.
func New(t *Transcript) *Provider { return &Provider{t: t} }

// compile-time assertion: the replay provider satisfies the model interface.
var _ providers.ModelProvider = (*Provider)(nil)

// Complete returns the next recorded turn, ignoring the request entirely — the
// transcript is a fixed script, not a function of the prompt.
//
// Running past the end is an ERROR, deliberately. The loop asking for one more turn
// than was recorded means the fixture no longer matches the code driving it, and the
// only correct response is to say so and name the fix.
func (p *Provider) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.i >= len(p.t.Turns) {
		return providers.CompletionResponse{}, fmt.Errorf(
			"transcript exhausted after %d turns (the loop asked for another) — re-record with `lore demo investigate --record <path>`",
			len(p.t.Turns))
	}
	turn := p.t.Turns[p.i]
	p.i++
	return providers.CompletionResponse{
		Text:      turn.Text,
		ToolCalls: turn.ToolCalls,
		Usage:     turn.Usage,
	}, nil
}
