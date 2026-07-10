// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func lifecycleNow() time.Time { return time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC) }

func TestLifecycleClosesOnlyAgedUnprotected(t *testing.T) {
	now := lifecycleNow()
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 1, Labels: []string{"runlore"}, UpdatedAt: now.Add(-40 * 24 * time.Hour)},             // aged → close
		{Number: 2, Labels: []string{"runlore"}, UpdatedAt: now.Add(-2 * time.Hour)},                   // fresh → keep
		{Number: 3, Labels: []string{"runlore", "accepted"}, UpdatedAt: now.Add(-40 * 24 * time.Hour)}, // aged but protected → keep
	}}
	l := Lifecycle{Forge: f, StaleAfter: 30 * 24 * time.Hour, Now: func() time.Time { return now }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.closed) != 1 || f.closed[0] != 1 {
		t.Fatalf("only the aged unprotected PR #1 should close, got %v", f.closed)
	}
}

func TestLifecycleZeroStaleAfterDisables(t *testing.T) {
	now := lifecycleNow()
	f := &fakeForge{prs: []providers.CuratedIssue{{Number: 1, Labels: []string{"runlore"}, UpdatedAt: now.Add(-365 * 24 * time.Hour)}}}
	l := Lifecycle{Forge: f, StaleAfter: 0, Now: func() time.Time { return now }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.closed) != 0 {
		t.Fatalf("StaleAfter==0 must close nothing, got %v", f.closed)
	}
}

func TestLifecycleUnknownAgeNotClosed(t *testing.T) {
	now := lifecycleNow()
	f := &fakeForge{prs: []providers.CuratedIssue{{Number: 1, Labels: []string{"runlore"}}}} // zero UpdatedAt
	l := Lifecycle{Forge: f, StaleAfter: 24 * time.Hour, Now: func() time.Time { return now }, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.closed) != 0 {
		t.Fatalf("a PR with unknown age must never close, got %v", f.closed)
	}
}
