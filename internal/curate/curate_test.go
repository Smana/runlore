package curate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
)

type countingPass struct {
	ran *int
	err error
}

func (p countingPass) Run(context.Context) error { *p.ran++; return p.err }

func TestAgentRunsAllPasses(t *testing.T) {
	n := 0
	a := Agent{Passes: []Pass{
		countingPass{ran: &n},
		countingPass{ran: &n, err: errors.New("boom")},
		countingPass{ran: &n},
	}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	a.Run(context.Background())
	if n != 3 {
		t.Fatalf("all 3 passes must run even when one errors, ran=%d", n)
	}
}

// compile-time: the grooming passes satisfy Pass.
var (
	_ Pass = Dedup{}
	_ Pass = Queue{}
	_ Pass = Lifecycle{}
)
