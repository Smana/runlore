// Package curator writes completed investigations back to the git forge: confident
// findings become a PR drafting an OKF knowledge entry; less-confident findings
// become an issue. It closes RunLore's Learn loop. Writes target the forge only.
package curator

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Smana/runlore/internal/providers"
)

// Curator routes an investigation to a PR or an issue by confidence.
type Curator struct {
	Issues          providers.IssueProvider
	MinConfidencePR float64 // confidence ≥ this drafts a PR; otherwise an issue
	Log             *slog.Logger
}

// Curate writes the investigation to the forge and returns the created ref.
func (c *Curator) Curate(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	if inv.Confidence >= c.MinConfidencePR {
		ref, err := c.Issues.OpenPR(ctx, draftKBEntry(inv))
		if err != nil {
			return providers.Ref{}, fmt.Errorf("open PR: %w", err)
		}
		c.Log.Info("curated as PR", "url", ref.URL, "confidence", inv.Confidence)
		return ref, nil
	}
	ref, err := c.Issues.OpenIssue(ctx, inv)
	if err != nil {
		return providers.Ref{}, fmt.Errorf("open issue: %w", err)
	}
	c.Log.Info("curated as issue", "url", ref.URL, "confidence", inv.Confidence)
	return ref, nil
}
