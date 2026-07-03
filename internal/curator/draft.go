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

	refs := changeRefs(inv)

	// --- decision card ---
	fmt.Fprintf(&b, "## Decision\n\n")
	fmt.Fprintf(&b, "- **why keep:** %s\n", firstLine(inv))
	fmt.Fprintf(&b, "- **confidence:** %.0f%%\n", inv.Confidence*100)
	if len(refs) > 0 {
		fmt.Fprintf(&b, "- **provenance:** %s\n", strings.Join(refs, ", "))
	}

	// --- Symptom ---
	fmt.Fprintf(&b, "\n## Symptom\n\n%s\n", inv.Title)
	if ref := inv.Resource.Ref(); ref != "" {
		// Name the affected workload: what a future reader checks first, and lexical
		// recall signal in the indexed body (the kind appears nowhere else).
		if inv.Resource.Kind != "" {
			fmt.Fprintf(&b, "\nAffected resource: %s %s\n", inv.Resource.Kind, ref)
		} else {
			fmt.Fprintf(&b, "\nAffected resource: %s\n", ref)
		}
	}

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

	// --- Citations (OKF §8: numbered references at the document bottom) ---
	if len(refs) > 0 {
		b.WriteString("\n## Citations\n\n")
		for i, r := range refs {
			fmt.Fprintf(&b, "[%d] %s\n", i+1, r)
		}
	}

	typ := entryType(inv)
	return providers.KBEntry{
		Type:        typ,
		Title:       inv.Title,
		Description: firstLine(inv),
		Resource:    inv.Resource.Ref(),
		Tags:        entryTags(inv, typ),
		Body:        b.String(),
		Fingerprint: DupFingerprint(inv),
		Confidence:  inv.Confidence,
		Provenance:  refs,
	}
}

// entryType derives the OKF type for a drafted entry. The default is Incident: a
// point-in-time card carrying the OKF evidence sections (Symptom/Investigate/
// Cause/Resolution) that draftKBEntry always renders and that kbvalidate requires
// for Incident.
//
// A finding is a Playbook — generalized, reusable runbook knowledge — only when it
// is BOTH change-agnostic and resource-agnostic: no concrete affected resource ref
// AND no causing-change provenance on the top cause, yet a reusable suggested
// action. A specific ChangeRef ("crossplane/xplane-harbor") or a concrete resource
// pins the finding to one incident, so either keeps it an Incident — preventing
// the heuristic from over-firing on incidents that merely failed to capture a
// resource ref.
//
// We never emit Postmortem: it is not in the validator vocabulary {Incident,
// Playbook, Concept}, so it would fail `lore validate-kb`. (The validator relaxes
// the section requirements for Playbook, so the extra structure draftKBEntry
// renders is harmless.)
func entryType(inv providers.Investigation) string {
	if len(inv.RootCauses) == 0 {
		return "Incident"
	}
	top := inv.RootCauses[0]
	if inv.Resource.Ref() == "" && top.ChangeRef == "" && top.SuggestedAction != "" {
		return "Playbook"
	}
	return "Incident"
}

// entryTags derives the entry's tags: the constant runlore + type pair plus the
// workload kind and namespace. Tags feed the catalog's BM25+embedding corpus
// (catalog.entryText), so each derived tag is recall signal the constant pair
// can't provide. Empties are dropped and duplicates collapsed.
func entryTags(inv providers.Investigation, typ string) []string {
	tags := []string{"runlore", strings.ToLower(typ)}
	seen := map[string]bool{tags[0]: true, tags[1]: true}
	for _, t := range []string{strings.ToLower(inv.Resource.Kind), inv.Resource.Namespace} {
		if t != "" && !seen[t] {
			seen[t] = true
			tags = append(tags, t)
		}
	}
	return tags
}

func firstLine(inv providers.Investigation) string {
	if len(inv.RootCauses) > 0 {
		return inv.RootCauses[0].Summary
	}
	return inv.Title
}

// changeRefs collects the distinct change references cited across root causes
// (the causing/fixing-change provenance the merge bar requires). They feed the
// decision card, the OKF Citations section, and the provenance frontmatter.
func changeRefs(inv providers.Investigation) []string {
	var refs []string
	seen := map[string]bool{}
	for _, rc := range inv.RootCauses {
		if rc.ChangeRef != "" && !seen[rc.ChangeRef] {
			seen[rc.ChangeRef] = true
			refs = append(refs, rc.ChangeRef)
		}
	}
	return refs
}
