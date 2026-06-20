// Package notify delivers completed investigations to chat (Slack, Matrix).
package notify

import (
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// Format renders an Investigation as a concise markdown-ish message used by all
// notifiers.
func Format(inv providers.Investigation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*Investigation* — confidence %.0f%%\n", inv.Confidence*100)
	for i, rc := range inv.RootCauses {
		fmt.Fprintf(&b, "%d. *%s* (%.0f%%)\n", i+1, rc.Summary, rc.Confidence*100)
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
	return b.String()
}
