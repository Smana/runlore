// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// gapTitlePrefix titles every knowledge-gap issue; it is also the existence-check
// key (the pass opens at most one issue per pattern).
const gapTitlePrefix = "knowledge-gap: "

// Recurrence opens ONE knowledge-gap issue when an unresolved incident pattern recurs
// at least Threshold times. It is ledger-driven and idempotent: it recomputes counts
// from the outcome ledger every run and opens an issue only when no knowledge-gap
// issue for that pattern already exists — so re-running never double-opens (no mutable
// store or watermark).
//
// ONE-OFF MIGRATION: patterns are grouped (and titled) by resource IDENTITY rather
// than by the raw persisted string — see recurrencePattern. A gap issue already open
// under a pod-scoped or ARN-spelled title no longer matches the pattern it guarded,
// so at most one duplicate issue can be opened per affected pattern on the first run
// after the upgrade; thereafter the new title guards it. Titles for a plain
// namespace/name are byte-identical and are unaffected.
type Recurrence struct {
	Forge  Forge
	Ledger interface {
		Episodes() ([]outcome.Episode, error)
	}
	Threshold int // default 3 when 0
	// Suppressed, when set, is the set of KB entries a human closed WITHOUT merging
	// (keyed by DupFingerprint). Their episodes are counted SEPARATELY — silently,
	// keyed on the fingerprint — and, past the threshold, escalate via a knowledge-gap
	// issue that LINKS the closed PR ("closed unmerged but recurred N times —
	// reconsider?") instead of the plain gap message. A close-without-merge is a
	// deliberate "no": we respect it and never reopen the PR. Nil disables the branch
	// (behaviour is then identical to the pre-suppression pass).
	Suppressed SuppressionSource
	Log        *slog.Logger
}

// Run opens a knowledge-gap issue for each pattern that crosses the threshold and has
// none open yet. Episodes belonging to a suppressed (closed-unmerged) entry are routed
// to a separate escalation that links the closed PR rather than the generic gap issue.
func (r Recurrence) Run(ctx context.Context) error {
	thr := r.Threshold
	if thr == 0 {
		thr = 3
	}
	eps, err := r.Ledger.Episodes()
	if err != nil {
		return fmt.Errorf("recurrence: load episodes: %w", err)
	}
	// Fetch the suppression set only when an unresolved fingerprinted episode could
	// match it — otherwise the (paginated) forge round-trip is wasted work on a quiet
	// or fingerprint-less ledger.
	var suppressed map[string]SuppressedEntry
	if r.Suppressed != nil && hasFingerprintedUnresolved(eps) {
		if suppressed, err = r.Suppressed.Suppressed(ctx); err != nil {
			return fmt.Errorf("recurrence: load suppressed entries: %w", err)
		}
	}
	// Count UNRESOLVED occurrences. Episodes whose fingerprint is suppressed are counted
	// on the fingerprint (the precise incident identity of the rejected entry); the rest
	// are counted per pattern (affected resource; title fallback). Suppressed episodes
	// never feed the generic pattern count, so a rejected entry is escalated once — via
	// the PR-linking issue — never doubly.
	counts := map[string]int{}
	sup := map[string]*supAgg{} // suppressed DupFingerprint -> unresolved count + display pattern
	for _, e := range eps {
		if e.Resolved {
			continue
		}
		if e.DupFingerprint != "" {
			if _, ok := suppressed[e.DupFingerprint]; ok {
				a := sup[e.DupFingerprint]
				if a == nil {
					a = &supAgg{pattern: recurrencePattern(e)}
					sup[e.DupFingerprint] = a
				}
				a.count++
				continue
			}
		}
		counts[recurrencePattern(e)]++
	}
	// Existing knowledge-gap issues are the idempotency guard. OpenIssue labels issues
	// "runlore"/"triggered" and titles them gapTitlePrefix+pattern, so match by title.
	existing, err := r.Forge.ListIssuesByLabel(ctx, "runlore")
	if err != nil {
		return fmt.Errorf("recurrence: list issues: %w", err)
	}
	open := map[string]bool{}
	for _, iss := range existing {
		if p, ok := strings.CutPrefix(iss.Title, gapTitlePrefix); ok {
			open[p] = true
		}
	}
	// Suppressed entries first: their PR-linking escalation takes precedence over a
	// plain gap issue on a colliding pattern.
	for fp, a := range sup {
		if a.count < thr || open[a.pattern] {
			continue
		}
		se := suppressed[fp]
		summary := fmt.Sprintf("The KB entry for %q was closed unmerged (#%d%s) but the incident has recurred %d times — reconsider whether it is knowledge-base-worthy. RunLore did not reopen the closed PR: a close-without-merge is a deliberate human decision.", a.pattern, se.PRNumber, reasonSuffix(se.Reason), a.count)
		if r.openGap(ctx, a.pattern, summary) {
			open[a.pattern] = true // guard against a same-run generic duplicate on this pattern
			r.Log.Info("escalated recurring closed-unmerged KB entry", "pattern", a.pattern, "pr", se.PRNumber, "count", a.count)
		}
	}
	for pattern, n := range counts {
		if n < thr || open[pattern] {
			continue
		}
		summary := fmt.Sprintf("RunLore could not resolve incidents on %q across %d occurrences — needs seeded knowledge or a RunLore fix.", pattern, n)
		if r.openGap(ctx, pattern, summary) {
			r.Log.Info("opened knowledge-gap issue", "pattern", pattern, "count", n)
		}
	}
	return nil
}

