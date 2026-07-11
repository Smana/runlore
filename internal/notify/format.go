// SPDX-License-Identifier: Apache-2.0

// Package notify delivers completed investigations to chat (Slack, Matrix).
package notify

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// verdictBadge maps a model verdict to its emoji + human label. Empty/unknown
// verdicts return ("", "") and are rendered nowhere — never invent a verdict.
func verdictBadge(v providers.Verdict) (emoji, label string) {
	switch v {
	case providers.VerdictNoAction:
		return "✅", "No action needed"
	case providers.VerdictActionSuggested:
		return "🛠", "Action suggested"
	case providers.VerdictActionRequired:
		return "🔥", "Action required"
	case providers.VerdictInconclusive:
		return "❓", "Inconclusive"
	}
	return "", ""
}

// Format renders an Investigation as a concise markdown-ish message used by all
// notifiers.
//
// Invariant: every literal this function emits (labels, separators, bullets)
// avoids the three mrkdwn-meta chars & < >. The Slack fallback is
// escapeMrkdwn(Format(inv)) — only untrusted content (evidence, summaries) may
// carry those chars, so escaping leaves the scaffolding intact. Use · • and
// *bold*; TestFormatScaffoldingHasNoMrkdwnMeta guards it.
func Format(inv providers.Investigation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*Investigation* — confidence %.0f%%\n", inv.Confidence*100)
	// The model verdict is the headline actionability call — show it right under
	// confidence. Empty/unknown verdicts render nothing (never invent one).
	if emoji, label := verdictBadge(inv.Verdict); label != "" {
		fmt.Fprintf(&b, "%s Verdict: %s\n", emoji, label)
	}
	// Name the affected resource up front: it is the first thing an on-call needs
	// (which workload is this about?) and it isn't otherwise in the shared text.
	if ref := inv.Resource.Ref(); ref != "" {
		fmt.Fprintf(&b, "Resource: %s\n", strings.TrimSpace(inv.Resource.Kind+" "+ref))
	}
	// Compact trigger-time metadata line, assembled from whatever the source
	// stamped — omitted entirely for sources that carry none of it.
	if meta := metadataLine(inv); meta != "" {
		fmt.Fprintf(&b, "%s\n", meta)
	}
	if !inv.StartedAt.IsZero() {
		fmt.Fprintf(&b, "Started: %s\n", inv.StartedAt.UTC().Format(time.RFC3339))
	}
	// Seen-before block: only when this is a repeat of a known incident (a first
	// sighting — Occurrences ≤ 1, or 0 = ledger disabled — prints nothing). When
	// the completion pipeline found the merged KB entry for this incident
	// (Prior), the previous cause and human-reviewed resolution are quoted
	// inline — the zero-click payoff of the knowledge base; otherwise the
	// counter + link still tell the reader this is not new.
	if inv.Recalled && inv.Prior != nil {
		// Recall short-circuit: make the knowledge-base cache hit explicit (no fresh
		// investigation ran) and quote the known answer + its resolve-rate track record.
		p := inv.Prior
		b.WriteString("⚡ Instant recall — answered from your knowledge base, no investigation was run\n")
		if p.Cause != "" {
			fmt.Fprintf(&b, "   Known cause: %s\n", p.Cause)
		}
		if p.Resolution != "" {
			fmt.Fprintf(&b, "   Validated resolution: %s\n", p.Resolution)
		}
		if p.Recalls > 0 {
			fmt.Fprintf(&b, "   Resolve rate: %d/%d recalls resolved\n", p.Resolved, p.Recalls)
		}
		if ref := inv.PrevCuratedURL; ref != "" {
			fmt.Fprintf(&b, "   Knowledge-base entry: %s\n", ref)
		} else if p.EntryPath != "" {
			fmt.Fprintf(&b, "   Knowledge-base entry: %s\n", p.EntryPath)
		}
	} else if inv.Occurrences > 1 {
		fmt.Fprintf(&b, "📚 Seen before: ×%d — last investigated %s\n", inv.Occurrences, inv.LastOccurrence.UTC().Format(time.RFC3339))
		if p := inv.Prior; p != nil {
			if p.Cause != "" {
				fmt.Fprintf(&b, "   Prior cause: %s\n", p.Cause)
			}
			if p.Resolution != "" {
				fmt.Fprintf(&b, "   Prior resolution: %s\n", p.Resolution)
			}
			if p.Recalls > 0 {
				fmt.Fprintf(&b, "   Resolve rate: %d/%d recalls resolved\n", p.Resolved, p.Recalls)
			}
		}
		if inv.PrevCuratedURL != "" {
			fmt.Fprintf(&b, "Previous conclusion: %s\n", inv.PrevCuratedURL)
		}
	}
	// Existing-KB match: a full investigation whose kb_search matched a known
	// runbook/entry at clear-match strength — visible proof RunLore already had
	// knowledge for this incident. Suppressed when Prior (recurrence) is set: the
	// Seen-before block above already covers it. The scaffolding here carries no
	// mrkdwn-meta (& < >); the untrusted title/ref stay unescaped like every other
	// field Format emits (the notifier escapes the whole output).
	if mk := inv.MatchedKnowledge; mk != nil && inv.Prior == nil {
		ref := mk.URL
		if ref == "" {
			ref = mk.Path
		}
		// Em-dash (not "(ref)") so a bare URL is readable without surrounding
		// punctuation that would attach to the URL in a copy-paste.
		if ref != "" {
			fmt.Fprintf(&b, "📚 Matches known runbook: %s — %s\n", mk.Title, ref)
		} else {
			fmt.Fprintf(&b, "📚 Matches known runbook: %s\n", mk.Title)
		}
	}
	for i, rc := range inv.RootCauses {
		fmt.Fprintf(&b, "%d. *%s* (%.0f%%)\n", i+1, rc.Summary, rc.Confidence*100)
		// The change the root cause pins the incident on — previously rendered only
		// in the Slack blocks, so Matrix/webhook/CLI readers never saw it.
		if rc.ChangeRef != "" {
			fmt.Fprintf(&b, "   What changed: %s\n", rc.ChangeRef)
		}
		for _, e := range rc.Evidence {
			fmt.Fprintf(&b, "   • %s\n", e)
		}
		if rc.SuggestedAction != "" {
			rev := ""
			if rc.Reversible {
				rev = " (reversible)"
			}
			fmt.Fprintf(&b, "   → suggested: %s%s\n", rc.SuggestedAction, rev)
		}
	}
	if len(inv.Unresolved) > 0 {
		b.WriteString("*Unresolved:*\n")
		for _, u := range inv.Unresolved {
			fmt.Fprintf(&b, "   • %s\n", u)
		}
	}
	// Honest limits: hypotheses actively disproved, and signals we could not get.
	// Both mirror the Unresolved section's shape and are omitted when empty.
	if len(inv.RuledOut) > 0 {
		b.WriteString("*Ruled out:*\n")
		for _, r := range inv.RuledOut {
			fmt.Fprintf(&b, "   • %s\n", r)
		}
	}
	if len(inv.DataGaps) > 0 {
		b.WriteString("*Data gaps:*\n")
		for _, d := range inv.DataGaps {
			fmt.Fprintf(&b, "   • %s\n", d)
		}
	}
	if len(inv.Actions) > 0 {
		b.WriteString("*Suggested actions* (not executed — apply manually):\n")
		for _, a := range inv.Actions {
			rev := ""
			if a.Reversible {
				rev = " (reversible)"
			}
			fmt.Fprintf(&b, "   • %s%s\n", a.Description, rev)
		}
	}
	if inv.CuratedURL != "" {
		fmt.Fprintf(&b, "📚 Knowledge base: %s\n", inv.CuratedURL)
	}
	// Cost footer: a one-line usage summary for humans, appended ONLY to the shared
	// delivery message — never to the curated KB body (the curator builds its own
	// body and does not call Format), so cost never pollutes the knowledge base.
	if foot := usageFooter(inv.Usage); foot != "" {
		fmt.Fprintf(&b, "%s\n", foot)
	}
	return b.String()
}

