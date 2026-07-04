package investigate

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// ReinvestigateLabel is the label a human adds to a curated KB issue to ask RunLore
// to run the investigation again (e.g. after more has happened, or to get a fresh
// look). RunLore polls for it — it receives no inbound GitHub webhooks.
const ReinvestigateLabel = "reinvestigate"

// Reinvestigator polls the forge for issues labelled ReinvestigateLabel, re-runs
// the investigation, posts the fresh findings as a comment, and flips the label to
// "investigating". This is the outbound-poll re-trigger path that fits RunLore's
// no-inbound-webhook design.
type Reinvestigator struct {
	Forge providers.ReinvestForge
	// Run executes a fresh investigation for the request and returns its findings.
	Run func(ctx context.Context, req Request) (providers.Investigation, error)
	Log *slog.Logger
}

// Poll re-investigates flagged issues on each tick until ctx is done.
func (r *Reinvestigator) Poll(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 2 * time.Minute
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		r.pollOnce(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (r *Reinvestigator) pollOnce(ctx context.Context) {
	issues, err := r.Forge.ListIssuesByLabel(ctx, ReinvestigateLabel)
	if err != nil {
		r.Log.Warn("reinvestigate poll: list issues failed", "err", err)
		return
	}
	for _, is := range issues {
		// Only re-run RunLore-originated issues: require the "runlore" provenance
		// label so an arbitrary (drive-by) issue carrying "reinvestigate" can't
		// trigger an investigation. Title/body are untrusted, fed only as context.
		if !slices.Contains(is.Labels, "runlore") {
			continue
		}
		req := Request{Source: SourceAlert, Title: is.Title, Reason: "re-investigation requested via the reinvestigate label", Message: is.Body}
		// Like a GitOps failure, a reinvestigate poll carries no external alert
		// fingerprint. Derive a stable, deterministic one from the issue identity so the
		// incident is not invisible to the outcome ledger (see outcome.DeriveFingerprint).
		// The reinvestigate: prefix marks it as non-resolvable (no resolve webhook can
		// match it).
		fp := outcome.DeriveFingerprint(outcome.ReinvestigateFingerprintPrefix, fmt.Sprintf("issue-%d", is.Number))
		req.Fingerprint = fp
		req.Fingerprints = []string{fp}
		inv, err := r.Run(ctx, req)
		if err != nil {
			r.Log.Warn("reinvestigate: run failed", "issue", is.Number, "err", err)
			continue
		}
		if err := r.Forge.Comment(ctx, is.Number, reinvestComment(inv)); err != nil {
			r.Log.Warn("reinvestigate: comment failed", "issue", is.Number, "err", err)
			continue
		}
		// Move the lifecycle forward and clear the trigger so it isn't re-run every tick.
		_ = r.Forge.ReplaceLabel(ctx, is.Number, ReinvestigateLabel, "investigating")
		r.Log.Info("reinvestigated", "issue", is.Number, "confidence", inv.Confidence, "root_causes", len(inv.RootCauses))
	}
}

// reinvestComment renders the fresh findings for an issue comment.
func reinvestComment(inv providers.Investigation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "🔁 **RunLore re-investigation** — confidence %.0f%%\n\n", inv.Confidence*100)
	if len(inv.RootCauses) == 0 {
		b.WriteString("_No root cause could be verified this run._\n")
	}
	for i, rc := range inv.RootCauses {
		fmt.Fprintf(&b, "%d. **%s** (%.0f%%)\n", i+1, rc.Summary, rc.Confidence*100)
		for _, e := range rc.Evidence {
			fmt.Fprintf(&b, "   - %s\n", e)
		}
		if rc.SuggestedAction != "" {
			fmt.Fprintf(&b, "   - suggested: %s\n", rc.SuggestedAction)
		}
	}
	if len(inv.Unresolved) > 0 {
		b.WriteString("\n**Unresolved:**\n")
		for _, u := range inv.Unresolved {
			fmt.Fprintf(&b, "- %s\n", u)
		}
	}
	return b.String()
}
