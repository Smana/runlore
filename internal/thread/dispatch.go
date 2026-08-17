// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// Dispatcher runs detached work under three bounds: how much runs at once, how
// long any single piece may run, and a drain that lets shutdown wait for what is
// in flight.
//
// It exists because both transports need identical behaviour here. A chat
// transport acknowledges the platform BEFORE doing the work (Slack retries
// anything unacked within 3 seconds; the Matrix sync loop must keep
// long-polling), so the work necessarily outlives the call that scheduled it —
// and unbounded detached goroutines on an internet-facing path is its own
// problem. Duplicating this per transport would guarantee the two drift.
type Dispatcher struct {
	slots   chan struct{}
	timeout time.Duration
	wg      sync.WaitGroup
	log     *slog.Logger
}

// NewDispatcher returns a Dispatcher allowing `slots` concurrent pieces of work,
// each bounded by `timeout`.
func NewDispatcher(slots int, timeout time.Duration, log *slog.Logger) *Dispatcher {
	if slots <= 0 {
		slots = 1
	}
	if log == nil {
		log = slog.Default()
	}
	return &Dispatcher{slots: make(chan struct{}, slots), timeout: timeout, log: log}
}

// Go runs fn on a bounded worker and reports whether it was accepted. It never
// blocks: when every slot is busy it returns false immediately, having run
// nothing, so the caller can tell the human rather than queue silently.
//
// fn receives a context derived from ctx with cancellation stripped and the
// dispatcher's timeout applied. Stripping cancellation is deliberate: ctx is
// typically a request context that dies the moment its handler returns, which
// would cancel the work before it could do anything.
func (d *Dispatcher) Go(ctx context.Context, fn func(context.Context)) bool {
	select {
	case d.slots <- struct{}{}:
	default:
		return false
	}
	work := context.WithoutCancel(ctx)
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		defer func() { <-d.slots }()
		defer func() {
			if rec := recover(); rec != nil {
				d.log.Error("recovered from panic in detached work", "panic", rec, "stack", string(debug.Stack()))
			}
		}()
		if d.timeout > 0 {
			var cancel context.CancelFunc
			work, cancel = context.WithTimeout(work, d.timeout)
			defer cancel()
		}
		fn(work)
	}()
	return true
}

// Drain waits for in-flight work to finish, bounded by ctx. A wedged handler
// must not be able to block shutdown forever.
func (d *Dispatcher) Drain(ctx context.Context) {
	done := make(chan struct{})
	go func() { d.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
		d.log.Warn("drain timed out with work still in flight")
	}
}
