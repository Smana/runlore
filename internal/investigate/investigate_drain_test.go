package investigate

import (
	"context"
	"testing"
	"time"
)

// blockingInvestigator blocks in Investigate until released, recording the ctx error
// at finish so a test can prove the investigation completed (ctx not cancelled =
// graceful drain) vs was aborted (ctx cancelled).
type blockingInvestigator struct {
	started  chan struct{}
	release  chan struct{}
	finished chan struct{}
	ctxErr   error
}

func newBlockingInvestigator() *blockingInvestigator {
	return &blockingInvestigator{
		started:  make(chan struct{}, 1),
		release:  make(chan struct{}),
		finished: make(chan struct{}),
	}
}

func (b *blockingInvestigator) Investigate(ctx context.Context, _ Request) error {
	b.started <- struct{}{}
	select {
	case <-b.release:
		b.ctxErr = ctx.Err() // expect nil on a graceful drain
		close(b.finished)
		return nil
	case <-ctx.Done():
		b.ctxErr = ctx.Err()
		return ctx.Err()
	}
}

// TestQueueDrainLetsInFlightFinish locks down GO-P1B: Drain waits for the in-flight
// investigation to COMPLETE (its ctx not cancelled), rather than aborting it.
func TestQueueDrainLetsInFlightFinish(t *testing.T) {
	b := newBlockingInvestigator()
	q := NewQueue(b, discardLog)
	workCtx, cancel := context.WithCancel(context.Background()) // NOT cancelled during the drain
	defer cancel()
	go q.Run(workCtx)
	q.Enqueue(Request{Source: SourceAlert, Title: "x"})

	<-b.started // the investigation is in-flight

	drainReturned := make(chan struct{})
	go func() { q.Drain(context.Background()); close(drainReturned) }()

	// Drain must NOT return while the investigation is still running.
	select {
	case <-drainReturned:
		t.Fatal("Drain returned before the in-flight investigation finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(b.release) // let it finish
	<-b.finished

	select {
	case <-drainReturned:
	case <-time.After(2 * time.Second):
		t.Fatal("Drain did not return after the investigation finished")
	}
	if b.ctxErr != nil {
		t.Fatalf("investigation was aborted during graceful drain (ctx err %v); want a clean finish", b.ctxErr)
	}
}

// TestQueueDrainRespectsDeadline ensures a stuck investigation can't hang shutdown:
// Drain returns when its deadline fires even if the in-flight work never finishes.
func TestQueueDrainRespectsDeadline(t *testing.T) {
	b := newBlockingInvestigator()
	q := NewQueue(b, discardLog)
	workCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(workCtx)
	q.Enqueue(Request{Source: SourceAlert, Title: "x"})
	<-b.started

	dctx, dcancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer dcancel()
	start := time.Now()
	q.Drain(dctx) // never released → must return at the deadline
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Drain ignored its deadline with a stuck investigation (took %s)", elapsed)
	}
	close(b.release) // cleanup
}

// TestQueueDrainNoLeaderNoop: Drain is a no-op when the queue isn't running (a
// standby replica has nothing in flight).
func TestQueueDrainNoLeaderNoop(t *testing.T) {
	q := NewQueue(newBlockingInvestigator(), discardLog)
	done := make(chan struct{})
	go func() { q.Drain(context.Background()); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Drain should be an immediate no-op when not running")
	}
}
