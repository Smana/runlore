// SPDX-License-Identifier: Apache-2.0

// Package notify delivers completed investigations to chat (Slack, Matrix).
package notify

import (
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/thread"
)

// curateErrorRunes caps the rendered curate-failure reason. It is a DIAGNOSTIC pointer,
// not the payload: the full error is in the logs and the failure is counted by
// runlore_curations_total{result="error"}, so the card only needs enough to tell an
// operator which class of failure it was (auth, rate limit, validation). Deliberately
// well under Slack's 3000-char section limit, because this line shares that budget with
// the entire finding it is appended to.
//
// 300 and not the ~120 a "which class was it" pointer sounds like it needs, because
// truncate cuts the HEAD: a wrapped Go error puts the diagnosis LAST
// ("open PR: github GET /repos/o/r/git/ref/heads/main: status 403: Resource not
// accessible by integration" is already 118 runes with a two-character owner/repo), so a
// 120-rune cap would reliably keep the call site and drop the status and the message —
// the only two things an operator can act on. The soft-wrap forgery a tighter cap would
// have addressed is closed by the inline code span instead (see curateFailureReason).
const curateErrorRunes = 300

// curateFailureReason renders inv.CurateError into the one form every surface prints,
// or "" when there is nothing to report. Both call sites go through it so the Slack
// card and Format cannot drift into telling a human two different things — the drift
// that made the first version of this fix miss the Slack card entirely, which is the
// exact surface #506 reports.
//
// redact → strip backticks → flatten → cap. The first three are thread.SafeErrorText,
// which is where that pipeline now lives; see its doc comment for what each one answers
// and why the order is not free. The cap stays HERE because the two surfaces that
// publish a forge error bound it in different units for different reasons: this counts
// runes against a Slack section limit, and internal/thread counts bytes against a chat
// message budget it shares with a note quote.
//
// It calls that helper rather than keeping this pipeline local, and the reason is the
// drift this comment used to document from the wrong side. The paragraph below argued
// that publishing a forge error was already precedented because "internal/thread already
// posts err.Error() from the same forge client into the same room, uncapped and
// unredacted" — which was true, and was a defect there rather than a licence here. Both
// surfaces now go through one function, so the next measure added to either reaches
// both.
//
// The inline code span the callers add is the fourth measure, and it is what flattening
// alone does not buy: a reason this long soft-wraps in every client, and a continuation
// line starts at the left margin with no quote bar (unlike thread.QuoteUntrusted, whose
// "> " prefix survives wrapping). A padded body could otherwise put a forged
// "📚 Knowledge base: https://evil…" at the start of a visual line. Inside a code span it
// is <code> on Matrix and monospaced-with-a-background on Slack — visibly not RunLore's
// own voice — and it degrades to literal backticks on the CLI, a surface with one reader
// who ran the command themselves.
//
// The forge's own words are kept rather than classified into "auth / rate limit /
// validation" deliberately. Classifying would need a parser over forge error strings —
// a second, narrower copy of a vocabulary this package does not own, the same drift the
// paragraph above exists to avoid — and it would drop "Resource not accessible by
// integration", the half that told the operator the fix was a GitHub App permission and
// not a broken token. If server-supplied topology (internal DNS, RFC1918, a GHE proxy
// banner) is judged too much for a chat room, that is a property of every forge error
// RunLore publishes anywhere and belongs in redact as a topology filter at the single
// egress chokepoint — not as a special case on this one line.
func curateFailureReason(inv providers.Investigation) string {
	if inv.CurateError == "" {
		return ""
	}
	return truncate(thread.SafeErrorText(inv.CurateError), curateErrorRunes)
}

// inconclusiveAccountHeader labels the account an `inconclusive` verdict owes its
// reader when the card has no Why to be the answer instead: what stopped the
// investigation. It reuses the verdict's own ❓ deliberately — the reader is being
// told what that badge MEANS on this card, not shown a second, unrelated section.
const inconclusiveAccountHeader = "*❓ Why this is inconclusive*"

