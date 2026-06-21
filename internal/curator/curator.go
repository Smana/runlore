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
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// Curator is the file-time learning gate. It dedups, quality-gates, and drafts a
// merge-ready PR for novel quality findings; everything else produces no repo
// artifact.
type Curator struct {
	Forge         providers.CurationForge
	Catalog       catalog.ScoredSearcher // nil ⇒ no catalog dedup
	DupScore      float64                // catalog BM25 dup threshold
	MinConfidence float64                // quality gate: minimum overall confidence
	Log           *slog.Logger
}

// Curate applies the three-step gate. It returns the created PR ref, or an empty
// ref when the finding was coalesced (duplicate) or dropped (below the bar).
func (c *Curator) Curate(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	// 1. dedup — catalog, then open PRs
	if dup, hit, err := (Novelty{Catalog: c.Catalog, DupScore: c.DupScore}).IsDuplicate(ctx, inv); err != nil {
		c.Log.Warn("dedup: catalog search failed", "err", err)
	} else if dup {
		c.Log.Info("finding duplicates a catalog entry; not filing", "entry", hit.Title)
		return providers.Ref{}, nil
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

	// 2. quality gate — below the bar ⇒ no repo artifact (chat alert only)
	if !meetsBar(inv, c.MinConfidence) {
		c.Log.Info("finding below the quality bar; chat-only, no KB artifact",
			"confidence", inv.Confidence, "root_causes", len(inv.RootCauses))
		return providers.Ref{}, nil
	}

	// 3. draft a merge-ready PR (labelled solved by the forge lifecycle labels)
	ref, err := c.Forge.OpenPR(ctx, draftKBEntry(inv))
	if err != nil {
		return providers.Ref{}, fmt.Errorf("open PR: %w", err)
	}
	c.Log.Info("curated as PR", "url", ref.URL, "confidence", inv.Confidence)
	return ref, nil
}

// duplicateOpenPR reports an open KB PR whose normalized title matches this
// finding (cheap title-slug match; deep cross-incident dedup is the curate agent).
func (c *Curator) duplicateOpenPR(ctx context.Context, inv providers.Investigation) (int, bool, error) {
	prs, err := c.Forge.ListPRsByLabel(ctx, "runlore")
	if err != nil {
		return 0, false, err
	}
	want := normTitle(inv.Title)
	for _, pr := range prs {
		if normTitle(strings.TrimPrefix(pr.Title, "KB: ")) == want {
			return pr.Number, true, nil
		}
	}
	return 0, false, nil
}

// meetsBar is the file-time QUALITY gate (not the merge condition): a confirmed,
// confident root cause with cited evidence. The resolved/accepted MERGE condition
// is enforced later by the curate agent + the human.
func meetsBar(inv providers.Investigation, minConf float64) bool {
	if inv.Confidence < minConf || len(inv.RootCauses) == 0 {
		return false
	}
	top := inv.RootCauses[0]
	return top.Summary != "" && len(top.Evidence) > 0
}

func normTitle(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func coalesceComment(inv providers.Investigation) string {
	return fmt.Sprintf("RunLore saw this incident again (confidence %.0f%%). Coalesced rather than re-filed.", inv.Confidence*100)
}
