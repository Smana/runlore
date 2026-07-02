package clientcore

import (
	"encoding/json"
	"fmt"
	"io"
	"iter"

	"github.com/Smana/runlore/internal/httpx"
)

// SSEEvents parses an SSE stream into decoded events of type E. It wraps the
// generic httpx.SSEData framing primitive and json-unmarshals each event's
// data payload; a framing/read error or a JSON decode error is surfaced via
// the second yield value.
//
// A provider whose stream grammar doesn't fit (e.g. openai's non-JSON [DONE]
// sentinel) keeps its own loop over httpx.SSEData.
func SSEEvents[E any](r io.Reader) iter.Seq2[E, error] {
	return func(yield func(E, error) bool) {
		for payload, err := range httpx.SSEData(r) {
			if err != nil {
				var zero E
				yield(zero, err)
				return
			}
			var ev E
			if err := json.Unmarshal(payload, &ev); err != nil {
				var zero E
				if !yield(zero, fmt.Errorf("decode sse event: %w", err)) {
					return
				}
				continue
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}
