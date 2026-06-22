package investigate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// Recall short-circuits an investigation when the knowledge catalog already has a
// high-confidence answer for the symptom — skipping the slow, paid ReAct loop. It
// is opt-in (off by default) with a tunable MinScore, since BM25 scores are
// corpus-dependent and a stale hit shouldn't silently replace an investigation.
type Recall struct {
	Catalog  catalog.ScoredSearcher
	MinScore float64
	Metrics  *telemetry.Metrics // optional; nil-safe — instruments are no-op when provider is unset
	Log      *slog.Logger       // optional; nil-safe — log line omitted when unset
}

// lookup returns the best catalog entry for the request and its score if it meets
// the threshold, else (nil, 0). The BM25 score is always recorded in the histogram
// when hits exist, even when below threshold, to allow threshold tuning.
func (r *Recall) lookup(ctx context.Context, req Request) (*catalog.Entry, float64) {
	if r == nil || r.Catalog == nil {
		return nil, 0
	}
	// Query the symptom (title + message); severity/reason is noise for matching.
	hits, err := r.Catalog.SearchScored(strings.TrimSpace(req.Title+" "+req.Message), 1)
	if err != nil || len(hits) == 0 {
		return nil, 0
	}
	score := hits[0].Score
	// Always record score at the decision point — needed to tune MinScore threshold.
	if r.Metrics != nil {
		r.Metrics.RecallScore.Record(ctx, score)
	}
	if r.Log != nil {
		r.Log.Info("kb recall decision",
			"alert", req.Title, "entry_id", hits[0].Entry.Path, "score", score,
			"min_score", r.MinScore, "hit", score >= r.MinScore)
	}
	if score < r.MinScore {
		return nil, 0
	}
	e := hits[0].Entry
	return &e, score
}

// recalledInvestigation builds findings directly from a catalog entry. It is
// explicit that this is a recalled match, not a fresh investigation.
func recalledInvestigation(req Request, e catalog.Entry) providers.Investigation {
	rc := providers.Hypothesis{
		Summary:    e.Title + " — " + e.Description,
		Confidence: 0.8,
		Evidence:   []string{fmt.Sprintf("instant recall: matched knowledge-base entry %q", e.Path)},
	}
	return providers.Investigation{
		Title:      req.Title,
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{rc},
		Unresolved: []string{"recalled from the catalog without a fresh investigation — confirm it still applies"},
	}
}
