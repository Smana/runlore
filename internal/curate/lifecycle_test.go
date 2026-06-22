package curate

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestStaleClosesUnlabelledOldArtifacts(t *testing.T) {
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 70, Title: "KB: ancient draft", Labels: []string{"runlore", "triggered"}},
		{Number: 71, Title: "KB: queued", Labels: []string{"runlore", "ready-to-merge"}},
		{Number: 72, Title: "KB: accepted", Labels: []string{"runlore", "accepted"}},
	}}
	// every PR is "stale" by age, but 71/72 are protected by their labels.
	l := Lifecycle{Forge: f, Stale: func(int) bool { return true }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := l.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 1 || f.closed[0] != 70 {
		t.Fatalf("only the stale, unprotected #70 should close, got %v", f.closed)
	}
}
