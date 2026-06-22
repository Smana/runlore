package ratelimit

import (
	"testing"
	"time"
)

func TestWindowAllowAndSlide(t *testing.T) {
	now := time.Unix(0, 0)
	w := New(2, time.Minute)
	w.now = func() time.Time { return now }

	if !w.Allow() || !w.Allow() {
		t.Fatal("first two starts within budget should be allowed")
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
