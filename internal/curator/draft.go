package curator

import (
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// draftKBEntry renders an investigation as a merge-ready OKF knowledge entry: a
// decision card (why-keep + confidence) followed by the OKF sections
// Symptom / Investigate / Cause / Resolution. The decision card makes the human
// merge trivial; the sections make the entry reusable knowledge (the #48 standard).
func draftKBEntry(inv providers.Investigation) providers.KBEntry {
	var b strings.Builder

	// --- decision card ---
	fmt.Fprintf(&b, "## Decision\n\n")
	fmt.Fprintf(&b, "- **why keep:** %s\n", firstLine(inv))
	fmt.Fprintf(&b, "- **confidence:** %.0f%%\n", inv.Confidence*100)
	if cr := changeRefs(inv); cr != "" {
		fmt.Fprintf(&b, "- **provenance:** %s\n", cr)
	}

	// --- Symptom ---
	fmt.Fprintf(&b, "\n## Symptom\n\n%s\n", inv.Title)

	// --- Investigate (evidence trail) ---
	b.WriteString("\n## Investigate\n\n")
	for _, rc := range inv.RootCauses {
		for _, e := range rc.Evidence {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}

	// --- Cause (ranked root causes) ---
	b.WriteString("\n## Cause\n\n")
	for i, rc := range inv.RootCauses {
		fmt.Fprintf(&b, "%d. **%s** (%.0f%%)", i+1, rc.Summary, rc.Confidence*100)
		if rc.ChangeRef != "" {
			fmt.Fprintf(&b, " — change: %s", rc.ChangeRef)
		}
		b.WriteString("\n")
	}

	// --- Resolution (suggested, reversible-first) ---
	b.WriteString("\n## Resolution\n\n")
	for _, rc := range inv.RootCauses {
		if rc.SuggestedAction != "" {
			fmt.Fprintf(&b, "- %s (reversible=%t)\n", rc.SuggestedAction, rc.Reversible)
		}
	}
	if len(inv.Unresolved) > 0 {
		b.WriteString("\n## Unresolved\n\n")
		for _, u := range inv.Unresolved {
			fmt.Fprintf(&b, "- %s\n", u)
		}
	}

	return providers.KBEntry{
		Type:        "Incident",
		Title:       inv.Title,
		Description: firstLine(inv),
		Tags:        []string{"runlore", "incident"},
		Body:        b.String(),
	}
}

func firstLine(inv providers.Investigation) string {
	if len(inv.RootCauses) > 0 {
		return inv.RootCauses[0].Summary
	}
	return inv.Title
}

// changeRefs collects the distinct change references cited across root causes
// (the causing/fixing-change provenance the merge bar requires).
func changeRefs(inv providers.Investigation) string {
	var refs []string
	seen := map[string]bool{}
	for _, rc := range inv.RootCauses {
		if rc.ChangeRef != "" && !seen[rc.ChangeRef] {
			seen[rc.ChangeRef] = true
			refs = append(refs, rc.ChangeRef)
		}
	}
	return strings.Join(refs, ", ")
}
