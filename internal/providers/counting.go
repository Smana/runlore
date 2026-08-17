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

// Complete delegates to Inner and accumulates the response usage on success.
func (c *CountingModel) Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error) {
	resp, err := c.Inner.Complete(ctx, req)
	if err == nil {
		c.mu.Lock()
		c.total.InputTokens += resp.Usage.InputTokens
		c.total.OutputTokens += resp.Usage.OutputTokens
		c.total.CachedInputTokens += resp.Usage.CachedInputTokens
		c.total.CacheWriteTokens += resp.Usage.CacheWriteTokens
		c.mu.Unlock()
	}
	return resp, err
}

// Total returns the usage accumulated so far.
func (c *CountingModel) Total() Usage {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.total
}
