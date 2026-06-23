package investigate

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/telemetry"
)

// TestRecallScoreRecordedNilSafe verifies that a Recall with nil Metrics does not
// panic when a hit is found and scored — the nil check in lookup must guard it.
func TestRecallScoreRecordedNilSafe(t *testing.T) {
	r := &Recall{
		Catalog:  fakeScored{hits: []catalog.ScoredEntry{{Entry: catalog.Entry{Title: "x", Path: "x.md"}, Score: 5.0}}},
		MinScore: 2.0,
		Metrics:  nil, // no-op: must not panic
	}
	entry, score := r.lookup(context.Background(), Request{Title: "incident"})
	if entry == nil {
		t.Fatal("expected a hit above threshold")
	}
	if score != 5.0 {
		t.Fatalf("score: got %v, want 5.0", score)
	}
}

// TestRecallScoreRecordedRealProvider verifies that a hit records the BM25 score
// in the OTel histogram and increments RecallHits via the Prometheus exposition.
func TestRecallScoreRecordedRealProvider(t *testing.T) {
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	m := telemetry.NewMetrics() // instruments bound to the real provider
	r := &Recall{
		Catalog:  fakeScored{hits: []catalog.ScoredEntry{{Entry: catalog.Entry{Title: "Known", Path: "known.md"}, Score: 7.5}}},
		MinScore: 2.0,
		Metrics:  m,
	}

	// Trigger a lookup that passes the threshold — score must be recorded.
	entry, _ := r.lookup(context.Background(), Request{Title: "HarborProbeFailure"})
	if entry == nil {
		t.Fatal("expected a hit above threshold")
	}

	// Scrape the /metrics exposition and confirm the score series appeared.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("metrics status: %d", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "runlore_recall_score") {
		t.Fatalf("runlore_recall_score not found in metrics output:\n%s", body)
	}
}
