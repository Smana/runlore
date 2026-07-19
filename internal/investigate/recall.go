// SPDX-License-Identifier: Apache-2.0

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
// trustworthy answer for the symptom — skipping the slow, paid ReAct loop. From a
// wider candidate set it keeps only entries whose stored resource structurally
// agrees with the alert's workload (a workload-less request agrees only with
// resource-less entries — the scopeless tier), then requires a clear margin over
// the runner-up among those agreeing candidates, plus (in the loop) the
// adversarial verify pass.
// Confidence is derived from those signals, never asserted — scores are
// corpus-dependent and a stale hit must not silently replace an investigation.
type Recall struct {
	Catalog              catalog.ScoredSearcher
	MinScore             float64 // similarity floor for the top hit
	MarginGap            float64 // top hit must beat the runner-up by at least this
	SoloFloor            float64 // confident bar when there is only one hit
	RequireWorkloadMatch bool    // true = exact namespace+workload (also disables scopeless matching); false = namespace-level agreement is enough

	// Hybrid, when non-nil AND it has vectors, switches recall to fused BM25+embedding
	// retrieval gated on COSINE similarity (HybridMinScore / HybridMarginGap) instead
	// of the BM25 thresholds above. nil (or no vectors) ⇒ BM25 recall, unchanged.
	Hybrid          catalog.HybridSearcher
	HybridMinScore  float64 // cosine floor for the top hit (hybrid mode)
	HybridMarginGap float64 // cosine margin over the runner-up (hybrid mode)

	// Rerank, when non-nil, REPLACES the BM25-magnitude fire gate (SoloFloor/MarginGap)
	// with an LLM reranker: it ranks the top-K structurally-agreeing candidates and
	// short-circuits only on the reranker's CALIBRATED, corpus-INDEPENDENT match
	// confidence. nil ⇒ the BM25-magnitude gate is used, byte-for-byte unchanged. The
	// structural pre-filter, outcome decay, confirm and verify steps are identical
	// either way — the reranker only changes WHICH signal decides the fire. Opt-in
	// (catalog.instant_recall.rerank). See the Reranker doc for the full rationale.
	Rerank *Reranker

	Outcome      OutcomeStats // optional; nil ⇒ no outcome decay
	OutcomePrior float64      // k — Beta prior strength for decay (e.g. 2.0)
	OutcomeFloor float64      // reject the recall when the outcome factor drops below this (e.g. 0.5)

	Metrics *telemetry.Metrics // optional; nil-safe — instruments are no-op when provider is unset
	Log     *slog.Logger       // optional; nil-safe — log line omitted when unset
}

// recallCandidateK is the internal lexical candidate window. Recall fetches this
// many hits, then structurally pre-filters them, so an entry matching the alert's
// workload is reachable even when other workloads' entries score higher on symptom
// text alone.
const recallCandidateK = 20

// buildRecallQuery constructs the BM25 query text for a recall lookup from a
// Request. It is the single seam between "what the incident says" and "what the
// lexical index is asked" — extracted so the retrieval quality can be measured
// directly (see recalleval_test.go) rather than only observed through the gate.
//
// It queries the symptom (title + message) PLUS the discriminating structured
// entity: the workload ref (namespace + normalized name) and — when it adds
// anything — the alertname. WHY the enrichment:
//
// A real label-derived Kubernetes alert (KubePodNotReady, pod=harbor-registry-…)
// carries a GENERIC alertname as its title and a terse-or-empty annotation as its
// message; the object that actually identifies the incident (namespace/name) lives
// only in the labels and drove nothing but the structural pre-filter. So the raw
// title+message query is ~1–2 generic tokens against a differently-worded runbook
// ("Harbor Registry Down due to IAM Access Key Quota Limit") — the classic
// vocabulary-mismatch problem. Measured live it scored 0.096, far below the
// production solo_floor (4.0), so recall never fired despite a perfect KB entry;
// the recalleval harness reproduces this (4 identity-in-the-label cases return zero
// BM25 hits). Folding the workload ref into the QUERY — LM/entity expansion in
// front of BM25, the best-evidenced fix — gives the retriever the terms the runbook
// actually shares ("tooling", "harbor-registry"), lifting those cases from zero-hit
// to rank #1.
//
// Deliberate boundaries: the name is NORMALIZED (pod-hash stripped, same function
// as the structural gate) so a per-pod alert matches the controller-family runbook;
// the alertname is appended only when it is NOT already the title, so a label alert
// (title == alertname) is not double-counted; and the ref is additive, so the
// GitOps-failure source — whose title already carries "Kind/Name" — is not harmed
// (its extra namespace token is at worst neutral). Empty parts are dropped so the
// query never carries stray whitespace.
func buildRecallQuery(req Request) string {
	parts := make([]string, 0, 5)
	add := func(s string) {
		if s = strings.TrimSpace(s); s != "" {
			parts = append(parts, s)
		}
	}
	add(req.Title)
	add(req.Message)
	add(req.Workload.Namespace)
	add(providers.NormalizeWorkloadName(req.Workload.Name))
	if an := req.Labels["alertname"]; an != req.Title {
		add(an)
	}
	return strings.Join(parts, " ")
}

