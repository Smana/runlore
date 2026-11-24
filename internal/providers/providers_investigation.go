// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"slices"
	"strings"
	"time"
)

// Verdict classifies an investigation's actionability for the humans reading the
// notification — the "do I need to do anything?" answer, separate from confidence
// (how sure the model is) and severity (how the alert was labelled).
type Verdict string

// The model-facing verdict vocabulary; submit_findings requires one of these.
const (
	VerdictNoAction        Verdict = "no_action"        // benign / self-healed / synthetic / expected; nothing to fix (a false positive from a rule defect is action_suggested)
	VerdictActionSuggested Verdict = "action_suggested" // a human should follow the suggested next steps
	VerdictActionRequired  Verdict = "action_required"  // live impact; act promptly
	VerdictInconclusive    Verdict = "inconclusive"     // could not be determined with available data
)

// Conclusive reports whether v is an ANSWER — the investigation reached a
// judgement the on-call can act on (or deliberately not act on). It is the ONE
// definition of "conclusive": the outcome ledger folds its per-trigger standing
// answer with it, and TriggerRecurrence.Concluded re-checks stored verdicts through
// it, so the writer and the reader cannot disagree. inconclusive is not an answer,
// and neither is "" (a pre-verdict ledger event, or a reply the parser could not
// normalize) — both mean "we still owe a real answer".
func (v Verdict) Conclusive() bool {
	switch v {
	case VerdictNoAction, VerdictActionSuggested, VerdictActionRequired:
		return true
	}
	return false
}

// ValidVerdict reports whether v is one of the model-facing enum values; the
// parser normalizes anything else to "" so formatters can safely omit it. Defined
// in terms of Conclusive so the three actionability verdicts are enumerated once —
// a fifth verdict is then one switch to update, not two adjacent ones.
func ValidVerdict(v Verdict) bool {
	return v.Conclusive() || v == VerdictInconclusive
}

// AppliedResolution is a fix RunLore saw merged and soaked: what was applied
// and the verdict the soak returned. Written by RunLore, never by the model.
type AppliedResolution struct {
	FixPR, MergeSHA, Verdict, Why, Diff, Evidence string
	VerifiedAt                                    time.Time
}

// PREntry is the catalog entry a knowledge-base pull request carries and the
// state it is in: "open", "merged", or "closed" (closed without merging).
type PREntry struct{ Path, State string }

// EntryPRState is what the forge says about an open entry PR when deciding whether to rebuild it.
type EntryPRState struct {
	Branch, HeadSHA string
	Mergeable       *bool    // nil while the forge computes it
	Fork, Truncated bool     // Truncated: a full page of commits or files, so the lists below may be cut
	CommitAuthors   []string // one per commit; empty for one with no linked account, or the forge did not write and sign
	Files           []string
}

// PullSummary is what the forge says a merged fix changed: the PR's URL, the
// merge commit, the first few files and the line counts.
type PullSummary struct {
	URL, MergeSHA        string
	Files                []string
	Additions, Deletions int
}

