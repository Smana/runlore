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
