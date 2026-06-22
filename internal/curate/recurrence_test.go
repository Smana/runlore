package curate

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

type memStore struct{ counts map[string]int }

func (m *memStore) Load(context.Context) (map[string]int, error)   { return m.counts, nil }
func (m *memStore) Save(_ context.Context, c map[string]int) error { m.counts = c; return nil }

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