// lookup returns the matched entry and a DERIVED confidence when a recall is
// trustworthy enough to short-circuit, else (nil, 0). The BM25 score is always
// recorded (even on rejection) so the thresholds can be tuned from live data. It is a
// thin wrapper over lookupWithUsage with no usage sink — kept for callers (and tests)
// that don't thread the per-investigation token total.
func (r *Recall) lookup(ctx context.Context, req Request) (*catalog.Entry, float64) {
	return r.lookupWithUsage(ctx, req, nil)
}

// lookupWithUsage is lookup plus an optional usage sink: when a Reranker runs, its
// completion's token usage is accumulated into totals (nil ⇒ ignored) so the loop can
// fold the reranker's cost into the per-investigation total. The gate logic is
// identical to lookup — totals is a side channel, never a decision input.
func (r *Recall) lookupWithUsage(ctx context.Context, req Request, totals *providers.UsageTotals) (*catalog.Entry, float64) {
	if r == nil || r.Catalog == nil {
		return nil, 0
	}
	query := buildRecallQuery(req)
	// Mode select: hybrid (BM25+embedding, cosine-gated) when an embedder-backed
	// catalog is live, else BM25 — unchanged. The gate logic below is identical; only
	// the candidate source and the thresholds differ.
	minScore, marginGap, soloFloor := r.MinScore, r.MarginGap, r.SoloFloor
	var hits []catalog.ScoredEntry
	var err error
	if r.Hybrid != nil && r.Hybrid.HasVectors() {
		minScore, marginGap, soloFloor = r.HybridMinScore, r.HybridMarginGap, r.HybridMinScore
		hits, err = r.Hybrid.SearchHybrid(ctx, query, recallCandidateK)
	} else {
		hits, err = r.Catalog.SearchScored(query, recallCandidateK)
	}
	if err != nil || len(hits) == 0 {
		return nil, 0
	}

	// Structural pre-filter: keep candidates whose stored resource agrees with the
	// alert's workload, preserving lexical order. Pre-filtering (rather than checking
	// only the top hit) lets a structurally-correct entry win even when wrong-workload
	// entries score higher on symptom tokens.
	var agreeing []catalog.ScoredEntry
	for _, h := range hits {
		if entryAgrees(req.Workload, h.Entry, r.RequireWorkloadMatch) != matchNone {
			agreeing = append(agreeing, h)
		}
	}
	if len(agreeing) == 0 {
		if r.Metrics != nil {
			r.Metrics.RecallScore.Record(ctx, hits[0].Score) // best lexical score, for miss visibility
		}
		r.reject(ctx, "no_resource_match")
		return nil, 0
	}

	winner := agreeing[0]
	score := winner.Score
	if r.Metrics != nil {
		r.Metrics.RecallScore.Record(ctx, score)
	}

	// The fire gate produces the recalled entry `e` and its confidence `conf`. Two
	// mutually-exclusive gates set them:
	//   - Reranker gate (opt-in): a CALIBRATED, corpus-independent match confidence.
	//   - BM25-magnitude gate (default): the classic margin/solo-floor logic, unchanged.
	var e catalog.Entry
	var conf float64
	var margin float64 // meaningful only in the magnitude gate; 0 (and logged as such) under rerank
	if r.Rerank != nil {
		// Cost guard: the reranker is a paid LLM call placed in front of the "free"
		// short-circuit, so only spend it when retrieval surfaced something plausible —
		// the best structurally-agreeing candidate must clear a trivial retrieval-score
		// floor. Below it, retrieval found nothing worth reranking; fall through to a full
		// investigation WITHOUT making the call (asserted by the cost-guard test).
		if score < r.Rerank.MinScore {
			r.reject(ctx, "rerank_no_signal")
			return nil, 0
		}
		// Rank only the top-K structurally-agreeing candidates (bounded for cost). They
		// are already in lexical order, so agreeing[:k] is the strongest few.
		k := r.Rerank.K
		if k <= 0 || k > len(agreeing) {
			k = len(agreeing)
		}
		matched, mconf, ok := r.Rerank.rank(ctx, req, agreeing[:k], totals)
		// Fire ONLY on a calibrated confidence at or above the (corpus-independent)
		// threshold. A no-match / low-confidence / hallucinated-id verdict falls through —
		// the false-recall guard (a wrong or absent match is worse than no recall).
		if !ok || mconf < r.Rerank.Threshold {
			r.reject(ctx, "rerank_low_confidence")
			return nil, 0
		}
		e = matched
		// The reranker's confidence IS a calibrated match confidence — use it directly as
		// the recall confidence, still capped below 1.0 (a cache hit never asserts
		// certainty). Outcome decay below can only lower it further.
		conf = clampF(mconf, 0, 0.90)
	} else {
		// Gate — margin among the structurally-agreeing candidates: a clear winner for
		// this workload, not merely the top lexical hit. A lone agreeing hit must clear
		// both the solo floor and the min score.
		strength := entryAgrees(req.Workload, winner.Entry, r.RequireWorkloadMatch)
		margin = score
		confident := score >= soloFloor && score >= minScore
		if len(agreeing) > 1 {
			margin = score - agreeing[1].Score
			confident = score >= minScore && margin >= marginGap
		}
		// A scopeless match carries zero structural evidence, so the margin gate alone
		// is too weak: regardless of how many candidates agree, a scopeless winner must
		// ALSO clear the solo floor + min score, exactly like a lone hit.
		if strength == matchScopeless {
			confident = confident && score >= soloFloor && score >= minScore
		}
		if !confident {
			r.reject(ctx, "low_margin")
			return nil, 0
		}
		e = winner.Entry
		conf = deriveRecallConfidence(score, margin, strength)
	}
	// Outcome decay: bias confidence by the entry's resolution track record, and
	// reject (re-investigate) an entry that recalls-but-never-resolves. Fail-safe —
	// a rejected recall just falls through to a full investigation.
	if r.Outcome != nil {
		if counts, err := r.Outcome.OpenCounts(); err == nil {
			f, ok := r.outcomeGate(counts, e.Path)
			if !ok {
				r.reject(ctx, "low_outcome")
				return nil, 0
			}
			conf = clampF(conf*f, 0, 0.90)
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

// nearMiss returns the top STRUCTURALLY-AGREEING candidate for a request, or nil.
// It is consulted by the loop ONLY when the confidence gate did NOT fire: the fire
// path discards every candidate on rejection, yet the structural pre-filter may have
// found an entry whose stored resource agrees with the alert's workload — a
// possibly-related past incident worth surfacing to the model as an UNVERIFIED lead
// (see the seed prompt's near-miss block) rather than throwing away.
//
// It reuses the SAME candidate window + structural pre-filter as lookupWithUsage —
// agreement is the only bar (no margin/solo-floor/outcome gate), because this is a
// hint to shape the prompt, never an answer: the returned entry is framed as
// untrusted and unverified, is subject to the same egress/ingress redaction as alert
// text, and (like instant recall) is gated off under actions.mode=auto by the
// caller. nil-safe; nil ⇒ no agreeing candidate (nothing to inject).
func (r *Recall) nearMiss(ctx context.Context, req Request) *catalog.Entry {
	return r.nearMissExcluding(ctx, req, "")
}

// nearMissExcluding is nearMiss with one entry skipped by path. The verify-rejection
// path passes the entry verify has just refuted against live state: re-offering it as
// a "possibly-related lead" would hand the model the very hypothesis that was proven
// wrong. Every other candidate remains eligible.
func (r *Recall) nearMissExcluding(ctx context.Context, req Request, exclude string) *catalog.Entry {
	if r == nil || r.Catalog == nil {
		return nil
	}
	query := buildRecallQuery(req)
	var hits []catalog.ScoredEntry
	var err error
	if r.Hybrid != nil && r.Hybrid.HasVectors() {
		hits, err = r.Hybrid.SearchHybrid(ctx, query, recallCandidateK)
	} else {
		hits, err = r.Catalog.SearchScored(query, recallCandidateK)
	}
	if err != nil || len(hits) == 0 {
		return nil
	}
	for _, h := range hits {
		if exclude != "" && h.Entry.Path == exclude {
			continue
		}
		if nearMissEntryAgrees(req.Workload, h.Entry, r.RequireWorkloadMatch) != matchNone {
			e := h.Entry
			return &e
		}
	}
	return nil
}

// nearMissAgrees is the structural gate for a NEAR-MISS, and it is deliberately looser
// than resourceAgrees.
//
// resourceAgrees guards INSTANT RECALL, which short-circuits the loop and presents a
// catalog entry as the answer. Refusing two distinct named workloads there is correct
// and must stay: auto-applying a pod's runbook to a HelmRelease alert would be wrong.
//
// A near-miss is not an answer. It is an UNVERIFIED lead, framed as such in the seed
// ("verify against live state, do not assume it applies"), redacted like alert text,
// and already disabled under actions.mode=auto. Judging it by the bar that protects an
// auto-executed action throws away the catalog's value at exactly the moment it is
// needed.
//
// The tier this adds is same-namespace/different-workload. Two named workloads in one
// namespace are very often the SAME incident seen from different objects, one step
// apart in the ownership chain: an alert on the HelmRelease `tooling/harbor` and a past
// incident filed on the pod `tooling/harbor-registry` are the same failure. Under
// resourceAgrees that pair is matchNone — so the entry is invisible to recall AND to
// the near-miss, and a full paid investigation runs beside a catalog that holds the
// answer. Observed live.
//
// requireWorkload is an explicit operator demand for exact agreement; it is honoured
// here too, so an operator who has asked for strictness does not silently get this tier.
// nearMissEntryAgrees is where the three recall fixes compose: a near-miss is judged by
// the LOOSE gate (nearMissAgrees — it is a lead, not an answer) applied to BOTH resources
// an entry carries (the fault locus, and the resource the originating alert fired on).
// The stronger tier wins.
func nearMissEntryAgrees(reqW providers.Workload, e catalog.Entry, requireWorkload bool) matchStrength {
	best := nearMissAgrees(reqW, e.Resource, requireWorkload)
	if e.AlertResource != "" {
		if s := nearMissAgrees(reqW, e.AlertResource, requireWorkload); s > best {
			best = s
		}
	}
	return best
}

func nearMissAgrees(reqW providers.Workload, entryResource string, requireWorkload bool) matchStrength {
	if s := resourceAgrees(reqW, entryResource, requireWorkload); s != matchNone {
		return s
	}
	if requireWorkload {
		return matchNone
	}
	if reqW.Namespace == "" || entryResource == "" {
		return matchNone
	}
	// Same namespace, different named workload — a lead, never an answer. The
	// namespace boundary is the limit: a hint from an unrelated namespace is noise.
	if strings.HasPrefix(entryResource, reqW.Namespace+"/") {
		return matchNamespace
	}
	return matchNone
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
	// matchScopeless is the weakest agreeing tier: BOTH sides carry no workload
	// scope at all. It exists so workload-less incident sources (PagerDuty carries
	// no Kubernetes namespace/name) can still recall hand-written runbooks and
	// curated Playbooks that are themselves resource-less. It provides zero
	// structural evidence, so the gate and the derived confidence treat it as
	// strictly weaker than any scoped tier.
	matchScopeless
	matchNamespace
	matchExact
)

// entryAgrees reports how strongly an alert's workload agrees with an ENTRY, matching
// on EITHER resource the entry carries and keeping the stronger tier:
//
//   - Resource      — where the fault was FOUND (the investigation's conclusion)
//   - AlertResource — where the ALERT that produced the entry FIRED (when different)
//
// Recall arrives with an alert's workload and nothing else. An entry indexed only by
// its fault locus is therefore unreachable from the alert that would surface it, even
// though that alert is exactly what produced it. Matching on either closes the loop
// without weakening anything: AlertResource is an ADDITIONAL way to agree, never a
// replacement, so hand-written entries and every entry curated before this field
// existed (AlertResource == "") behave precisely as they did.
func entryAgrees(reqW providers.Workload, e catalog.Entry, requireWorkload bool) matchStrength {
	best := resourceAgrees(reqW, e.Resource, requireWorkload)
	if e.AlertResource != "" {
		if s := resourceAgrees(reqW, e.AlertResource, requireWorkload); s > best {
			best = s
		}
	}
	return best
}

// resourceAgrees reports how strongly the alert's workload agrees with an entry's
// stored resource. requireWorkload demands an exact namespace+name match — which
// also disables the scopeless tier (a scopeless pair can never provide one).
func resourceAgrees(reqW providers.Workload, entryResource string, requireWorkload bool) matchStrength {
	// Scopeless tier: a request with NO workload at all may agree ONLY with entries
	// that are themselves resource-less. This never loosens scoped matching — a
	// request carrying ANY scope hint (namespace, or even a bare name) falls through
	// to the unchanged rules below, where a resource-less entry never agrees.
	if reqW.Namespace == "" && reqW.Name == "" {
		if entryResource == "" && !requireWorkload {
			return matchScopeless
		}
		return matchNone
	}
	if entryResource == "" || reqW.Namespace == "" {
		return matchNone
	}
	// Strip the volatile pod-hash suffix off the NAME segment on BOTH sides before
	// the structural comparison. A pod-scoped alert (KubePodNotReady carries only a
	// `pod` label — no deployment/workload label) arrives with the full pod name,
	// e.g. tooling/harbor-registry-59598dbd57-ltkzw, while the KB entry stores the
	// normalized controller family tooling/harbor-registry. Without normalization the
	// two never agree → the recall is rejected (no_resource_match) and a full paid
	// investigation runs despite a perfect KB entry (live-found). Normalizing BOTH
	// sides also matches an entry written before the curator-side CORE-681 fix, which
	// may itself still carry a pod hash. Only the name is normalized — never the
	// namespace — and the normalization is the same idempotent one the dedup path
	// uses, so two distinct workloads never collapse together.
	if normalizeResourceRef(reqW.Ref()) == normalizeResourceRef(entryResource) {
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

// normalizeResourceRef strips the volatile pod-hash suffix from the NAME segment of
// a "namespace/name" resource ref, leaving a bare "namespace" (or "") untouched. It
// splits on the first "/" only — Kubernetes namespaces and names never contain a
// slash — so the namespace is never normalized, only the name. It delegates to the
// shared providers.NormalizeWorkloadName so the recall gate and the curator dedup
// path strip identically (and idempotently).
func normalizeResourceRef(ref string) string {
	ns, name, ok := strings.Cut(ref, "/")
	if !ok {
		return ref // bare namespace (or empty): no name segment to normalize
	}
	return ns + "/" + providers.NormalizeWorkloadName(name)
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

// outcomeGate applies outcome decay (Gate 3) to ONE candidate entry, given the
// OpenCounts snapshot the caller fetched once per lookup. It returns the decay
// factor to multiply into the recall confidence, and ok=false when the entry's
// track record falls below OutcomeFloor (the caller must not fire this entry).
// Fail-safe: an entry with no recall/feedback history returns (1, true) —
// absence of evidence never blocks a recall.
func (r *Recall) outcomeGate(counts map[string]outcome.Aggregate, path string) (float64, bool) {
	agg, ok := counts[path]
	if !ok {
		return 1, true
	}
	f := outcomeFactor(agg.Recalls, agg.Resolved, agg.FeedbackUp, agg.FeedbackDown, r.OutcomePrior)
	return f, f >= r.OutcomeFloor
}

// outcomeFactor decays a recall's confidence by its track record as the posterior
// mean of a symmetric Beta(k/2, k/2) prior over the success rate:
//
//	factor = (resolved + up + k/2) / (recalls + up + down + k)
//
// Human 👍/👎 feedback (up/down) are extra Bernoulli observations in the SAME
// posterior — a 👍 is one success, a 👎 one failure, each weighing exactly like a
// resolved/unresolved recall. That is deliberate: feedback is the only ground
// truth non-resolvable sources (GitOps failures) can ever accumulate, since their
// recalls are excluded from resolve-based decay (see outcome.applyOpenLocked).
//
// k is the total pseudo-observation count (the prior strength). With the default
// k=2 this is the documented Beta(1,1) posterior (resolved+up+1)/(recalls+up+down+2):
// an entry with no history sits at the prior mean 0.5, a consistently-resolving one
// asymptotes toward 1 without ever reaching it, and one that recalls-but-never-
// resolves decays fast (0.333 after a single unresolved recall — or a single 👎).
// Always in (0, 1) for k > 0, resolved ≤ recalls and non-negative votes. Entries
// with no recall or feedback history never reach this gate (they are absent from
// OpenCounts), so a brand-new entry is not punished by the 0.5 prior mean.
func outcomeFactor(recalls, resolved, up, down int, k float64) float64 {
	return (float64(resolved+up) + k/2) / (float64(recalls+up+down) + k)
}

// deriveRecallConfidence turns the match signals into an explainable confidence,
// capped below 1.0 — a cache hit never asserts certainty. (Constants are the shape;
// tune via recall_score / recall_rejections.)
func deriveRecallConfidence(score, margin float64, strength matchStrength) float64 {
	base := 0.55
	if score > 0 {
		base = 0.55 + 0.30*clampF(margin/score, 0, 1) // decisive winner → up to 0.85
	}
	// Scopeless is the weakest tier: with zero structural evidence the confidence
	// rides on the lexical margin alone, so it starts lower AND is capped below
	// every scoped tier — a workload-less recall must never look as trustworthy as
	// a namespace- or workload-anchored one.
	if strength == matchScopeless {
		return clampF(base-0.10, 0.45, 0.70)
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
	inv := providers.Investigation{
		Title:         req.Title,
		Confidence:    confidence,
		RootCauses:    []providers.Hypothesis{rc},
		Unresolved:    []string{"recalled from the catalog without a fresh investigation — confirm it still applies"},
		Recalled:      true,
		RecalledEntry: e.Path,
		Resource:      req.Workload,
	}
	// Quote the entry's human-reviewed cause + resolution so the notification makes the
	// cache hit substantive (the "⚡ instant recall" block), not just a low-confidence
	// finding. The recall track record (resolve rate) is filled in downstream by
	// onInvestigationComplete, which has the outcome ledger. Best-effort: empty sections
	// simply render nothing.
	if cause, resolution := e.Section("Cause"), e.Section("Resolution"); cause != "" || resolution != "" {
		inv.Prior = &providers.PriorKnowledge{Cause: cause, Resolution: resolution, EntryPath: e.Path}
	}
	stampRequestFacts(&inv, req) // same trigger-time facts as the full-loop site, factored to prevent drift
	return inv
}
