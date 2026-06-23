// Package curator is the file-time learning gate: it dedups a finding against the
// catalog and open PRs, gates on quality, and drafts a merge-ready PR for novel,
// quality findings. Uncertain/low-quality findings produce NO repo artifact (the
// chat delivery already informed the human). It never opens issues — the only
// issues are knowledge-gap issues, opened by the curate agent (Phase 2).
package curator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
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

	// 1. quality gate FIRST — a finding below the bar (e.g. unverified) produces NO
	// repo artifact at all: not a PR, and not a coalesce comment on an existing PR.
	// Gating before dedup keeps the open-PR coalesce path from posting an unreviewed
	// finding onto a verified PR.
	if !meetsBar(inv, c.MinConfidence) {
		c.Log.Info("finding below the quality bar; chat-only, no KB artifact",
			"confidence", inv.Confidence, "root_causes", len(inv.RootCauses))
		return providers.Ref{}, nil
	}

	// 2. dedup — catalog (observe the top-hit score on every check), then open PRs
	nov := Novelty{Catalog: c.Catalog, DupScore: c.DupScore}
	if top, ok, err := nov.TopHit(ctx, inv); err != nil {
		c.Log.Warn("dedup: catalog search failed", "err", err)
	} else if ok {
		if c.Metrics != nil {
			c.Metrics.CurationDedupScore.Record(ctx, top.Score)
		}
		if top.Score >= c.DupScore {
			c.Log.Info("finding duplicates a catalog entry; not filing", "entry", top.Entry.Title, "score", top.Score)
			return providers.Ref{}, nil
		}
	}
	if n, ok, err := c.duplicateOpenPR(ctx, inv); err != nil {
		c.Log.Warn("dedup: list open PRs failed", "err", err)
	} else if ok {
		if err := c.Forge.Comment(ctx, n, coalesceComment(inv)); err != nil {
			return providers.Ref{}, fmt.Errorf("coalesce comment: %w", err)
		}
		c.Log.Info("finding coalesced onto an open PR", "pr", n)
		return providers.Ref{}, nil
	}

	// 3. draft a merge-ready PR (labels: runlore + triggered; the curate agent
	// later advances solved/resolved/ready-to-merge — Phase 2, not here)
	ref, err := c.Forge.OpenPR(ctx, draftKBEntry(inv))
	if err != nil {
		return providers.Ref{}, fmt.Errorf("open PR: %w", err)
	}
	c.Log.Info("curated as PR", "url", ref.URL, "confidence", inv.Confidence)
	return ref, nil
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

func coalesceComment(inv providers.Investigation) string {
	return fmt.Sprintf("RunLore saw this incident again (confidence %.0f%%). Coalesced rather than re-filed.", inv.Confidence*100)
}
