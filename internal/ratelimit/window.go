// SPDX-License-Identifier: Apache-2.0

// Package ratelimit provides a sliding-window start limiter — the windowed
// timestamp pattern from internal/action/auto.go:reserve(), reusable.
package ratelimit

import (
	"sync"
	"time"
)

// Window allows up to max events per sliding window. max <= 0 is unlimited.
// Safe for concurrent use; clock injectable for tests.
type Window struct {
	max    int
	window time.Duration
	now    func() time.Time
	mu     sync.Mutex
	recent []time.Time
}

// New returns a Window allowing maxEvents per window.
func New(maxEvents int, window time.Duration) *Window {
	return &Window{max: maxEvents, window: window, now: time.Now}
}

func (w *Window) slideLocked() {
	cutoff := w.now().Add(-w.window)
	kept := w.recent[:0]
	for _, t := range w.recent {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	w.recent = kept
}

// Allow reports whether an event fits the budget, recording it if so.
func (w *Window) Allow() bool {
	if w.max <= 0 {
		return true
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.slideLocked()
	if len(w.recent) >= w.max {
		return false
	}
	w.recent = append(w.recent, w.now())
	return true
}

// Count returns the number of events currently in the window (peek; no record).
func (w *Window) Count() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.slideLocked()
	return len(w.recent)
}

// Window returns the configured sliding-window duration.
func (w *Window) Window() time.Duration { return w.window }