// metadataLine assembles the trigger-time facts stamped on the investigation
// into one compact " · "-joined line (e.g. "Alert: HarborDown · severity
// critical · env prod · cluster eu-west-1 · tenant platform"). Only non-empty
// parts appear, so a source that carries none of them yields "" and the caller
// omits the line. All separators/labels are mrkdwn-safe (no & < >).
func metadataLine(inv providers.Investigation) string {
	parts := make([]string, 0, 5)
	if inv.AlertName != "" {
		parts = append(parts, "Alert: "+inv.AlertName)
	}
	if inv.Severity != "" {
		parts = append(parts, "severity "+inv.Severity)
	}
	if inv.Environment != "" {
		parts = append(parts, "env "+inv.Environment)
	}
	if inv.Cluster != "" {
		parts = append(parts, "cluster "+inv.Cluster)
	}
	if inv.Tenant != "" {
		parts = append(parts, "tenant "+inv.Tenant)
	}
	return strings.Join(parts, " · ")
}

// usageFooter renders the per-investigation model usage as one line:
//
//	N model calls · X in / Y out tokens (Z% cached)
//
// and appends " · ~$C.CC" only when pricing was configured (Usage.Priced).
// Returns "" when no model call was made (e.g. a pure recall short-circuit), so
// the footer is simply omitted.
func usageFooter(u providers.UsageTotals) string {
	if u.ModelCalls == 0 {
		return ""
	}
	cachedPct := 0
	if u.InputTokens > 0 {
		cachedPct = int(float64(u.CachedInputTokens)/float64(u.InputTokens)*100 + 0.5)
	}
	s := fmt.Sprintf("%d model calls · %d in / %d out tokens (%d%% cached)",
		u.ModelCalls, u.InputTokens, u.OutputTokens, cachedPct)
	if u.Priced {
		s += fmt.Sprintf(" · ~$%.2f", u.CostUSD)
	}
	return s
}

// FormatProgress renders an interim progress update as a concise plain-text
// status line, shared by notifiers (Slack fallback; Matrix/webhook later). The
// fields are untrusted (title + model interim text), so a mrkdwn-parsing notifier
// escapes the composed output before sending.
func FormatProgress(up providers.ProgressUpdate) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Investigating: %s — step %d/%d\n", up.Title, up.Step, up.MaxSteps)
	if s := progressToolsSummary(up.ToolsUsed); s != "" {
		fmt.Fprintf(&b, "Tools used: %s\n", s)
	}
	if t := strings.TrimSpace(up.Interim); t != "" {
		fmt.Fprintf(&b, "%s\n", t)
	}
	return b.String()
}

// progressToolsSummary renders the tools-used map as a stable, name-sorted
// "name×count" list (e.g. "kb_search×1, what_changed×2"). Returns "" for an empty
// map so callers can omit the line.
func progressToolsSummary(used map[string]int) string {
	if len(used) == 0 {
		return ""
	}
	names := make([]string, 0, len(used))
	for n := range used {
		names = append(names, n)
	}
	sort.Strings(names)
	parts := make([]string, 0, len(names))
	for _, n := range names {
		parts = append(parts, fmt.Sprintf("%s×%d", n, used[n]))
	}
	return strings.Join(parts, ", ")
}
