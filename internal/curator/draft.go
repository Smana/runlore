// SPDX-License-Identifier: Apache-2.0

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
	actions := 0
	for _, rc := range inv.RootCauses {
		if rc.SuggestedAction != "" {
			fmt.Fprintf(&b, "- %s (reversible=%t)\n", rc.SuggestedAction, rc.Reversible)
			actions++
		}
	}
	if actions == 0 {
		// A no-action verdict leaves every SuggestedAction empty, but kbvalidate
		// rejects an Incident whose `## Resolution` section is present and empty —
		// the draft would fail RunLore's own merge gate. Say the honest thing
		// (OKF: an explicit unknown beats a fabricated action) instead.
		if len(inv.Unresolved) > 0 {
			b.WriteString("- No action suggested by the investigation — see `## Unresolved`.\n")
		} else {
			b.WriteString("- No action suggested by the investigation.\n")
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
		Type: typ,
		// Cap the free-form investigation title to kbvalidate's merge gate: a single
		// line of ≤120 bytes. inv.Title is LLM/alert-derived and can run long or carry
		// newlines, which would fail RunLore's own `lore validate-kb` hard checks.
		Title:       CapTitle(inv.Title),
		Description: firstLine(inv),
		Resource:    normalizeResource(inv.Resource),
		// Only when the alert fired on a DIFFERENT resource than the one the
		// investigation settled on. Equal values would be pure duplication; a
		// difference is the whole point — it is the alert-side index that makes this
		// entry reachable from the alert that produced it.
		AlertResource: alertResourceIfDistinct(inv),
		Tags:          entryTags(inv, typ),
		Body:          b.String(),
		Fingerprint:   DupFingerprint(inv),
		Confidence:    inv.Confidence,
		Provenance:    refs,
		// Recurrence facts for the PR body's related-knowledge section — stamped
		// on the Investigation BEFORE curation runs (see onInvestigationComplete).
		Occurrences:    inv.Occurrences,
		PrevCuratedURL: inv.PrevCuratedURL,
	}
}

// alertResourceIfDistinct returns the normalized resource the ORIGINATING ALERT fired
// on, or "" when it is the same as the affected resource (nothing to add) or unset.
//
// The investigation routinely refines the alert's workload to a deeper object:
// preferDiscoveredResource lets a discovered resource win over the alert's. That is
// right for the entry's human-facing "affected resource" — but recall matches an
// INCOMING ALERT against the entry's resource, so an entry indexed only by the fault
// locus is unreachable from the very alert that would surface it. Live: an alert on the
// HelmRelease tooling/harbor, investigated down to the pod tooling/harbor-registry, was
// filed under the pod — and every later firing of that same alert missed it
// (no_resource_match) and paid for a full investigation instead.
func alertResourceIfDistinct(inv providers.Investigation) string {
	alert := normalizeResource(inv.AlertResource)
	if alert == "" || alert == normalizeResource(inv.Resource) {
		return ""
	}
	return alert
}

// normalizeResource renders the affected workload as its canonical namespace/name
// ref with the volatile pod-hash suffix stripped from the NAME segment only (via
// the curator-local normalizeWorkloadName → providers.NormalizeWorkloadName, the
// single source of truth for CORE-681).
//
// WHY: a pod-scoped alert (KubePodNotReady carries only a `pod` label) resolves to
// the FULL pod name INCLUDING the ReplicaSet/pod-hash suffix, e.g.
// "tooling/harbor-registry-59598dbd57-ltkzw". Written verbatim to the entry's
// resource: frontmatter that (1) pollutes the human-facing entry with a hash that
// changes every rollout; (2) breaks recall's structural matching against future
// occurrences (the READ side normalizes before comparing — this is the WRITE side);
// (3) forks a second KB entry from an already-normalized one ("tooling/harbor-
// registry"), driving duplicate entries. Normalizing here aligns the WRITTEN
// resource with what DupFingerprint already normalizes internally, so the dedup
// identity is unchanged — only the stored, human/recall-facing ref is corrected.
//
// It reuses Workload.Ref(), so the namespace is never touched and the empty /
// bare-namespace cases pass through unchanged; w is a value copy, so mutating its
// Name is local. Idempotent (NormalizeWorkloadName is).
//
// The Ref() is then narrowed by draftResource → providers.EntryResourceRef, because
// Workload.Name on a curated finding is MODEL-WRITTEN free text and a
// whitespace-bearing one ("essentials, monitoring, argocd-app-of-apps") fails
// RunLore's own merge gate — the curator would draft an entry its own `validate`
// job rejects. That was reported against thread capture (#491), which shares this
// exact source; only the value the model happened to produce differed, so fixing
// one path and not the other would leave the same defect armed here.
func normalizeResource(w providers.Workload) string {
	w.Name = normalizeWorkloadName(w.Name)
	ref, _ := draftResource(w.Ref())
	return ref
}

