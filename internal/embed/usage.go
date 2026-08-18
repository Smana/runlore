// SPDX-License-Identifier: Apache-2.0

package embed

import (
	"context"
	"sync"
)

// UsageSink accumulates what the /embeddings endpoint billed for the calls made
// under one context. It is how an embedding call's cost reaches a CALLER that has a
// spend ceiling — an investigation — without widening catalog.Embedder, which returns
// vectors only and is deliberately narrow (the catalog must not have to know what an
// embedder costs).
//
// A context-scoped side channel rather than a return value because the call sits two
// interfaces below its payer: investigate.Recall asks catalog.HybridSearcher, which
// asks catalog.Embedder, and only the third of those touches money. Threading a usage
// return up through both would put spend accounting into two interfaces that have no
// other reason to know about it, and would break every fake implementing them.
// internal/investigate already carries per-investigation state this way (see
// WithObservedResources), so this is the shape that package's readers expect.
//
// Concurrency-safe: one sink is shared by every embedding call an investigation makes,
// and nothing promises those are sequential.
//
// The zero value is a usable, empty sink.
type UsageSink struct {
	mu     sync.Mutex
	calls  int
	tokens int
}

// record charges one /embeddings round trip to the sink. promptTokens is what the
// endpoint reported for it, or 0 when it reported nothing — which means UNKNOWN, not
// free, exactly as providers.Usage's zero value does. The call is counted either way:
// a request the endpoint accepted is billed whether or not we could parse its answer.
func (s *UsageSink) record(promptTokens int) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.tokens += promptTokens
}

// Totals reports how many /embeddings requests have been charged to this sink and
// their total reported prompt tokens. Nil-safe: a caller that never installed a sink
// reads 0, 0.
//
// Embeddings have no output half — the reply is a vector, and every OpenAI-compatible
// endpoint reports the cost as prompt tokens only — so one token figure is the whole
// bill, not the input half of one.
func (s *UsageSink) Totals() (calls, promptTokens int) {
	if s == nil {
		return 0, 0
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls, s.tokens
}

type usageSinkKey struct{}

// WithUsageSink derives a context whose embedding calls charge sink. Callers that
// bound or report their own spend install one for the duration of the work they are
// accounting for; everything else (the CLI, the catalog syncer) passes a plain
// context and behaves exactly as before.
func WithUsageSink(ctx context.Context, sink *UsageSink) context.Context {
	return context.WithValue(ctx, usageSinkKey{}, sink)
}

// UsageSinkFrom returns the sink installed on ctx, or nil. The returned pointer is
// always safe to call Totals on.
func UsageSinkFrom(ctx context.Context) *UsageSink {
	s, _ := ctx.Value(usageSinkKey{}).(*UsageSink)
	return s
}
