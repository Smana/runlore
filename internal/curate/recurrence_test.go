package curate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

type memStore struct {
	counts  map[string]int
	saveErr error // when set, Save fails (and does not persist) — simulates a flaky store
}

// Load returns a COPY (like a real forge-backed store), so an increment on the
// loaded map does not mutate the persisted state unless Save succeeds.
func (m *memStore) Load(context.Context) (map[string]int, error) {
	cp := make(map[string]int, len(m.counts))
	for k, v := range m.counts {
		cp[k] = v
	}
	return cp, nil
}
func (m *memStore) Save(_ context.Context, c map[string]int) error {
	if m.saveErr != nil {
		return m.saveErr
	}
	m.counts = c
	return nil
}

// gapForge records the issue titles it was asked to open.
type gapForge struct {
	*recordingForge
	openedTitles []string
}

func (g *gapForge) OpenIssue(_ context.Context, inv providers.Investigation) (providers.Ref, error) {
	g.openedTitles = append(g.openedTitles, inv.Title)
	return providers.Ref{URL: "https://github.com/x/y/issues/1"}, nil
}

func TestRecurrenceOpensGapIssueOnThreshold(t *testing.T) {
	store := &memStore{counts: map[string]int{"flux gitrepository not found": 2}} // already seen twice
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Store: store, Threshold: 3, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := r.Observe(context.Background(), "flux gitrepository not found"); err != nil {
		t.Fatal(err)
	}
	if len(gf.openedTitles) != 1 {
		t.Fatalf("want 1 knowledge-gap issue at the threshold, got %d", len(gf.openedTitles))
	}
	if store.counts["flux gitrepository not found"] != 3 {
		t.Fatalf("count not persisted: %v", store.counts)
	}
}

func TestRecurrenceSaveFailureDoesNotDoubleOpen(t *testing.T) {
	// Save fails the first time. Because the count is persisted BEFORE the issue is
	// opened, a Save failure means NO issue is opened that run (and the count isn't
	// persisted) — so the next run retries and opens exactly once. Never twice.
	store := &memStore{counts: map[string]int{"p": 2}, saveErr: errors.New("store unavailable")}
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Store: store, Threshold: 3, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}

	if err := r.Observe(context.Background(), "p"); err == nil {
		t.Fatal("want a save error on the first Observe")
	}
	if len(gf.openedTitles) != 0 {
		t.Fatalf("must NOT open an issue when Save fails, got %d", len(gf.openedTitles))
	}
	// Retry with a working store → opens exactly once.
	store.saveErr = nil
	if err := r.Observe(context.Background(), "p"); err != nil {
		t.Fatal(err)
	}
	if len(gf.openedTitles) != 1 {
		t.Fatalf("want exactly 1 issue after the retry, got %d", len(gf.openedTitles))
	}
}

func TestRecurrenceBelowThresholdNoIssue(t *testing.T) {
	store := &memStore{counts: map[string]int{}}
	gf := &gapForge{recordingForge: &recordingForge{}}
	r := Recurrence{Forge: gf, Store: store, Threshold: 3, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := r.Observe(context.Background(), "new pattern"); err != nil {
		t.Fatal(err)
	}
	if len(gf.openedTitles) != 0 {
		t.Fatalf("below threshold must not open an issue")
	}
}