// draftResource is the draft path's decision about the `resource:` frontmatter
// field: it returns the value to WRITE, plus the reason that value still cannot
// serve as recall's structural index ("" when it can).
//
// The write side is providers.EntryResourceRef — see it for why a value that
// merely clears the merge gate is not good enough, and for what it repairs.
//
// The reason exists because repair has a hard limit. `resource` is matched by
// string equality against a live workload's "namespace/name" ref, so anything
// else is at best a weaker index and at worst unmatchable — but the draft path
// cannot invent the missing half, and MUST NOT drop the finding over it. So it
// reports, and the caller logs; #518's requirement in one line: an unrecallable
// entry is still better than a lost investigation, as long as it is not silent.
//
// A bare token is deliberately only a warning, not a repair. It is genuinely
// ambiguous: Workload.Ref() renders a bare NAMESPACE when the name is unknown
// (routine on alert-triggered investigations, and recall's matchNamespace tier
// serves it), while a model that wrote a workload name with no namespace produces
// the same shape and will match nothing. Guessing which would either mangle a
// working index or fabricate a namespace.
//
// An empty resource is NOT a defect: it is the honest scopeless entry, and recall
// has a matchScopeless tier for exactly it. Since a NON-EMPTY resource disables
// that tier, a wrong value is strictly worse than none.
//
// Idempotent — draftResource(v) for a v it already produced returns v and the same
// reason, which is what lets the curator re-derive the warning from the finished
// entry instead of re-plumbing the raw ref.
func draftResource(ref string) (resource, reason string) {
	resource = providers.EntryResourceRef(ref)
	if resource == "" {
		return "", "" // legitimately scopeless
	}
	ns, name, ok := strings.Cut(resource, "/")
	switch {
	case !ok:
		return resource, "reads as a bare namespace, so it matches every workload in it rather than one object"
	case !isDNSLabel(ns) || !isDNSSubdomain(name):
		return resource, "is not shaped namespace/name (RFC 1123), so recall's exact match can never agree with it"
	}
	return resource, ""
}

// isDNSLabel and isDNSSubdomain report whether a ref's halves could name a real
// Kubernetes object: a namespace is an RFC 1123 LABEL (lowercase alphanumerics and
// "-"), a name an RFC 1123 SUBDOMAIN (the same, plus "."). Both must start and end
// alphanumeric, and neither may be empty.
//
// The warning is keyed on this ALLOWLIST while EntryResourceRef's repair stays a
// denylist of five observed separators, and the asymmetry is deliberate. Repair is
// destructive, so it only cuts at characters proven impossible; diagnosis is free,
// so it reports everything the charset rules out. Keyed the other way — reporting
// only the five — each new separator a model invented would ship silently until
// someone appended another byte to a string literal, which is the very failure #518
// is about: "argocd/essentials|monitoring" and "tooling/Harbor Registry" both clear
// the merge gate, can never equal a Workload.Ref(), and said nothing.
func isDNSLabel(s string) bool { return isDNS(s, false) }

func isDNSSubdomain(s string) bool { return isDNS(s, true) }

func isDNS(s string, dots bool) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		switch c := s[i]; {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-', c == '.' && dots:
			// Interior punctuation only: the first/last-byte check below rejects a
			// leading or trailing one, which no Kubernetes object may carry either.
		default:
			return false
		}
	}
	return isAlnum(s[0]) && isAlnum(s[len(s)-1])
}

func isAlnum(c byte) bool {
	return c >= 'a' && c <= 'z' || c >= '0' && c <= '9'
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

// titleMaxBytes is kbvalidate's hard title limit: `len(e.Title) > 120` is a merge
// gate error, measured in BYTES (Go `len` on a string), not runes.
const titleMaxBytes = 120

// ellipsis marks a truncated title; "…" (U+2026) is 3 bytes in UTF-8.
const ellipsis = "…"

// CapTitle makes an arbitrary investigation title satisfy kbvalidate's title
// merge gate by construction: a single line of at most titleMaxBytes bytes. It
// collapses every whitespace run (newlines/tabs included) into a single space
// and trims, then — if the result still exceeds the byte budget — truncates on a
// rune boundary (preferring the last word boundary) and appends an ellipsis so
// the FINAL byte length stays ≤ titleMaxBytes. Empty/whitespace-only input yields
// "" so we never invent a title (the validator flags the empty title separately).
func CapTitle(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= titleMaxBytes {
		return s
	}

	// Reserve room for the ellipsis; keep the prefix ≤ budget bytes.
	budget := titleMaxBytes - len(ellipsis)

	// Truncate on a rune boundary so we never split a multibyte rune.
	cut := 0
	for i := range s {
		if i > budget {
			break
		}
		cut = i
	}
	prefix := s[:cut]

	// Prefer trimming back to the last space so we don't end mid-word; fall back
	// to the hard rune-boundary cut when there's no reasonable space.
	if sp := strings.LastIndexByte(prefix, ' '); sp > 0 {
		prefix = prefix[:sp]
	}
	return strings.TrimRight(prefix, " ") + ellipsis
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
