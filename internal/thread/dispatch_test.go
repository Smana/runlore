// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

func TestDispatcherRunsWork(t *testing.T) {
	d := NewDispatcher(2, time.Minute, silentLog())
	done := make(chan struct{})
	if !d.Go(context.Background(), func(context.Context) { close(done) }) {
		t.Fatal("Go returned false with free slots")
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("work never ran")
	}
}

func TestDispatcherRefusesWhenSaturated(t *testing.T) {
	d := NewDispatcher(1, time.Minute, silentLog())
	block, running := make(chan struct{}), make(chan struct{})
	if !d.Go(context.Background(), func(context.Context) { close(running); <-block }) {
		t.Fatal("first Go refused")
	}
	<-running
	if d.Go(context.Background(), func(context.Context) { t.Error("must not run when saturated") }) {
		t.Fatal("Go returned true while saturated")
	}
	close(block)
}

func TestDispatcherSlotIsReleasedAfterWork(t *testing.T) {
	d := NewDispatcher(1, time.Minute, silentLog())
	for i := 0; i < 3; i++ {
		done := make(chan struct{})
		if !d.Go(context.Background(), func(context.Context) { close(done) }) {
			t.Fatalf("Go %d refused: the slot was not released", i)
		}
		<-done
		d.Drain(context.Background())
	}
}

func TestDispatcherSlotIsReleasedAfterPanic(t *testing.T) {
	d := NewDispatcher(1, time.Minute, silentLog())
	panicked := make(chan struct{})
	if !d.Go(context.Background(), func(context.Context) { close(panicked); panic("boom") }) {
		t.Fatal("first Go refused")
	}
	<-panicked
	d.Drain(context.Background())
	done := make(chan struct{})
	if !d.Go(context.Background(), func(context.Context) { close(done) }) {
		t.Fatal("slot leaked after a panic")
	}
	<-done
}

func TestDispatcherAppliesTimeout(t *testing.T) {
	d := NewDispatcher(1, 50*time.Millisecond, silentLog())
	var cancelled atomic.Bool
	done := make(chan struct{})
	d.Go(context.Background(), func(ctx context.Context) {
		<-ctx.Done()
		cancelled.Store(true)
		close(done)
	})
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the timeout never fired")
	}
	if !cancelled.Load() {
		t.Fatal("work context was not cancelled by the timeout")
	}
}

func TestDispatcherWorkOutlivesTheCallersContext(t *testing.T) {
	// The caller's context is a request context that is cancelled the moment the
	// handler returns. Work must NOT die with it — only with the dispatcher's own
	// timeout — or every mention would be cancelled before it could be written.
	d := NewDispatcher(1, time.Minute, silentLog())
	ctx, cancel := context.WithCancel(context.Background())
	started, result := make(chan struct{}), make(chan error, 1)
	d.Go(ctx, func(wctx context.Context) {
		close(started)
		time.Sleep(50 * time.Millisecond)
		result <- wctx.Err()
	})
	<-started
	cancel()
	if err := <-result; err != nil {
		t.Fatalf("work context was cancelled with the caller's: %v", err)
	}
}

func TestDispatcherDrainWaitsForInFlightWork(t *testing.T) {
	d := NewDispatcher(2, time.Minute, silentLog())
	var finished atomic.Int32
	release := make(chan struct{})
	for i := 0; i < 2; i++ {
		d.Go(context.Background(), func(context.Context) { <-release; finished.Add(1) })
	}
	go func() { time.Sleep(50 * time.Millisecond); close(release) }()
	d.Drain(context.Background())
	if got := finished.Load(); got != 2 {
		t.Fatalf("Drain returned with %d/2 finished", got)
	}
}

func TestDispatcherDrainIsBounded(t *testing.T) {
	d := NewDispatcher(1, time.Minute, silentLog())
	block := make(chan struct{})
	defer close(block)
	d.Go(context.Background(), func(context.Context) { <-block })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	d.Drain(ctx)
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("Drain blocked %v on a wedged handler; it must honour its context", elapsed)
	}
}
