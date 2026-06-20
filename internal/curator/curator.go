// Package curator writes completed investigations back to the git forge: confident
// findings become a PR drafting an OKF knowledge entry; less-confident findings
// become an issue. It closes RunLore's Learn loop. Writes target the forge only.
package curator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

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

// draftKBEntry renders an investigation as an OKF knowledge entry.
func draftKBEntry(inv providers.Investigation) providers.KBEntry {
	var b strings.Builder
	fmt.Fprintf(&b, "## Summary\n\nConfidence %.0f%%.\n\n## Root causes\n", inv.Confidence*100)
	tags := []string{"runlore", "incident"}
	for i, rc := range inv.RootCauses {
		fmt.Fprintf(&b, "%d. **%s** (%.0f%%)\n", i+1, rc.Summary, rc.Confidence*100)
		for _, e := range rc.Evidence {
			fmt.Fprintf(&b, "   - evidence: %s\n", e)
		}
		if rc.SuggestedAction != "" {
			fmt.Fprintf(&b, "   - suggested: %s (reversible=%t)\n", rc.SuggestedAction, rc.Reversible)
		}
	}
	if len(inv.Unresolved) > 0 {
		b.WriteString("\n## Unresolved\n")
		for _, u := range inv.Unresolved {
			fmt.Fprintf(&b, "- %s\n", u)
		}
	}
	return providers.KBEntry{
		Type:        "Incident",
		Title:       inv.Title,
		Description: firstLine(inv),
		Tags:        tags,
		Body:        b.String(),
	}
}

func firstLine(inv providers.Investigation) string {
	if len(inv.RootCauses) > 0 {
		return inv.RootCauses[0].Summary
	}
	return inv.Title
}
