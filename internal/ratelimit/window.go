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

// Refund hands one recorded event back to the budget, for a caller that took a
// token before an operation and then learned the operation never happened.
//
// It exists because Allow() is necessarily optimistic: a caller has to reserve
// before it acts, or two callers race past the same last token. Without a way to
// undo that reservation, every failure downstream is charged as though it had
// succeeded — which is how a forge outage spent an hour's whole write budget on
// writes that landed nothing (see thread.Responder.write).
//
// It removes the MOST RECENT timestamp, which is not necessarily the one this
// caller recorded. That is deliberate rather than sloppy: tokens here are
// fungible — the window only ever asks HOW MANY events fall inside it, never
// which — so returning one restores exactly the headroom that was taken. The
// only observable difference is which instant the returned token would have
// expired at, and dropping the newest is the conservative end of that: the
// remaining timestamps expire no later than the one removed, so a refund can
// never extend how long the window stays full.
//
// A refund with nothing recorded is a no-op, so it can neither push the count
// negative nor mint budget the window never granted. That also covers a refund
// arriving after the window has already rolled past the event being refunded —
// the case a plain counter would turn into headroom for the NEXT window: the
// entries are append-ordered, so an expired one can only ever PRECEDE a live
// one, and dropping the tail therefore gives back a live token or nothing.
//
// It does not slide first, unlike Allow and Count, and that is not an omission:
// by the ordering above, sliding could not change which entry the tail is or
// what the window answers afterwards, and a line no test can falsify is worse
// than the sentence explaining why it is absent.
func (w *Window) Refund() {
	if w.max <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if len(w.recent) == 0 {
		return
	}
	w.recent = w.recent[:len(w.recent)-1]
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
