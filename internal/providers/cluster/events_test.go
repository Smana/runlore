// SPDX-License-Identifier: Apache-2.0

package cluster

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"
)

// TestEventsWarnOnlyOrdersByOwnTimestamp guards against a filtered-slice /
// original-index mismatch: when warnOnly drops a Normal event, the surviving
// Warning events must still be returned most-recent-first by THEIR OWN
// timestamps — not ordered by whichever original-list entry happened to land at
// their post-filter index.
func TestEventsWarnOnlyOrdersByOwnTimestamp(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := func(name, reason, etype string, ageSec int) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "apps"},
			Type:           etype,
			Reason:         reason,
			LastTimestamp:  metav1.NewTime(base.Add(time.Duration(ageSec) * time.Second)),
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "p1"},
		}
	}
	// List order (a, b, c): a NEW Normal event that warnOnly drops, then an OLD
	// warning, then a NEW warning. The dropped Normal sits at the original index
	// the buggy comparator reads for the first surviving warning.
	r := New(fake.NewSimpleClientset(
		ev("a-normal", "NormalEvt", corev1.EventTypeNormal, 100),
		ev("b-warn-old", "WarnOld", corev1.EventTypeWarning, 10),
		ev("c-warn-new", "WarnNew", corev1.EventTypeWarning, 50),
	))

	out, err := r.Events(context.Background(), "apps", "", true)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("warnOnly should return 2 warnings (Normal filtered), got %d: %+v", len(out), out)
	}
	if out[0].Reason != "WarnNew" || out[1].Reason != "WarnOld" {
		t.Fatalf("warnings not most-recent-first by own timestamp: got [%s, %s], want [WarnNew, WarnOld]",
			out[0].Reason, out[1].Reason)
	}
}

// K2 (v0.9): kube_events fetched a single un-windowed page and only sorted
// newest-first AFTER fetching — in a busy namespace the newest events can live on
// a page never fetched. EventsSince must window by time (since_minutes), drop
// events older than the window, and return the newest in-window events first.
func TestEventsSinceWindowsAndOrders(t *testing.T) {
	now := time.Now()
	ev := func(name, reason string, ago time.Duration) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "apps"},
			Type:           corev1.EventTypeWarning,
			Reason:         reason,
			LastTimestamp:  metav1.NewTime(now.Add(-ago)),
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "p1"},
		}
	}
	r := New(fake.NewSimpleClientset(
		ev("old", "TooOld", 90*time.Minute), // outside a 30m window
		ev("mid", "InWindowOld", 20*time.Minute),
		ev("new", "InWindowNew", 2*time.Minute),
	))

	out, err := r.EventsSince(context.Background(), "apps", "", true, 30)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("since_minutes=30 must drop the 90m-old event, got %d: %+v", len(out), out)
	}
	if out[0].Reason != "InWindowNew" || out[1].Reason != "InWindowOld" {
		t.Fatalf("in-window events must be newest-first: got [%s, %s]", out[0].Reason, out[1].Reason)
	}
	for _, e := range out {
		if e.Reason == "TooOld" {
			t.Fatalf("out-of-window event leaked: %+v", out)
		}
	}
}

// A zero window must be equivalent to the un-windowed Events call (safe default:
// a test-cluster run must not regress today's behavior).
func TestEventsSinceZeroWindowEqualsEvents(t *testing.T) {
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ev := func(name string, ageSec int) *corev1.Event {
		return &corev1.Event{
			ObjectMeta:     metav1.ObjectMeta{Name: name, Namespace: "apps"},
			Type:           corev1.EventTypeWarning,
			Reason:         name,
			LastTimestamp:  metav1.NewTime(base.Add(time.Duration(ageSec) * time.Second)),
			InvolvedObject: corev1.ObjectReference{Kind: "Pod", Name: "p1"},
		}
	}
	r := New(fake.NewSimpleClientset(ev("a", 10), ev("b", 50)))
	since, err := r.EventsSince(context.Background(), "apps", "", true, 0)
	if err != nil {
		t.Fatalf("EventsSince: %v", err)
	}
	plain, err := r.Events(context.Background(), "apps", "", true)
	if err != nil {
		t.Fatalf("Events: %v", err)
	}
	if len(since) != len(plain) || len(since) != 2 {
		t.Fatalf("zero window must match Events: since=%d plain=%d", len(since), len(plain))
	}
	if since[0].Reason != plain[0].Reason || since[1].Reason != plain[1].Reason {
		t.Fatalf("zero window ordering diverged from Events: %+v vs %+v", since, plain)
	}
}