// Investigation is the structured output contract of an investigation.
type Investigation struct {
	Title      string
	RootCauses []Hypothesis
	Changes    []Change
	Unresolved []string // honest: what the agent could not determine
	Verdict    Verdict  // model-classified actionability; "" when the model omitted it (rendered nowhere)
	RuledOut   []string // hypotheses considered and rejected, one line each with the disproving evidence
	DataGaps   []string // signals that could not be obtained (tool errors, missing metrics, truncation) — a data limitation, not a question for a human
	Confidence float64
	Grounding  *EvidenceGrounding `json:"grounding,omitempty"` // server-computed rollup across surviving RootCauses; nil when unmeasured
	// Resolutions are the fixes RunLore verified after merge. Set by RunLore at
	// runtime, never by the model.
	Resolutions []AppliedResolution
	// PinnedFingerprint is the identity a hold froze for this incident. Set by RunLore at runtime,
	// never by the model: when non-empty, curator.DupFingerprint returns it, so the draft's marker,
	// the coalesce and the catalog check all read the hold's identity.
	PinnedFingerprint string
	Recalled          bool     // true when produced by instant recall (a KB cache hit); the curator skips re-curating it
	Resource          Workload // the workload the investigation identified as affected; defaults to the originating alert workload when none was named (stored on curated entries for structural recall)
	// AlertResource is the workload the ORIGINATING ALERT fired on, stamped verbatim
	// from the Request and never touched by the model. It is NOT Resource: Resource is
	// where the fault was FOUND, which the investigation routinely refines to a deeper
	// object (an alert on the HelmRelease tooling/harbor resolving to the pod
	// tooling/harbor-registry). Recall, however, matches by the resource an ALERT
	// carries — so an entry indexed only by the fault locus is unreachable from the
	// alert that would surface it. Persisting both is what closes that gap.
	AlertResource Workload
	// Trigger-time facts stamped verbatim from the Request for the notification's
	// metadata block. The model never sees or sets them; empty for sources that lack them.
	Severity    string    // alert severity label at trigger time
	Environment string    // deployment environment (prod/staging/…)
	Cluster     string    // alert "cluster" label
	Tenant      string    // alert "tenant" label
	AlertName   string    // triggering alert name (labels["alertname"]); "" for non-alert sources
	StartedAt   time.Time // incident start (alert startsAt / failure time)
	// InvestigationStartedAt is when RUNLORE began investigating — distinct from StartedAt,
	// which is when the INCIDENT began. The two can be far apart: a request waits out
	// debounce/coalescing, then queues behind the single sequential worker and any
	// rate-limit backoff before the loop starts. Stamped by the loop at its delivery
	// chokepoint (never by the model) and carried onto the outcome-ledger open, where it is
	// the exact bound on resolve-before-open pairing (see outcome.resolvesSince) — the open
	// itself is stamped at COMPLETION, so without this the pairing window is unknowable.
	InvestigationStartedAt time.Time
	Actions                []Action // proposed remediations (autonomy ladder; never executed at rung "suggest")
	CuratedURL             string   // runtime: KB issue/PR the curator opened, linked in delivery (set after curation)
	// CurateError is the reason the curator could not write this finding to the KB, set
	// only when a write was ATTEMPTED and failed. It exists because an empty CuratedURL is
	// ambiguous: it is also the normal state for a finding below curate.min_confidence or
	// carrying a skip_verdicts verdict, so "no KB link" cannot tell a human whether the
	// learning loop is working. Rendered by notify.Format AND by the Slack card's footer
	// (notify.summaryBlocks), both through notify.curateFailureReason; never written to
	// the KB body.
	//
	// One of exactly two free-text fields on this struct stamped AFTER
	// investigate.redactInvestigation — the other is Prior (Cause/Resolution) — because
	// both are produced by the OnComplete pipeline rather than inside the loop. Each is
	// therefore redacted at its assignment (app.onInvestigationComplete) rather than by
	// the reflection walk, and neither is on redactionSkipField: both are untrusted free
	// text, the opposite of what that list is for. Everything else the pipeline stamps
	// past the chokepoint is a number, a time, or a server-derived identifier already on
	// that list (CuratedURL, PrevCuratedURL, Action.ApprovalID).
	CurateError   string
	Fingerprint   string      // originating alert fingerprint; for outcome-ledger attribution
	Fingerprints  []string    // coalesced batch fingerprints; one outcome open is recorded per entry
	TriggerKey    string      // deterministic incident identity set at trigger time (alerts: host-invariant per-class key from curator.IncidentKey; GitOps: failing resource+condition). curator.DupFingerprint prefers it so reworded re-investigations (#137) AND the same alert on a different pod/node (CORE-681) still dedupe
	RecalledEntry string      // when Recalled: the catalog entry Path that was matched
	Verified      bool        // true when the adversarial verify pass ran and a root cause survived it
	Usage         UsageTotals // per-investigation model token/cost accounting (loop + verify); surfaced to humans + metrics, never written to the curated KB body
	// Recurrence facts stamped at completion from the outcome ledger's per-TriggerKey
	// index (never seen by the model). They describe PRIOR investigations of the same
	// TriggerKey; this run's own open is recorded after they are read.
	Occurrences    int             // Nth recorded investigation of this TriggerKey (1 = first); 0 = unknown/ledger disabled
	LastOccurrence time.Time       // when the previous occurrence was investigated
	PrevCuratedURL string          // the previous occurrence's KB link, for the "same conclusion as before" pointer
	Prior          *PriorKnowledge // what the merged KB entry already says about this recurring incident; nil when unknown (see PriorKnowledge)
	// MatchedKnowledge is the single strongest PRE-EXISTING knowledge-base entry that
	// this investigation's kb_search calls matched at clear-match strength — the visible
	// proof that RunLore already had documented knowledge for the incident. It is stamped
	// by the ReAct loop (never by the model) and is DISTINCT from Prior: Prior reports
	// RECURRENCE ("this exact incident, investigated N times before", from the outcome
	// ledger), whereas MatchedKnowledge reports that a FULL investigation reused a known
	// runbook/entry even on a first sighting. nil when no kb_search hit cleared the bar.
	// Notifiers render it only when Prior == nil (the recurrence block already covers the
	// "seen before" case — don't double-render).
	MatchedKnowledge *MatchedEntry
}

