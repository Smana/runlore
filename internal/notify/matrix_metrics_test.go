// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"context"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Smana/runlore/internal/telemetry"
	"github.com/Smana/runlore/internal/thread"
)

// meteredNotifyInstruments mirrors internal/server's own helper of the same
// shape (meteredInstruments): installs a REAL SDK meter provider backed by a
// manual reader, and returns the instrument set bound to it plus a reader
// that sums an int64 counter by its exported series name (0, false when the
// series was never recorded). The provider is global, so a test using this
// must not run in parallel with another that does; cleanup restores the
// no-op provider.
func meteredNotifyInstruments(t *testing.T) (*telemetry.Metrics, func(series string) (int64, bool)) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	return telemetry.NewMetrics(), func(series string) (int64, bool) {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		for _, sm := range rm.ScopeMetrics {
			for _, md := range sm.Metrics {
				if md.Name != series {
					continue
				}
				sum, ok := md.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("series %q is not an int64 sum (%T)", series, md.Data)
				}
				var total int64
				for _, dp := range sum.DataPoints {
					total += dp.Value
				}
				return total, true
			}
		}
		return 0, false
	}
}

// TestMatrixHandleMessageMentionDroppedLogsAndCounts is the regression test
// for Fix 4: when Dispatch itself is saturated, the mention is
// unrecoverably lost — /sync's position token has already advanced past
// this event by the time this run finishes, so the homeserver will never
// redeliver it — yet before this fix nothing was logged and nothing was
// counted (only the DOUBLE-saturation case, busy dispatch also full,
// logged). Mirrors Slack's own regression test,
// TestEventsSaturatedPoolNotifiesBusyAndCountsMetric
// (internal/server/events_test.go).
func TestMatrixHandleMessageMentionDroppedLogsAndCounts(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixThreadCaptureServer(t, self)
	defer srv.Close()

	m, read := meteredNotifyInstruments(t)

	rep := &fakeMentionReplier{doneAt: 1, done: make(chan struct{})}
	d := thread.NewDispatcher(1, time.Minute, matrixTestLog())
	busy := thread.NewDispatcher(4, time.Minute, matrixTestLog())
	block, running := make(chan struct{}), make(chan struct{})
	if !d.Go(context.Background(), func(context.Context) { close(running); <-block }) {
		t.Fatal("first Go refused with a free slot")
	}
	<-running // the one slot is now occupied
	defer close(block)

	responder := &thread.Responder{Forge: &fakeThreadForge{}, Log: matrixTestLog()}
	f := NewMatrixFeedback(srv.URL, room, "tok", nil, matrixTestLog(),
		WithThreadCapture(&thread.Mention{Responder: responder, Replier: rep, Log: matrixTestLog()}, d, busy),
		WithMetrics(m))
	f.self = self

	e := matrixEvent{Sender: "@alice:hs", EventID: "$reply-drop"}
	e.Content.Body = "@runlore:hs note: please record this"
	e.Content.Mentions.UserIDs = []string{self}
	e.Content.RelatesTo.RelType = "m.thread"
	e.Content.RelatesTo.EventID = "$root-ours"

	f.handleMessage(context.Background(), e)
	// The main dispatcher is saturated, so handleMessage falls through to the
	// busy-notice path (its own, separate dispatcher) — wait for that reply to
	// land before reading the counter, so the read is not a race against the
	// detached Add call.
	waitForReplies(t, rep)

	if got, ok := read("runlore_mentions_dropped_on_saturation_total"); !ok || got != 1 {
		t.Fatalf("runlore_mentions_dropped_on_saturation_total = %d (recorded=%v), want 1", got, ok)
	}
}

// TestMatrixHandleMessageMentionDroppedNilMetricsSafe pins the nil-safety
// half of Fix 4: MatrixFeedback built without WithMetrics (every listener
// before this fix, and every test that doesn't opt in) must not panic when a
// mention is dropped.
func TestMatrixHandleMessageMentionDroppedNilMetricsSafe(t *testing.T) {
	const room = "!r:hs"
	const self = "@runlore:hs"
	srv := matrixThreadCaptureServer(t, self)
	defer srv.Close()

	rep := &fakeMentionReplier{doneAt: 1, done: make(chan struct{})}
	d := thread.NewDispatcher(1, time.Minute, matrixTestLog())
	busy := thread.NewDispatcher(4, time.Minute, matrixTestLog())
	block, running := make(chan struct{}), make(chan struct{})
	if !d.Go(context.Background(), func(context.Context) { close(running); <-block }) {
		t.Fatal("first Go refused with a free slot")
	}
	<-running
	defer close(block)

	responder := &thread.Responder{Forge: &fakeThreadForge{}, Log: matrixTestLog()}
	f := NewMatrixFeedback(srv.URL, room, "tok", nil, matrixTestLog(),
		WithThreadCapture(&thread.Mention{Responder: responder, Replier: rep, Log: matrixTestLog()}, d, busy))
	f.self = self

	e := matrixEvent{Sender: "@alice:hs", EventID: "$reply-drop-nil-metrics"}
	e.Content.Body = "@runlore:hs note: please record this"
	e.Content.Mentions.UserIDs = []string{self}
	e.Content.RelatesTo.RelType = "m.thread"
	e.Content.RelatesTo.EventID = "$root-ours"

	f.handleMessage(context.Background(), e) // must not panic with no metrics wired
	waitForReplies(t, rep)
}