// unaccountedInconclusiveNotice is what a card says in place of that account when
// the payload supplies none — providers.Investigation.UnaccountedInconclusive.
//
// It exists because the alternative shipped live on 2026-08-18: a card whose entire
// body was "❓ Inconclusive — <a definite conclusion about self-healed node churn>"
// and "⚪ confidence not stated". No Why, no evidence, no next steps, and nothing in
// the thread either, because there was nothing to put there. The verdict says "I
// could not determine the cause"; the title states a cause. A reader has no way to
// tell which half to believe, and the honest answer — neither — was the one thing
// the card did not say.
//
// So it says it. Not a guess at what the model meant, and not a suppression of the
// title (the conclusion may well be right; the sibling KubeNodeUnreachable
// investigation reached the same one at 75% with full evidence): a plain statement
// that nothing on this card backs it, which is exactly what makes an incomplete run
// distinguishable from a finding.
//
// It says "this RUN produced", not "this card carries", and the difference is
// load-bearing: the card may well carry an answer this run did not reach. A
// recurring incident stamps Prior (block 3, "Seen before ×N" with the merged
// entry's cause and human-reviewed resolution) and a kb_search hit stamps
// MatchedKnowledge (block 4) regardless of what the fresh run concluded, so a bare
// `inconclusive` on a recurrence renders a prior cause three blocks above this
// notice. Claiming the card carries nothing would tell the on-call to disregard an
// answer that is on screen. The claim scoped to the run is true in every case.
const unaccountedInconclusiveNotice = "⚠️ *No account given* — the investigation reported no cause, " +
	"no open question and no data gap, so this run produced neither an answer nor a reason it could not " +
	"reach one. Treat it as an incomplete run, not a finding."

// inconclusiveAccount returns the lead line and the bullets a card renders to
// account for an `inconclusive` verdict, or ("", nil) when it owes none.
//
// `lead` is the section's FIRST LINE, and it has two shapes: normally the header
// the bullets sit under, but on the unaccounted arm the standalone notice, with no
// bullets to follow it. Callers must render lead first and the bullets (possibly
// none) under it — which is what makes the two arms one code path.
//
// It owes one exactly when the verdict is inconclusive AND no root cause will
// render: with a cause on the card the reader has the answer whatever the label says
// (the mislabel #471 documents — "a recurrence of a fault you DID identify is NOT
// inconclusive"), and a conclusive verdict has nothing to account for. What is owed
// is the model's own open questions and data gaps, in that order — the channels
// every synthesised stop in internal/investigate (budget kill, timeout, refusal,
// non-convergence) writes its blocker into — falling back to the notice above when
// it gave neither.
//
// This is the one place the Slack SUMMARY renders data gaps, against the rule that
// they belong only in the threaded detail. That rule is about fold space: gaps must
// not push the answer below "Show more". On this card there IS no answer to push
// down, and the gap is the closest thing to one the run produced — a reader who has
// to open a thread to learn that RBAC blocked the pod logs is reading a card that
// told them nothing.
func inconclusiveAccount(inv providers.Investigation) (lead string, bullets []string) {
	if inv.Verdict != providers.VerdictInconclusive || len(inv.RootCauses) > 0 {
		return "", nil
	}
	// slices.Concat rather than append(inv.Unresolved, inv.DataGaps...): the latter
	// would stomp the caller's backing array when Unresolved has spare capacity.
	bullets = slices.Concat(inv.Unresolved, inv.DataGaps)
	if len(bullets) == 0 {
		// The RuledOut fallback. The verify pass is the reason this is not simply the
		// notice: applyVerdicts (internal/investigate/verify.go) moves every REJECTED
		// hypothesis into RuledOut with its disproving reason, empties RootCauses and
		// forces the verdict to `inconclusive` — without touching Unresolved or DataGaps.
		// Verify is on by default at every call site, so a run whose reviewer refuted all
		// of the model's hypotheses arrives here having accounted for itself perfectly
		// well, and the notice would call it an incomplete run while the thread reply
		// listed the refutations. What is ruled out IS the reason this run reached no
		// answer, so it is the account when it is the only one — and it stays a fallback,
		// never merged into the list above, because "what it is not" is worth fold space
		// only when there is no "why don't you know" to spend it on.
		//
		// The predicate on the contract deliberately excludes RuledOut (see
		// providers.Investigation.UnaccountedInconclusive): it answers "did the MODEL
		// contradict itself", which is what the loop logs at submit time, before verify
		// can rewrite anything. This answers the later, different question "is there
		// anything the READER can use", so the two must not be the same test.
		if len(inv.RuledOut) == 0 {
			return unaccountedInconclusiveNotice, nil
		}
		bullets = slices.Clone(inv.RuledOut)
	}
	return inconclusiveAccountHeader, bullets
}

