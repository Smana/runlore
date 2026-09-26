// SPDX-License-Identifier: Apache-2.0

// Package curator is the file-time learning gate: it dedups a finding against the
// catalog and open PRs, gates on quality, and drafts a merge-ready PR for novel,
// quality findings. Uncertain/low-quality findings produce NO repo artifact (the
// chat delivery already informed the human). It never opens issues — the only
// issues are knowledge-gap issues, opened by the curate agent (Phase 2).
package curator

import (
	"cmp"
	"context"
	"fmt"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/kbvalidate"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// ConfirmationSink records that a FRESH investigation independently reached an
// existing catalog entry's conclusion (identical DupFingerprint) — the recovery
// evidence that lets a contested entry's outcome factor climb back (see
// outcome.Ledger.Confirm, which satisfies this). Optional and nil-safe, like
// Metrics: a curator without a ledger simply keeps today's behavior.
type ConfirmationSink interface {
	Confirm(entry, triggerKey, dupFP string, at time.Time) error
}

// Related-knowledge bounds: how many BM25 neighbors a drafted PR shows the
// reviewer, and the noise floor below which a "neighbor" is lexical debris.
// The floor is deliberately low — BM25 absolute scores are corpus-dependent
// (see the recall margin gate), so the top-k cap does the real limiting and
// the floor only drops near-zero matches on genuinely novel incidents.
const (
	relatedK     = 5
	relatedFloor = 0.2
)

// Curator is the file-time learning gate. It dedups, quality-gates, and drafts a
// merge-ready PR for novel quality findings; everything else produces no repo
// artifact.
type Curator struct {
	Forge         providers.CurationForge
	Catalog       catalog.ScoredSearcher // nil ⇒ no catalog dedup
	Metrics       *telemetry.Metrics     // optional; nil-safe — dedup score unrecorded when unset
	DupScore      float64                // catalog BM25 dup threshold
	MinConfidence float64                // quality gate: minimum overall confidence
	// SkipVerdicts holds verdicts that must NOT draft a KB PR (e.g. no_action).
	// A nil/empty map draws no distinction — every verdict is eligible, preserving
	// pre-gate behaviour.
	SkipVerdicts  map[providers.Verdict]bool
	Confirmations ConfirmationSink // optional; nil-safe — dedup matches are recovery evidence
	// Decider optionally replaces the BM25 dup_score gate with a calibrated
	// same-pattern judgement. Nil ⇒ the BM25 path, unchanged. The band edges are
	// required when it is set (config validates that), because a default nobody
	// measured would be invented rather than chosen.
	Decider            providers.Decider
	DedupSkipAbove     float64
	DedupAnnotateAbove float64
	// DedupMinScore is the top-hit BM25 score below which the Decider is not asked
	// and the BM25 gate decides instead: the decider is a paid off-host call, and a
	// catalog returns SOME hit for every query. 0 ⇒ relatedFloor, the floor below
	// which a hit is not shown to the reviewer as related either. A parameter rather
	// than the constant in place, because BM25 scores are corpus-dependent and a spend
	// floor and a display floor will want to move apart; not yet wired to config.
	DedupMinScore float64
	Log           *slog.Logger
}

// Curate applies the three-step gate. It returns the created PR ref, or an empty
// ref when the finding was coalesced (duplicate) or dropped (below the bar).
func (c *Curator) Curate(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	// A recall is a match against an existing entry — not novel; never re-curate it
	// (chat delivery still happens upstream). Avoids drafting near-duplicate PRs.
	if inv.Recalled {
		c.Log.Info("skipping curation of a recalled finding (cache hit, not novel)", "title", inv.Title)
		return providers.Ref{}, nil
	}

	// Verdict gate: an operator can configure benign verdicts (e.g. no_action) to
	// stay out of the KB review queue — chat delivery already informed the human, so
	// no repo artifact (PR or coalesce comment) is created. Sits alongside the quality
	// and dedup gates; nil/empty map ⇒ every verdict is eligible (backward-compatible).
	if c.SkipVerdicts[inv.Verdict] {
		c.Log.Info("skipping curation: verdict configured to skip a KB PR (chat-only)",
			"verdict", inv.Verdict, "title", inv.Title)
		return providers.Ref{}, nil
	}

	// 1. quality gate FIRST — a finding below the bar (e.g. unverified) produces NO
	// repo artifact at all: not a PR, and not a coalesce comment on an existing PR.
	// Gating before dedup keeps the open-PR coalesce path from posting an unreviewed
	// finding onto a verified PR.
	if !meetsBar(inv, c.MinConfidence) {
		c.Log.Info("finding below the quality bar; chat-only, no KB artifact",
			"confidence", inv.Confidence, "root_causes", len(inv.RootCauses))
		return providers.Ref{}, nil
	}

	// 2. dedup — catalog exact fingerprint first (deterministic identity, no
	// threshold to tune), then catalog BM25 (observe the top-hit score on every
	// check), then open PRs
	if ff, ok := c.Catalog.(fingerprintFinder); ok {
		fp := DupFingerprint(inv)
		if e, found := ff.FindFingerprint(fp); found {
			c.Log.Info("finding matches a catalog entry's fingerprint; not filing", "entry", e.Title, "path", e.Path)
			// Recovery evidence: a fresh investigation independently re-derived this
			// entry's conclusion — exactly what a standing 👎 forces. Recalls never
			// reach here (Curate returns on inv.Recalled above). Best-effort: a sink
			// error is logged, never blocks the (artifact-free) dedup outcome.
			if c.Confirmations != nil {
				if err := c.Confirmations.Confirm(e.Path, inv.TriggerKey, fp, time.Now()); err != nil {
					c.Log.Warn("confirmation record failed", "entry", e.Path, "err", err)
				}
			}
			return providers.Ref{}, nil
		}
	}
	nov := Novelty{Catalog: c.Catalog, DupScore: c.DupScore}
	hits, herr := nov.Hits(ctx, inv, relatedK)
	// dedupSuspect is the catalog entry the annotate tier flags, threaded onto the
	// drafted entry's SuspectedDuplicate so the PR description can surface it. Nil
	// unless the decider lands in the middle band below.
	var dedupSuspect *providers.RelatedEntry
	if herr != nil {
		c.Log.Warn("dedup: catalog search failed", "err", herr)
	} else if len(hits) > 0 {
		if c.Metrics != nil {
			c.Metrics.CurationDedupScore.Record(ctx, hits[0].Score)
		}
		// Log the score on EVERY dedup decision, not only when the gate fires.
		//
		// dup_score cannot be tuned by observation unless the below-threshold case is
		// visible, and previously only the firing branch logged. A deployment where the
		// gate never fires therefore produced no score data at all — and the
		// curation_dedup_score histogram cannot fill that gap, because its lowest
		// boundary above zero is 5, which is also the default threshold: every
		// non-firing observation collapses into one bucket.
		//
		// Not hypothetical. On a ~50-entry catalog measured 2026-08-14 the BM25 scores
		// run sub-1 (a live instant-recall hit scored 0.494) against the default
		// dup_score of 5.0, so the gate is effectively inert: RunLore re-filed an entry
		// for a fault the catalog already documented, under the same resource. Nothing
		// in the operator's data could have told them what to set instead.
		//
		// Info rather than Debug: curation is infrequent (5 decisions in 30 days on that
		// deployment), so this is a handful of lines, and it is the only way to answer
		// "what should dup_score be?".
		//
		// When a Decider is wired it REPLACES this BM25 comparison (tiered on a
		// calibrated same-pattern probability, corpus-independent unlike dup_score) —
		// unless the decider itself is unreachable, in which case sameIncidentPattern
		// returns ok=false and curation falls back to the BM25 gate below, so a third
		// party being down never blocks curation.
		//
		// Not below DedupMinScore: the decider is a paid off-host call, and below the
		// floor the BM25 gate decides as it always did (see the field's doc).
		tiered := false
		if c.Decider != nil && hits[0].Score >= cmp.Or(c.DedupMinScore, relatedFloor) {
			if prob, ok := c.sameIncidentPattern(ctx, inv, hits[0]); ok {
				tiered = true
				switch {
				case prob >= c.DedupSkipAbove:
					c.Log.Info("dedup: decision model confirms a catalog duplicate; not filing",
						"entry", hits[0].Entry.Title, "path", hits[0].Entry.Path, "probability", prob)
					// Recovery evidence, but NOT the same guarantee as the exact-fingerprint
					// match above: that one is deterministic identity with no false-positive
					// rate, while this fires on a threshold-calibrated but fallible
					// probability — config only requires dedup_skip_above to sit in (0,1]
					// above dedup_annotate_above, so an operator could set it to (say) 0.61
					// and let "confirmation" fire on barely-better-than-a-coinflip confidence.
					// Confirm is what lets a contested, down-voted entry regain trust, so
					// tuning dedup_skip_above also tunes how easily that trust is restored.
					if c.Confirmations != nil {
						if err := c.Confirmations.Confirm(hits[0].Entry.Path, inv.TriggerKey, DupFingerprint(inv), time.Now()); err != nil {
							c.Log.Warn("confirmation record failed", "entry", hits[0].Entry.Path, "err", err)
						}
					}
					return providers.Ref{}, nil
				case prob >= c.DedupAnnotateAbove:
					c.Log.Info("dedup: decision model flags a possible duplicate; filing with the suspect named",
						"entry", hits[0].Entry.Title, "path", hits[0].Entry.Path, "probability", prob)
					re := toRelatedEntry(hits[0])
					dedupSuspect = &re
				default:
					c.Log.Info("dedup: decision model finds no duplicate; filing",
						"entry", hits[0].Entry.Title, "path", hits[0].Entry.Path, "probability", prob)
				}
			}
		}
		if !tiered {
			if hits[0].Score >= c.DupScore {
				c.Log.Info("dedup: duplicates a catalog entry; not filing",
					"entry", hits[0].Entry.Title, "path", hits[0].Entry.Path,
					"score", hits[0].Score, "dup_score", c.DupScore)
				return providers.Ref{}, nil
			}
			c.Log.Info("dedup: top hit below dup_score; filing",
				"top_entry", hits[0].Entry.Title, "path", hits[0].Entry.Path,
				"score", hits[0].Score, "dup_score", c.DupScore)
		}
	}
	if n, ok, err := c.duplicateOpenPR(ctx, inv); err != nil {
		c.Log.Warn("dedup: list open PRs failed", "err", err)
	} else if ok {
		// n came from ListPRsByLabel, so it is a PR/MR number — CommentOnPR, not
		// the issue-scoped call (on GitLab the two number spaces are disjoint).
		if err := c.Forge.CommentOnPR(ctx, n, coalesceComment(inv)); err != nil {
			c.recordCuration(ctx, "pr", "error")
			return providers.Ref{}, fmt.Errorf("coalesce comment: %w", err)
		}
		c.Log.Info("finding coalesced onto an open PR", "pr", n)
		c.recordCuration(ctx, "pr", "coalesced")
		return providers.Ref{}, nil
	}

	// 3. draft a merge-ready PR (labels: runlore + triggered; the curate agent
	// later advances solved/resolved/ready-to-merge — Phase 2, not here)
	entry := draftKBEntry(inv)
	// Reviewer context: the finding is novel, so its BM25 neighborhood is
	// precisely what the reviewer needs to double-check that call.
	entry.Related = relatedEntries(hits)
	// The annotate tier: the decider suspects a duplicate but isn't confident enough
	// to skip filing. SuspectedDuplicate is review metadata for the PR description
	// only — never Body, which is committed into the catalog file, so a merged
	// entry never carries a stale review note.
	entry.SuspectedDuplicate = dedupSuspect
	// The draft-time report the thread path also runs (kbvalidate.WarnDraft): it
	// never blocks the PR, so it sits before OpenPR rather than gating it. Metrics
	// travels with it (nil-safe) because the counter is what makes a defect
	// alertable, and recordCuration below scores this very PR "opened" whether or
	// not the entry inside it can ever be recalled.
	kbvalidate.WarnDraft(ctx, c.Log, c.Metrics, entry)
	ref, err := c.Forge.OpenPR(ctx, entry)
	if err != nil {
		c.recordCuration(ctx, "pr", "error")
		return providers.Ref{}, fmt.Errorf("open PR: %w", err)
	}
	c.Log.Info("curated as PR", "url", ref.URL, "confidence", inv.Confidence)
	c.recordCuration(ctx, "pr", "opened")
	return ref, nil
}

// fingerprintFinder is the optional exact-identity dedup surface of the catalog
// (implemented by *catalog.Catalog). Kept an optional type-assert — like
// providers.GitOpsInspector — so ScoredSearcher fakes and remote indexes without
// fingerprint support keep working unchanged.
type fingerprintFinder interface {
	FindFingerprint(fp string) (catalog.Entry, bool)
}

// recordCuration increments the curations_total counter (nil-safe). kind is the
// forge artifact (e.g. "pr"); result is opened | coalesced | error.
func (c *Curator) recordCuration(ctx context.Context, kind, result string) {
	if c.Metrics == nil {
		return
	}
	c.Metrics.Curations.Add(ctx, 1, metric.WithAttributes(
		attribute.String("kind", kind), attribute.String("result", result)))
}

// duplicateOpenPR reports an open KB PR whose stored dedup fingerprint matches this
// finding's — deterministic identity (resource + cause), not the LLM's free-text
// title. An empty fingerprint (no resource and no cause) never matches.
func (c *Curator) duplicateOpenPR(ctx context.Context, inv providers.Investigation) (int, bool, error) {
	want := DupFingerprint(inv)
	if want == "" {
		return 0, false, nil
	}
	prs, err := c.Forge.ListPRsByLabel(ctx, "runlore")
	if err != nil {
		return 0, false, err
	}
	for _, pr := range prs {
		if providers.ParseFingerprintMarker(pr.Body) == want {
			return pr.Number, true, nil
		}
	}
	return 0, false, nil
}

// meetsBar is the file-time QUALITY gate (not the merge condition): an
// adversarially-reviewed, confident root cause with cited evidence AND a provenance
// anchor (a causing change or a fixing action). The resolved/accepted MERGE
// condition is enforced later by the curate agent + the human.
func meetsBar(inv providers.Investigation, minConf float64) bool {
	if !inv.Verified {
		return false // only findings that survived the adversarial review reach the shared catalog
	}
	if inv.Confidence < minConf || len(inv.RootCauses) == 0 {
		return false
	}
	top := inv.RootCauses[0]
	if top.Summary == "" || len(top.Evidence) == 0 {
		return false
	}
	// Provenance: actionable knowledge, not a symptom restatement — anchored to a
	// causing change (ChangeRef) or a fixing action (SuggestedAction).
	return top.ChangeRef != "" || top.SuggestedAction != ""
}

// dedupQuestionID names the single question the decider is asked.
const dedupQuestionID = "same_pattern"

// sameIncidentPattern asks whether a finding and a catalog hit describe the same
// incident pattern. Returns the probability and whether it could be obtained at all;
// false means the caller uses the BM25 path, so a decider outage degrades to today's
// behaviour rather than blocking curation.
func (c *Curator) sameIncidentPattern(ctx context.Context, inv providers.Investigation, hit catalog.ScoredEntry) (float64, bool) {
	// Both sides name their resource, spelled the way the drafted entry writes it
	// (normalizeResource: pod hash folded, ARN collapsed), so the "same resource" half
	// of the question below can be answered from the state. It could not be before:
	// the finding's resource reached the decider only as Fingerprint's bare words and
	// the entry's not at all, so a same-symptom finding on a DIFFERENT workload read
	// as a confident duplicate, the skip tier dropped it, and a Confirm was recorded
	// against the wrong entry. The entry's alert-side index goes along when set: it
	// is the resource an incoming alert would carry, which is what the finding has.
	state := "PROPOSED FINDING\nresource: " + normalizeResource(inv.Resource) + "\n" + Fingerprint(inv) +
		"\n\nEXISTING CATALOG ENTRY\nresource: " + hit.Entry.Resource
	if hit.Entry.AlertResource != "" {
		state += "\nalert_resource: " + hit.Entry.AlertResource
	}
	state += "\n" + hit.Entry.Title + "\n" + hit.Entry.Description
	started := time.Now()
	ans, err := c.Decider.Decide(ctx, state, []providers.Question{{
		ID:   dedupQuestionID,
		Kind: providers.KindNoul,
		Instructions: "The proposed finding and the existing catalog entry describe the SAME incident pattern: " +
			"the same resource failing the same way for the same reason. A different fault on the same workload is NOT the same pattern.",
	}})
	a, ok := ans[dedupQuestionID]
	if err == nil && !ok {
		// Counted as an error: the fallback is identical, and an ok here would hide it.
		err = fmt.Errorf("decider returned no %q answer", dedupQuestionID)
	}
	// The same series the reranker's decider calls land on, so an outage here is as
	// visible as one there — it used to be a Warn line and a flat metric.
	c.Metrics.RecordModelRequest(ctx, "decide", started, err)
	if err != nil {
		c.Log.Warn("dedup decider failed; falling back to the BM25 gate", "err", err)
		return 0, false
	}
	if c.Metrics != nil {
		c.Metrics.DecisionConfidence.Record(ctx, a.Noul,
			metric.WithAttributes(attribute.String("consumer", "dedup")))
	}
	return a.Noul, true
}

// toRelatedEntry maps one scored catalog hit to the reviewer-context shape shared
// by the Related list and SuspectedDuplicate.
func toRelatedEntry(h catalog.ScoredEntry) providers.RelatedEntry {
	return providers.RelatedEntry{Path: h.Entry.Path, Title: h.Entry.Title, Resource: h.Entry.Resource, Score: h.Score}
}

// relatedEntries maps the draft-time search hits to the PR's reviewer-context
// list, dropping noise-floor matches. Order (best first) is preserved.
func relatedEntries(hits []catalog.ScoredEntry) []providers.RelatedEntry {
	var out []providers.RelatedEntry
	for _, h := range hits {
		if h.Score < relatedFloor {
			continue
		}
		out = append(out, toRelatedEntry(h))
	}
	return out
}

func coalesceComment(inv providers.Investigation) string {
	cause := "(no root cause identified)"
	if len(inv.RootCauses) > 0 && inv.RootCauses[0].Summary != "" {
		cause = inv.RootCauses[0].Summary
	}
	// The open-PR dedup keys on the trigger (resource+reason / alert fingerprint),
	// not the cause, so name the cause observed this time: if it differs from the one
	// documented in this PR, the symptom may now have a different root cause that the
	// trigger-keyed dedup would otherwise hide.
	return fmt.Sprintf(
		"RunLore saw this incident again (confidence %.0f%%). Observed root cause this time: %s\n\n"+
			"Coalesced onto this PR rather than re-filing (on alert/GitOps incidents the dedup key is the trigger, not the cause). "+
			"If that differs from the root cause documented above, this symptom may now have a *different* cause — worth a human check.",
		inv.Confidence*100, cause)
}
