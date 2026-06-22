package main

import (
	"context"
	"testing"
)

func TestShellStepRunnerEcho(t *testing.T) {
	sr := shellStepRunner{}
	if err := sr.Run(context.Background(), "true"); err != nil {
		t.Fatalf("true should succeed: %v", err)
	}
	if err := sr.Run(context.Background(), "false"); err == nil {
		t.Fatal("false should fail")
	}
}
