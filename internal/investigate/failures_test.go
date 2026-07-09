// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"io"
	"log/slog"
)

// Shared test helpers for the investigate package (debounce + drain tests).
type collectEnqueuer struct{ reqs []Request }

func (c *collectEnqueuer) Enqueue(r Request) { c.reqs = append(c.reqs, r) }

var discardLog = slog.New(slog.NewTextHandler(io.Discard, nil))
