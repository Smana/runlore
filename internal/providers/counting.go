// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"sync"
)

// CountingModel wraps a ModelProvider and sums the provider-reported token usage
// across completions.
//
// It lives here, next to the ModelProvider interface it decorates, because it is the
// answer to a question every ONE-SHOT command has and only `lore serve` has another
// answer to. The OTel metric set (internal/telemetry) is exported over a Prometheus
// endpoint that telemetry.Setup installs in `lore serve` alone, so in a CLI process
// — `lore eval`, `lore kb import`, `lore validate-kb` — those instruments bind to
// the global no-op meter, in a process that exits seconds later, and nothing ever
// scrapes them. A counter there would look like instrumentation and measure nothing.
// What a CLI can actually show its operator is what IT spent, in its own output, and
// this is the wrapper that knows.
type CountingModel struct {
	Inner ModelProvider

	mu    sync.Mutex
	total Usage
}

// Complete delegates to Inner and accumulates the response usage — on the error path
// too, not only on success.
//
// A completion that FAILED still cost whatever the provider reported before it broke:
// every client in this package hands those counts back alongside its error (see
// CompletionResponse.CostOnly, which exists for no other reader), because a provider
// bills for the prompt it accepted whether or not it finished the reply. Counting
// only successes made a flapping provider free by construction, and the figure this
// wrapper prints is the ONLY place a one-shot command's spend is visible — so the
// error was not imprecision, it was an under-report in the direction of the operator's
// bill. It is the same rule internal/investigate's loop applies to its own totals.
//
// Zero usage — a failure before the provider reported anything, e.g. a dial error —
// adds nothing. That is "unknown", never a claim that the call was free, and never a
// guess in the other direction either.
func (c *CountingModel) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	resp, err := c.Inner.Complete(ctx, req)
	c.mu.Lock()
	c.total.InputTokens += resp.Usage.InputTokens
	c.total.OutputTokens += resp.Usage.OutputTokens
	c.total.CachedInputTokens += resp.Usage.CachedInputTokens
	c.total.CacheWriteTokens += resp.Usage.CacheWriteTokens
	c.mu.Unlock()
	return resp, err
}

// Total returns the usage accumulated so far.
func (c *CountingModel) Total() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}