// supAgg accumulates the unresolved count and display pattern for one suppressed
// (closed-unmerged) DupFingerprint.
type supAgg struct {
	count   int
	pattern string
}

// hasFingerprintedUnresolved reports whether any episode is unresolved and carries a
// DupFingerprint — the precondition for the suppression set to be worth fetching.
func hasFingerprintedUnresolved(eps []outcome.Episode) bool {
	for _, e := range eps {
		if !e.Resolved && e.DupFingerprint != "" {
			return true
		}
	}
	return false
}

// gapIssue builds the knowledge-gap issue envelope shared by the generic and the
// closed-unmerged escalation paths (same title namespace, single-hypothesis body).
func gapIssue(pattern, summary string) providers.Investigation {
	return providers.Investigation{
		Title:      gapTitlePrefix + pattern,
		RootCauses: []providers.Hypothesis{{Summary: summary}},
	}
}

// openGap opens one knowledge-gap issue, returning whether it succeeded; a failure is
// logged and swallowed so the pass stays best-effort across the remaining patterns.
func (r Recurrence) openGap(ctx context.Context, pattern, summary string) bool {
	if _, err := r.Forge.OpenIssue(ctx, gapIssue(pattern, summary)); err != nil {
		r.Log.Warn("recurrence: open knowledge-gap issue failed", "pattern", pattern, "err", err)
		return false
	}
	return true
}

// reasonSuffix renders the close reason for the escalation summary, or "" when the
// close carried no distinguishing label.
func reasonSuffix(reason string) string {
	if reason == "" {
		return ""
	}
	return ", closed as " + reason
}

// recurrencePattern groups unresolved incidents by the affected resource, falling
// back to the incident title when no resource was identified.
//
// The resource is grouped by its IDENTITY, not by the string it was persisted as —
// the same rule the rest of the system keys and matches on. Two of the ways one
// resource is spelled two ways used to open two buckets here, so one recurring fault
// looked like two or three first sightings, each short of the threshold, and the
// knowledge gap was never reported:
//
//   - The volatile pod-hash suffix (CORE-681). A pod-scoped alert carries the full
//     pod name (KubePodNotReady has only a `pod` label), so three pods of one
//     Deployment counted as three resources. resourceAgrees and curator.IncidentKey
//     have forgiven this since CORE-681; this pass did not.
//   - The ARN and the short spelling of a cloud resource. inv.Resource is
//     model-written free text, so whichever spelling the model echoed back from the
//     seed prompt is what the ledger holds.
//
// The pattern is also the issue TITLE, and the title is this pass's idempotency key
// against already-open issues, so the grouping key and the rendered key are
// deliberately the same value.
func recurrencePattern(e outcome.Episode) string {
	if e.Resource != "" {
		return resourceIdentityRef(e.Resource)
	}
	return e.Title
}

// resourceIdentityRef rewrites a rendered "namespace/name" resource ref to the
// identity it should be GROUPED by, reusing providers.ParseResourceID — the single
// source of truth the recall gate (investigate.refsAgree) and the curator keys
// (curator.IncidentKey, DupFingerprint) already read. It restates none of it: a
// private fourth copy of resource normalisation is exactly the drift this programme
// has twice found in the BM25 query builders.
//
// Only the NAME half is rewritten, and the ref is cut on the FIRST "/" for the same
// reason refsAgree is: a Kubernetes namespace never contains one, so a slash-style
// ARN ("…:instance/i-0abc") survives whole in the name half. A ref with no "/" is a
// bare namespace and is returned untouched, as refsAgree also treats it.
//
// THE ACCOUNT IS APPENDED WHEN THE NAME CARRIED ONE, with "@" and only when present
// — the same shape DupFingerprint qualifies its ref with, so this is not a third
// rule. It is what stops two AWS accounts hosting an instance of the same name from
// grooming as one recurrence, which matters because collapsing an ARN to its bare
// identifier is precisely what forces the two accounts equal (see
// providers.NormalizeResourceName). A Kubernetes ref never has an account, so its
// pattern grows no suffix and its bytes are unchanged.
//
// KNOWN RESIDUAL. IncidentKey and DupFingerprint read the account off the Workload
// FIELD, which ingestion fills under EITHER spelling, so they qualify without
// re-splitting the spellings. Nothing of the sort is available here: outcome.Episode
// carries the resource as a rendered ref and Workload.Ref() renders namespace/name
// only, so the account exists only where the persisted spelling was an ARN. An
// ARN-spelled episode therefore knows its account and a short-spelled one does not,
// and the two group apart — the same residual the base change pinned, resolved the
// same way. Splitting costs a duplicate gap issue; collapsing would groom one
// account's production incident under another account's.
func resourceIdentityRef(ref string) string {
	namespace, name, scoped := strings.Cut(ref, "/")
	if !scoped {
		return ref // a bare namespace: no name half to resolve
	}
	id := providers.ParseResourceID(name)
	out := namespace + "/" + id.Name
	if id.Account != "" {
		out += "@" + id.Account
	}
	return out
}
