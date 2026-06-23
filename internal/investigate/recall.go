package investigate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// Recall short-circuits an investigation when the knowledge catalog already has a
// trustworthy answer for the symptom — skipping the slow, paid ReAct loop. A recall
// must clear three gates: a BM25 margin over the runner-up (corpus-portable, unlike
// an absolute MinScore), structural agreement on the affected resource, and (in the
// loop) the adversarial verify pass. Confidence is derived from those signals, never
// asserted — BM25 scores are corpus-dependent and a stale hit must not silently
// replace an investigation.
type Recall struct {
	Catalog              catalog.ScoredSearcher
	MinScore             float64 // similarity floor for the top hit
	MarginGap            float64 // top hit must beat the runner-up by at least this
	SoloFloor            float64 // confident bar when there is only one hit
	RequireWorkloadMatch bool    // true = exact namespace+workload; false = namespace-level agreement is enough

	Metrics *telemetry.Metrics // optional; nil-safe — instruments are no-op when provider is unset
	Log     *slog.Logger       // optional; nil-safe — log line omitted when unset
}

// lookup returns the matched entry and a DERIVED confidence when a recall is
// trustworthy enough to short-circuit, else (nil, 0). The BM25 score is always
// recorded (even on rejection) so the thresholds can be tuned from live data.
func (r *Recall) lookup(ctx context.Context, req Request) (*catalog.Entry, float64) {
	if r == nil || r.Catalog == nil {
		return nil, 0
	}
	// Query the symptom (title + message); severity/reason is noise for matching.
	// k=2 so the top hit can be required to be an unambiguous winner.
	hits, err := r.Catalog.SearchScored(strings.TrimSpace(req.Title+" "+req.Message), 2)
	if err != nil || len(hits) == 0 {
		return nil, 0
	}
	score := hits[0].Score
	if r.Metrics != nil {
		r.Metrics.RecallScore.Record(ctx, score)
	}

	// Gate 1 — similarity margin. A relative margin over the runner-up is portable
	// across corpus sizes; a lone hit needs a higher solo floor.
	margin := score
	confident := score >= r.SoloFloor && score >= r.MinScore // a lone hit must clear both floors
	if len(hits) > 1 {
		margin = score - hits[1].Score
		confident = score >= r.MinScore && margin >= r.MarginGap
	}
	if !confident {
		r.reject(ctx, "low_margin")
		return nil, 0
	}

	// Gate 2 — structural agreement: the alert's workload must match the entry's
	// stored resource. This attacks "symptoms are many-to-one with root causes".
	e := hits[0].Entry
	strength := resourceAgrees(req.Workload, e.Resource, r.RequireWorkloadMatch)
	if strength == matchNone {
		r.reject(ctx, "no_resource_match")
		return nil, 0
	}

	conf := deriveRecallConfidence(score, margin, strength)
	if r.Log != nil {
		r.Log.Info("instant recall decision",
			"alert", req.Title, "entry_id", e.Path, "score", score, "margin", margin, "confidence", conf)
	}
	return &e, conf
}

// reject records a rejection reason (nil-safe).
func (r *Recall) reject(ctx context.Context, reason string) {
	if r.Metrics != nil {
		r.Metrics.RecallRejections.Add(ctx, 1, metric.WithAttributes(attribute.String("reason", reason)))
	}
}

type matchStrength int

const (
	matchNone matchStrength = iota
	matchNamespace
	matchExact
)

// canonicalResource renders a workload as "namespace/name", or just "namespace"
// when the name is unknown (common for alert-triggered investigations).
func canonicalResource(w providers.Workload) string {
	if w.Namespace == "" {
		return ""
	}
	if w.Name == "" {
		return w.Namespace
	}
	return w.Namespace + "/" + w.Name
}

// resourceAgrees reports how strongly the alert's workload agrees with an entry's
// stored resource. requireWorkload demands an exact namespace+name match.
func resourceAgrees(reqW providers.Workload, entryResource string, requireWorkload bool) matchStrength {
	if entryResource == "" || reqW.Namespace == "" {
		return matchNone
	}
	if canonicalResource(reqW) == entryResource {
		return matchExact
	}
	if requireWorkload {
		return matchNone
	}
	if entryResource == reqW.Namespace || strings.HasPrefix(entryResource, reqW.Namespace+"/") {
		return matchNamespace
	}
	return matchNone
}

func clampF(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

// deriveRecallConfidence turns the match signals into an explainable confidence,
// capped below 1.0 — a cache hit never asserts certainty. (Constants are the shape;
// tune via recall_score / recall_rejections.)
func deriveRecallConfidence(score, margin float64, strength matchStrength) float64 {
	base := 0.55
	if score > 0 {
		base = 0.55 + 0.30*clampF(margin/score, 0, 1) // decisive winner → up to 0.85
	}
	if strength == matchExact {
		base += 0.05
	}
	return clampF(base, 0.50, 0.90)
}

// recalledInvestigation builds findings directly from a catalog entry, using the
// derived recall confidence. It is explicit that this is a recalled match, not a
// fresh investigation.
func recalledInvestigation(req Request, e catalog.Entry, confidence float64) providers.Investigation {
	rc := providers.Hypothesis{
		Summary:    e.Title + " — " + e.Description,
		Confidence: confidence,
		Evidence:   []string{fmt.Sprintf("instant recall: matched knowledge-base entry %q", e.Path)},
	}
	return providers.Investigation{
		Title:      req.Title,
		Confidence: confidence,
		RootCauses: []providers.Hypothesis{rc},
		Unresolved: []string{"recalled from the catalog without a fresh investigation — confirm it still applies"},
	}
}
