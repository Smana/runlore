// SPDX-License-Identifier: Apache-2.0

package ratelimit

import (
	"testing"
	"time"
)

func TestWindowAllowAndSlide(t *testing.T) {
	now := time.Unix(0, 0)
	w := New(2, time.Minute)
	w.now = func() time.Time { return now }

	if !w.Allow() {
		t.Fatal("first start within budget should be allowed")
	}
	if !w.Allow() {
		t.Fatal("second start within budget should be allowed")
	}
	if w.Allow() {
		t.Fatal("third start should be denied (budget 2)")
	}
	if got := w.Count(); got != 2 {
		t.Fatalf("Count: got %d, want 2", got)
	}
	// roll the window forward; old entries expire
	now = now.Add(2 * time.Minute)
	if w.Count() != 0 {
		t.Fatalf("window should have slid clear, Count=%d", w.Count())
	}
	if !w.Allow() {
		t.Fatal("after slide, a new start should be allowed")
	}
}

func TestWindowZeroMaxUnlimited(t *testing.T) {
	w := New(0, time.Minute)
	for i := 0; i < 100; i++ {
		if !w.Allow() {
			t.Fatal("max 0 must be unlimited")
		}
	}
}

// TestWindowRefundReturnsATokenAndOnlyWhenThereIsOne pins Refund's two halves:
// an allowed event can be handed back, and a Refund with nothing recorded is a
// no-op rather than a way to mint budget.
func TestWindowRefundReturnsATokenAndOnlyWhenThereIsOne(t *testing.T) {
	now := time.Unix(0, 0)
	w := New(2, time.Minute)
	w.now = func() time.Time { return now }

	// Nothing recorded yet: a refund must not push the count negative or create
	// headroom the window never had.
	w.Refund()
	if got := w.Count(); got != 0 {
		t.Fatalf("Refund on an empty window: Count = %d, want 0", got)
	}
	for range 2 {
		if !w.Allow() {
			t.Fatal("within budget")
		}
	}
	if w.Allow() {
		t.Fatal("budget 2 must be spent")
	}
	w.Refund()
	if got := w.Count(); got != 1 {
		t.Fatalf("after Refund: Count = %d, want 1", got)
	}
	if !w.Allow() {
		t.Fatal("the refunded token must be spendable again")
	}
	if w.Allow() {
		t.Fatal("Refund must return exactly one token, not reset the window")
	}
}

// TestWindowRefundDoesNotOutliveTheWindow keeps a refund from resurrecting a
// token the slide already expired: refunding after the window rolled forward
// leaves the count at zero rather than at -1, which would be an extra event's
// worth of headroom for the NEXT window.
func TestWindowRefundDoesNotOutliveTheWindow(t *testing.T) {
	now := time.Unix(0, 0)
	w := New(1, time.Minute)
	w.now = func() time.Time { return now }
	if !w.Allow() {
		t.Fatal("within budget")
	}
	now = now.Add(2 * time.Minute)
	w.Refund()
	if got := w.Count(); got != 0 {
		t.Fatalf("Count = %d, want 0", got)
	}
	if !w.Allow() {
		t.Fatal("a fresh window allows one")
	}
	if w.Allow() {
		t.Fatal("and only one — the stale refund must not have added headroom")
	}
}

// TestWindowRefundIsANoOpWhenUnlimited mirrors Allow's own max <= 0 guard: an
// unlimited window records nothing, so there is nothing to hand back.
func TestWindowRefundIsANoOpWhenUnlimited(t *testing.T) {
	w := New(0, time.Minute)
	w.Allow()
	w.Refund()
	if got := w.Count(); got != 0 {
		t.Fatalf("Count = %d, want 0", got)
	}
}