// UnaccountedInconclusive reports whether inv claims it could not determine the
// cause while giving no account of what blocked it: no root cause, no question for
// a human, no data gap. That payload contradicts itself — an honest "I could not
// determine" always has something in one of those three channels — and it is what
// the model produces when it reaches for `inconclusive` to mean "this is already
// known" (#471, and again live on 2026-08-18).
//
// It lives HERE, on the contract, because two sides consult it and must not
// disagree: the loop says it out loud at the source, where the payload is still
// attributable to the model call that made it, and the notifier acts on it at
// delivery, where a card that neither explains itself nor shows evidence is what
// the on-call actually has to read. Same reasoning as Verdict.Conclusive — one
// definition, so the writer and the reader cannot drift apart.
//
// One definition, but deliberately NOT one snapshot: the loop evaluates this before
// verifyFindings runs (it is judging the model's own payload) and the notifier
// evaluates it on the post-verify result, so the two legitimately disagree on a run
// whose reviewer rejected every hypothesis. Sharing the definition keeps the QUESTION
// identical; it was never meant to make the answers identical. The notifier does not
// render straight off this predicate for exactly that reason — see
// notify.unaccountedForReader, which additionally requires RuledOut to be empty.
//
// RuledOut is deliberately not one of the three: eliminating hypotheses says what
// the cause is NOT, which is neither a finding nor a reason the run could not reach
// one. The three channels here are the ones that answer "so what happened?" or
// "why don't you know?".
func (inv Investigation) UnaccountedInconclusive() bool {
	return inv.Verdict == VerdictInconclusive &&
		len(inv.RootCauses) == 0 && len(inv.Unresolved) == 0 && len(inv.DataGaps) == 0
}

// ActionWithoutRemedy reports whether inv tells the on-call to act and then gives
// them nothing to do: a verdict of action_suggested/action_required with no
// suggested_action on any root cause and no proposed action.
//
// Both fields are optional in submit_findings, so this payload is legal, and it
// shipped live on 2026-08-24 — a card headed "Action suggested" whose next-steps
// section was simply absent. The verdict badge is rendered from the verdict alone,
// so the header promises a remedy the body never carries; a card that does that
// costs more trust than one that says inconclusive.
//
// Placement, and the pre-verify/post-verify caveat, are exactly as for
// UnaccountedInconclusive above. As there, the notifier does not render straight off
// this predicate — see notify.remedyMissingForReader, which additionally requires
// that the card show no OTHER remedy (a validated prior resolution, a matched
// runbook).
func (inv Investigation) ActionWithoutRemedy() bool {
	if !inv.Verdict.ClaimsAction() {
		return false
	}
	for _, rc := range inv.RootCauses {
		if strings.TrimSpace(rc.SuggestedAction) != "" {
			return false
		}
	}
	for _, a := range inv.Actions {
		if strings.TrimSpace(a.Description) != "" {
			return false
		}
	}
	return true
}

// ClaimsAction reports whether a verdict tells the on-call to do something, and so
// obliges the delivered notification to show what. no_action and inconclusive make
// no such promise, and an empty/unknown verdict is a parse concern rather than a
// contract breach — it renders no badge at all, so it promises nothing.
//
// Defined in terms of Conclusive for the reason ValidVerdict gives: the three
// actionability verdicts stay enumerated once, so a fifth is one switch to update
// rather than two adjacent ones.
func (v Verdict) ClaimsAction() bool {
	return v.Conclusive() && v != VerdictNoAction
}

// MatchedEntry is the strongest pre-existing catalog entry an investigation's
// kb_search calls matched at clear-match strength. It closes a live visibility gap:
// when a full investigation's kb_search found a known runbook and used it, the
// delivered notification previously gave NO sign RunLore already had knowledge for
// the incident (the "Seen before"/Prior block only fires on a ledger recurrence, not
// when a full loop reuses a known entry). Path + Title always populate; URL only when
// a web link is cheaply derivable (else the notifier shows Path). Score is the BM25
// relevance of the matching hit — recorded so the clear-match bar can be tuned from
// live data, like the recall thresholds.
type MatchedEntry struct {
	Path          string  // catalog path of the matched entry (bundle-relative)
	Title         string  // entry title, for the human-facing line
	URL           string  // web link to the entry when cheaply derivable; "" ⇒ notifier shows Path
	SectionAnchor string  `json:"section_anchor,omitempty"` // e.g. "#resolution" when the matched entry has a ## Resolution heading
	Score         float64 // BM25 relevance score of the matching kb_search hit
}

