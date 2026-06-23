package curator

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// fakeForge records OpenPR / Comment calls and serves a fixed open-PR list.
type fakeForge struct {
	openedPR  *providers.KBEntry
	commented []int
	openPRs   []providers.CuratedIssue
}

func (f *fakeForge) OpenPR(_ context.Context, e providers.KBEntry) (providers.Ref, error) {
	f.openedPR = &e
	return providers.Ref{URL: "https://github.com/x/y/pull/2"}, nil
}
func (f *fakeForge) ListPRsByLabel(_ context.Context, _ string) ([]providers.CuratedIssue, error) {
	return f.openPRs, nil
}
func (f *fakeForge) Comment(_ context.Context, n int, _ string) error {
	f.commented = append(f.commented, n)
	return nil
}

func newCurator(f *fakeForge, cat catalog.ScoredSearcher) *Curator {
	return &Curator{
		Forge: f, Catalog: cat, DupScore: 5.0, MinConfidence: 0.75,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func goodFinding() providers.Investigation {
	return providers.Investigation{
		Title: "HarborRegistryDown", Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{
			Summary: "IAM quota exceeded", Confidence: 0.9,
			Evidence: []string{"CreateContainerConfigError"}, ChangeRef: "xplane-harbor", SuggestedAction: "delete a key",
		}},
	}
}

func TestCurateNovelHighQualityOpensPR(t *testing.T) {
	f := &fakeForge{}
	ref, err := newCurator(f, fakeScored{}).Curate(context.Background(), goodFinding())
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR == nil || ref.URL == "" {
		t.Fatalf("expected a PR, got %+v / %s", f.openedPR, ref.URL)
	}
}

func TestCurateDuplicateCoalescesNoPR(t *testing.T) {
	// An open PR already covers this incident (matching title), and the catalog does
	// NOT (fakeScored{} returns no hit) — so we fall through to the open-PR coalesce
	// path, which comments on the existing PR rather than filing a new one.
	f := &fakeForge{openPRs: []providers.CuratedIssue{{Number: 48, Title: "KB: HarborRegistryDown"}}}
	ref, err := newCurator(f, fakeScored{}).Curate(context.Background(), goodFinding())
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR != nil {
		t.Fatalf("duplicate must NOT open a PR, got %+v", f.openedPR)
	}
	if len(f.commented) == 0 || f.commented[0] != 48 {
		t.Fatalf("duplicate should coalesce by commenting on PR #48, got %v", f.commented)
	}
	if ref.URL != "" {
		t.Fatalf("duplicate ref should be empty, got %s", ref.URL)
	}
}

func TestCurateCatalogDuplicateDropsSilently(t *testing.T) {
	// The catalog already has this knowledge → drop without filing OR commenting.
	f := &fakeForge{}
	ref, err := newCurator(f, fakeScored{score: 9, title: "HarborRegistryDown"}).Curate(context.Background(), goodFinding())
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR != nil || len(f.commented) != 0 || ref.URL != "" {
		t.Fatalf("catalog duplicate must drop silently, got pr=%+v comment=%v ref=%s", f.openedPR, f.commented, ref.URL)
	}
}

func TestCurateCatalogErrorFallsThroughToPR(t *testing.T) {
	// A catalog search error must NOT block curation: it logs a warning and falls
	// through (fail-open) to the open-PR dedup + quality gate. With no matching open
	// PR and a good finding, that yields a PR.
	f := &fakeForge{}
	ref, err := newCurator(f, fakeScored{err: errors.New("index unavailable")}).Curate(context.Background(), goodFinding())
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR == nil || ref.URL == "" {
		t.Fatalf("catalog error should fall through to a PR, got %+v / %s", f.openedPR, ref.URL)
	}
}

func TestCurateLowQualityDropsNoArtifact(t *testing.T) {
	f := &fakeForge{}
	weak := providers.Investigation{Title: "vague", Confidence: 0.3} // below bar: no root cause, low conf
	ref, err := newCurator(f, fakeScored{}).Curate(context.Background(), weak)
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR != nil || len(f.commented) != 0 || ref.URL != "" {
		t.Fatalf("low-quality finding must produce no repo artifact, got pr=%+v comment=%v ref=%s", f.openedPR, f.commented, ref.URL)
	}
}

func TestCurateSkipsRecalled(t *testing.T) {
	f := &fakeForge{}
	inv := goodFinding() // high quality: would normally open a PR
	inv.Recalled = true  // but it's a cache hit, not novel
	ref, err := newCurator(f, fakeScored{}).Curate(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR != nil || ref.URL != "" {
		t.Fatalf("a recalled finding must not be curated, got pr=%+v ref=%s", f.openedPR, ref.URL)
	}
}

func TestCurateDedupScoreNilMetricsSafe(t *testing.T) {
	// newCurator leaves Metrics nil; recording the dedup score must not panic.
	c := newCurator(&fakeForge{}, fakeScored{score: 2.0, title: "Some entry"})
	if _, err := c.Curate(context.Background(), goodFinding()); err != nil {
		t.Fatal(err)
	}
}

func TestCurateRecordsDedupScore(t *testing.T) {
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	c := newCurator(&fakeForge{}, fakeScored{score: 2.0, title: "Some entry"}) // below DupScore → records, then continues
	c.Metrics = telemetry.NewMetrics()
	if _, err := c.Curate(context.Background(), goodFinding()); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if !strings.Contains(rec.Body.String(), "runlore_curation_dedup_score") {
		t.Fatalf("runlore_curation_dedup_score not found in metrics output:\n%s", rec.Body.String())
	}
}