// unaccountedForReader reports whether nothing the delivered notification can show
// accounts for an `inconclusive` verdict — the contract predicate AND no ruled-out
// hypotheses, for the reason spelled out in inconclusiveAccount. Format consults it
// directly because that message has no fold and prints the honest-limit lists in
// full below, so it needs the notice decision without the rendering.
func unaccountedForReader(inv providers.Investigation) bool {
	return inv.UnaccountedInconclusive() && len(inv.RuledOut) == 0
}

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
// notifiers (CLI, Matrix). Its ordering and de-duplication deliberately mirror
// the Slack card (slack.go's summaryBlocks) — the two must never diverge in
// what they claim: title → verdict, alone → confidence, shown exactly once
// (+ recall disambiguation when a recalled entry's own confidence differs from
// the delivered one) → seen-before/recall context → matched-known-runbook →
// root causes (the "why", rendered in full — there is no thread to defer to
// here) → honest limits (unresolved / ruled out / data gaps — kept, unlike the
// Slack SUMMARY card: this single flat message has no thread for them to live
// in instead) → suggested actions → trigger-time metadata (resource/alert
// facts/started) — orienting detail, so it drops below the answer and the
// action rather than leading with it → footer (verified / KB link / usage).
//
// Invariant: every literal this function emits (labels, separators, bullets)
// avoids the three mrkdwn-meta chars & < >. The Slack fallback is
// escapeMrkdwn(Format(inv)) — only untrusted content (evidence, summaries) may
// carry those chars, so escaping leaves the scaffolding intact. Use · • and
// *bold*; TestFormatScaffoldingHasNoMrkdwnMeta guards it.
func Format(inv providers.Investigation) string {
	var b strings.Builder
	// The title anchors the message the same way the Slack header does — without
	// it, a CLI/Matrix reader has no idea which incident this text describes.
	fmt.Fprintf(&b, "🔍 %s\n", displayTitle(inv.Title))
	// The model verdict is the headline actionability call, alone — the title
	// line above already named the incident.
	if emoji, label := verdictBadge(inv.Verdict); label != "" {
		fmt.Fprintf(&b, "%s Verdict: %s\n", emoji, label)
	}
	// Confidence — shown once. confidenceBadge is the SAME function Slack calls,
	// so the two never headline different numbers for the same investigation.
	emoji, level, pct, stated := confidenceBadge(inv)
	fmt.Fprintf(&b, "%s %s\n", emoji, confidenceText(level, pct, stated))
	if note := recallConfidenceNote(inv, pct); note != "" {
		fmt.Fprintf(&b, "   (%s)\n", note)
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
		// An unstated per-cause confidence renders as nothing rather than "(0%)" —
		// same reason as the headline; see confidenceText.
		if rc.Confidence > 0 {
			fmt.Fprintf(&b, "%d. *%s* (%.0f%%)\n", i+1, rc.Summary, rc.Confidence*100)
		} else {
			fmt.Fprintf(&b, "%d. *%s*\n", i+1, rc.Summary)
		}
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
	// This flat message already prints the open questions, ruled-out hypotheses and
	// data gaps below, so a genuine inconclusive accounts for itself here without
	// help. Only the self-contradicting shape needs saying out loud — otherwise this
	// message, like the Slack card the notice was written for, is a verdict label with
	// nothing behind it. Placed above the honest-limit sections it stands in for, and
	// gated on unaccountedForReader rather than the raw contract predicate so it can
	// never print immediately above a *Ruled out:* section enumerating the very
	// reasons it just claimed do not exist.
	if unaccountedForReader(inv) {
		b.WriteString(unaccountedInconclusiveNotice + "\n")
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
	// Trigger-time metadata — resource, alert facts, incident start. Orienting
	// detail, not the thing to lead with, so it sits below the answer (root
	// causes) and the action (suggested steps) rather than above them, mirroring
	// the Slack card's move.
	if ref := inv.Resource.Ref(); ref != "" {
		fmt.Fprintf(&b, "Resource: %s\n", strings.TrimSpace(inv.Resource.Kind+" "+ref))
	}
	if meta := metadataLine(inv); meta != "" {
		fmt.Fprintf(&b, "%s\n", meta)
	}
	if !inv.StartedAt.IsZero() {
		fmt.Fprintf(&b, "Started: %s\n", inv.StartedAt.UTC().Format(time.RFC3339))
	}
	// Footer — provenance: verification, the KB link, and the usage/cost summary.
	// Appended ONLY to the shared delivery message — never to the curated KB body
	// (the curator builds its own body and does not call Format), so cost never
	// pollutes the knowledge base.
	if inv.Verified {
		b.WriteString("✓ verified\n")
	}
	if inv.CuratedURL != "" {
		fmt.Fprintf(&b, "📚 Knowledge base: %s\n", inv.CuratedURL)
	}
	// The mirror of the line above, and the reason it exists: an EMPTY CuratedURL is
	// ambiguous. It is the normal state for a finding below curate.min_confidence or
	// carrying a skip_verdicts verdict — by far the common case — so "no KB link" cannot
	// tell a human whether the learning loop is working. Without this line a failed write
	// is indistinguishable from a skipped one, which is how a 403 ran unnoticed on a live
	// deployment until an operator happened to write a thread note (that path DOES report
	// it). The counter for alerting already exists: runlore_curations_total{result="error"}.
	//
	// The Slack card renders the same fact in its footer (slack.go, summaryBlocks step 8);
	// both go through curateFailureReason, which is where the safety measures and the
	// reasoning for them live.
	if reason := curateFailureReason(inv); reason != "" {
		fmt.Fprintf(&b, "⚠️ Could not save to the knowledge base: `%s`\n", reason)
	}
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
