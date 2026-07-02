// Package notify delivers completed investigations to chat (Slack, Matrix).
package notify

import (
	"fmt"
	"sort"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// Format renders an Investigation as a concise markdown-ish message used by all
// notifiers.
func Format(inv providers.Investigation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*Investigation* — confidence %.0f%%\n", inv.Confidence*100)
	// Name the affected resource up front: it is the first thing an on-call needs
	// (which workload is this about?) and it isn't otherwise in the shared text.
	if ref := inv.Resource.Ref(); ref != "" {
		fmt.Fprintf(&b, "Resource: %s\n", strings.TrimSpace(inv.Resource.Kind+" "+ref))
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
	return b.String()
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
