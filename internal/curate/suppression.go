// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"fmt"
	"slices"

	"github.com/Smana/runlore/internal/providers"
)

// SuppressedEntry is a KB entry a human closed WITHOUT merging — a deliberate
// "not knowledge-base-worthy". It is keyed on the entry's DupFingerprint so the
// recurrence pass can keep counting the incident silently and, past the threshold,
// escalate via a knowledge-gap issue that LINKS the closed PR — never reopening it.
type SuppressedEntry struct {
	Fingerprint string // curator DupFingerprint (resource+cause); the join key against ledger episodes
	PRNumber    int    // the closed-unmerged KB PR to reference in the escalation
	Reason      string // the close-reason label, when one distinguishes it (e.g. "wontfix"); "" otherwise
}

// SuppressionSource yields the set of suppressed fingerprints (closed-unmerged KB
// entries a human rejected), keyed by DupFingerprint. Recurrence consults it to
// escalate-via-issue instead of re-litigating a human "no".
type SuppressionSource interface {
	Suppressed(ctx context.Context) (map[string]SuppressedEntry, error)
}

// ClosedPRLister lists closed-but-unmerged KB PRs carrying a label. Merged PRs are
// accepted entries (never suppressed) and are filtered out by the implementation.
type ClosedPRLister interface {
	ListClosedUnmergedPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
}

// suppressReviseLabels mark a closed KB PR as "revise & resubmit" rather than an
// outright rejection: the entry IS considered KB-worthy, it just needs work. Such a
// close is NOT suppressed (it must not be escalated as a "reconsider" — a human
// already said yes-with-changes), so its incident flows through the generic
// recurrence path instead.
var suppressReviseLabels = []string{"needs-work"}

// suppressRejectLabels are explicit "not KB-worthy" close reasons; the first one
// present is captured on the SuppressedEntry so the escalation can cite it. A close
// with NO distinguishing label is treated as a rejection too (the default) — the
// escalation still fires, just without a named reason.
var suppressRejectLabels = []string{"wontfix", "not-kb-worthy"}

// ClosedPRSuppression derives the suppression set on every run from the forge's
// closed-unmerged KB PRs — no mutable store or watermark, mirroring Recurrence's
// idempotent, ledger-driven design. The forge retains closed PRs, and each carries
// the same hidden DupFingerprint marker the drafter stamped, so the set is
// reconstructable each pass. A markerless (legacy/hand-filed) close is skipped:
// there is no stable key to suppress or count it on.
type ClosedPRSuppression struct {
	Forge ClosedPRLister
}

// Suppressed implements SuppressionSource.
func (s ClosedPRSuppression) Suppressed(ctx context.Context) (map[string]SuppressedEntry, error) {
	prs, err := s.Forge.ListClosedUnmergedPRsByLabel(ctx, "runlore")
	if err != nil {
		return nil, fmt.Errorf("suppression: list closed-unmerged KB PRs: %w", err)
	}
	out := map[string]SuppressedEntry{}
	for _, pr := range prs {
		fp := providers.ParseFingerprintMarker(pr.Body)
		if fp == "" {
			continue // markerless: nothing stable to key the suppression on
		}
		reason, suppress := classifyClose(pr.Labels)
		if !suppress {
			continue // revise-and-resubmit: not a deliberate rejection
		}
		// One fingerprint may have several closed PRs over time; keep the most recent
		// (highest-numbered) close as the reference.
		if cur, ok := out[fp]; ok && cur.PRNumber >= pr.Number {
			continue
		}
		out[fp] = SuppressedEntry{Fingerprint: fp, PRNumber: pr.Number, Reason: reason}
	}
	return out, nil
}

// classifyClose reads a closed KB PR's labels to decide whether the close is a
// deliberate rejection (suppress=true) and, if so, its named reason. A "revise"
// label wins: it is an accept-with-changes, not a rejection.
func classifyClose(labels []string) (reason string, suppress bool) {
	if _, ok := firstLabelIn(labels, suppressReviseLabels); ok {
		return "", false
	}
	if l, ok := firstLabelIn(labels, suppressRejectLabels); ok {
		return l, true
	}
	return "", true // no distinguishing label: a plain close-without-merge is still a "no"
}

// firstLabelIn returns the first label from set that is present in labels, and whether
// any matched. It keeps the label-set membership checks out of classifyClose.
func firstLabelIn(labels, set []string) (string, bool) {
	for _, l := range set {
		if slices.Contains(labels, l) {
			return l, true
		}
	}
	return "", false
}
