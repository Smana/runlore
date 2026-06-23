package investigate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"
)

// OutcomeStats reports per-entry recall outcomes for confidence decay.
// *outcome.Ledger satisfies it.
type OutcomeStats interface {
	OpenCounts() (map[string]outcome.Aggregate, error)
}

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

	Outcome      OutcomeStats // optional; nil ⇒ no outcome decay
	OutcomePrior float64      // k — Beta prior strength for decay (e.g. 2.0)
	OutcomeFloor float64      // reject the recall when the outcome factor drops below this (e.g. 0.5)

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
	// Outcome decay: bias confidence by the entry's resolution track record, and
	// reject (re-investigate) an entry that recalls-but-never-resolves. Fail-safe —
	// a rejected recall just falls through to a full investigation.
	if r.Outcome != nil {
		if counts, err := r.Outcome.OpenCounts(); err == nil {
			if agg, ok := counts[e.Path]; ok { // only entries with recall history
				f := outcomeFactor(agg.Recalls, agg.Resolved, r.OutcomePrior)
				if f < r.OutcomeFloor {
					r.reject(ctx, "low_outcome")
					return nil, 0
				}
				conf = clampF(conf*f, 0, 0.90)
			}
		} else if r.Log != nil {
			r.Log.Warn("recall: outcome stats unavailable; skipping decay", "err", err)
		}
	}
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

// resourceAgrees reports how strongly the alert's workload agrees with an entry's
// stored resource. requireWorkload demands an exact namespace+name match.
func resourceAgrees(reqW providers.Workload, entryResource string, requireWorkload bool) matchStrength {
	if entryResource == "" || reqW.Namespace == "" {
		return matchNone
	}
	if reqW.Ref() == entryResource {
		return matchExact
	}
	if requireWorkload {
		return matchNone
	}
	// Namespace-level agreement only when one side is a bare namespace — never two
	// distinct named workloads (that would defeat disambiguation).
	if entryResource == reqW.Namespace { // entry is a bare namespace; reqW is in it
		return matchNamespace
	}
	if reqW.Name == "" && strings.HasPrefix(entryResource, reqW.Namespace+"/") { // reqW is a bare namespace; entry named in it
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

// outcomeFactor decays a recall's confidence by its track record using an
// optimistic Beta prior: an entry with no history (or that always resolves)
// scores 1.0; one that recalls-but-never-resolves decays toward 0. k is the
// prior strength. Always in (0, 1] provided resolved ≤ recalls and k > 0.
func outcomeFactor(recalls, resolved int, k float64) float64 {
	return (float64(resolved) + k) / (float64(recalls) + k)
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
		Title:         req.Title,
		Confidence:    confidence,
		RootCauses:    []providers.Hypothesis{rc},
		Unresolved:    []string{"recalled from the catalog without a fresh investigation — confirm it still applies"},
		Recalled:      true,
		RecalledEntry: e.Path,
		Fingerprint:   req.Fingerprint,
		Resource:      req.Workload,
	}
}