// PriorKnowledge is what the knowledge base already says about a recurring
// incident: excerpts of the merged entry's Cause and (human-reviewed)
// Resolution sections, plus the entry's recall track record from the outcome
// ledger. Stamped at completion — never seen by the model — and only on FRESH
// investigations of a recurring TriggerKey whose merged entry is findable by
// dup-fingerprint; nil otherwise, so notifiers fall back to the counter+link.
//
// Cause and Resolution are KB body text of external origin (whatever a human wrote
// during review, verbatim), and they are stamped PAST investigate.redactInvestigation
// — so app.onInvestigationComplete secret-redacts them at the assignment. EntryPath
// is a catalog path and stays verbatim.
type PriorKnowledge struct {
	Cause      string // excerpt of the merged entry's "## Cause" section
	Resolution string // excerpt of "## Resolution" — carries the human's review edits, the payoff of curation
	EntryPath  string // catalog path of the merged entry
	Recalls    int    // times the entry answered an incident via instant recall
	Resolved   int    // recalls followed by an incident-resolved signal
}

// UsageTotals aggregates model token usage over a whole investigation: every
// model call summed — the ReAct loop, the adversarial verify pass, and any recall
// verification. It is carried on Investigation so notifiers and metrics can
// surface usage/cost without re-reading provider internals. Zero when no model
// call reported usage (e.g. a pure recall short-circuit).
type UsageTotals struct {
	ModelCalls        int     // number of model completions made
	InputTokens       int     // total input/prompt tokens, INCLUDING any served from cache (mirrors Usage.InputTokens)
	OutputTokens      int     // total generated/output tokens
	CachedInputTokens int     // subset of InputTokens that was a cache read (the saving)
	CostUSD           float64 // estimated cost; meaningful only when Priced (model.pricing configured)
	Priced            bool    // pricing was configured, so CostUSD is populated (may legitimately be 0)
}

// EvidenceGrounding records how many cited evidence bullets traced back to actual
// tool results during the adversarial verify pass. A nil pointer (or Total == 0)
// means grounding was not measured (verify skipped, failed, or no tool transcript
// was available to check against).
type EvidenceGrounding struct {
	Traced            int   `json:"traced,omitempty"`
	Total             int   `json:"total,omitempty"`
	UngroundedIndices []int `json:"ungrounded_indices,omitempty"` // sorted, unique 0-based indices into Hypothesis.Evidence
}

// Ratio returns Traced / Total in [0, 1], or 0 when g is nil or Total <= 0.
func (g *EvidenceGrounding) Ratio() float64 {
	if g == nil || g.Total <= 0 {
		return 0
	}
	return float64(g.Traced) / float64(g.Total)
}

// IsUngrounded reports whether evidence index idx was flagged as ungrounded.
// Safe to call on a nil receiver (returns false).
func (g *EvidenceGrounding) IsUngrounded(idx int) bool {
	if g == nil || len(g.UngroundedIndices) == 0 {
		return false
	}
	return slices.Contains(g.UngroundedIndices, idx)
}

// ProvenanceLink is a server-constructed, verified HTTPS link pointing to the
// exact change, file hunk, or KB section backing a finding. Never populated
// from model output.
type ProvenanceLink struct {
	Label string `json:"label"`          // short human label, e.g. "aaa111..bbb222" or "apps/harbor/values.yaml#L42"
	URL   string `json:"url"`            // validated https:// permalink
	Kind  string `json:"kind,omitempty"` // "compare" | "commit" | "file" | "kb"
}

// Hypothesis is one ranked root-cause candidate with its evidence.
type Hypothesis struct {
	Summary         string
	Confidence      float64
	ChangeRef       string
	ChangeURL       string           `json:"change_url,omitempty"`
	Provenance      []ProvenanceLink `json:"provenance,omitempty"`
	Evidence        []string
	Grounding       *EvidenceGrounding `json:"grounding,omitempty"`
	SuggestedAction string             // reversible-first
	Reversible      bool
}
