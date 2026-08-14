// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

// verifyPrompt drives an adversarial review of an investigation's root causes —
// the defence against plausible-but-wrong findings (correlation passed off as
// causation, suggesting a revert of an unread change, ignoring a matching runbook).
const verifyPrompt = `You are a skeptical senior SRE reviewing another engineer's incident root causes.
Be adversarial: your job is to catch wrong-but-confident conclusions before they are published.

For EACH proposed root cause, judge it ONLY on the evidence given:
- reject — if it rests on correlation ("started after X") without a verified causal link, names a
  change whose diff was never read, blames a subsystem only because its tool errored, or contradicts
  the evidence. A guess does not become a root cause by being plausible.
- downgrade — plausible and partially evidenced, but not verified end-to-end (lower its confidence).
- keep — backed by concrete, causal evidence (e.g. the change was read AND matches the observed
  failure, or a logged error directly explains it).

Groundedness: each cited piece of evidence must trace to a tool result in the transcript excerpt
below. If a root cause's evidence cannot be found in the transcript, treat it as unverified — reject
or downgrade it. Absence of a tool result is not itself proof against a cause (the excerpt is
bounded and may omit some results), so weigh it alongside the other evidence rather than rejecting
solely on a missing line.

Call submit_verdicts once: one verdict per root cause by index, with a calibrated confidence and a
short reason.`

const submitVerdictsName = "submit_verdicts"

// submitVerdictsSpec is the structured-output tool for the verification pass.
func submitVerdictsSpec() providers.ToolSpec {
	return providers.ToolSpec{
		Name:        submitVerdictsName,
		Description: "Record your adversarial verdict on each proposed root cause.",
		Schema: `{"type":"object","properties":{"verdicts":{"type":"array","items":{"type":"object","properties":` +
			`{"index":{"type":"integer"},"verdict":{"type":"string","enum":["keep","downgrade","reject"]},` +
			`"confidence":{"type":"number"},"reason":{"type":"string"}},"required":["index","verdict"]}}},"required":["verdicts"]}`,
	}
}

type verdict struct {
	Index      int     `json:"index"`
	Verdict    string  `json:"verdict"`
	Confidence float64 `json:"confidence"`
	Reason     string  `json:"reason"`
}

func parseVerdicts(args string) ([]verdict, error) {
	var v struct {
		Verdicts []verdict `json:"verdicts"`
	}
	if err := json.Unmarshal([]byte(args), &v); err != nil {
		return nil, err
	}
	return v.Verdicts, nil
}

// verifyFindings runs one adversarial review pass over the investigation's root
// causes, rejecting correlation-only/unverified claims and downgrading unproven
// ones before delivery/curation. The verify completion's token usage is accumulated
// into totals (when non-nil) so the per-investigation cost includes the verify pass.
// transcript is the tool history the reviewer may treat as verified fact: the loop's
// accumulated messages on the full path, confirmRecall's current-state results on the
// recall short-circuit. A bounded, redacted excerpt of its tool results is fed to the
// reviewer so groundedness ("does this cause trace to a tool result?") can be checked
// rather than merely asserted.
//
// Passing nil is a claim, not merely an option: it says nothing here is independently
// verified, which leaves the groundedness rule with nothing to check against, so the
// reviewer can only downgrade. Reserve it for when that is actually true — handing nil
// to a path that DID gather evidence downgrades sound findings on principle, as the
// recall path once did (see confirmRecall).
//
// The second return, verified, reports whether an adversarial review actually
// happened: true when the returned investigation reflects a completed review
// (including the trivial case of no root causes to review — there is nothing an
// adversarial pass could reject), false when the review could not be completed (a
// model error, or a response with no usable verdicts) and inv is returned exactly as
// given.
//
// Callers differ in how load-bearing that signal is, and MUST differ deliberately:
//   - The full-investigation call site (loop.go, after the ReAct loop) may ignore
//     verified and keep findings as-is on failure. There, the findings were already
//     built from independently-gathered tool evidence before verify ever ran — verify
//     is a second opinion on real evidence, not the only check, so losing a real
//     finding to a down reviewer would be the worse failure mode. Best-effort is
//     correct there.
//   - The recall short-circuit call site (tryRecall, loop.go) MUST treat
//     verified==false as a forced fall-through to a full investigation. There is no
//     independently-gathered evidence on that path — the "as-is" investigation is text
//     and confidence lifted directly from a possibly-poisoned catalog entry, and verify
//     is the ENTIRE adversarial check. Passing it through unreviewed on a model error
//     would let untrusted catalog content bypass review by the simple expedient of the
//     reviewer being unavailable.
func (li *LoopInvestigator) verifyFindings(ctx context.Context, req Request, inv providers.Investigation, transcript []providers.Message, totals *providers.UsageTotals) (result providers.Investigation, verified bool) {
	if len(inv.RootCauses) == 0 {
		return inv, true
	}
	// Route the adversarial pass to a cheaper/faster model when one is configured;
	// otherwise reuse the main investigation model. Verify always runs (the honesty
	// guarantee) — this only lowers its cost, it never disables it.
	m := li.Model
	if li.VerifyModel != nil {
		m = li.VerifyModel
	}
	resp, err := m.Complete(ctx, providers.CompletionRequest{
		System:   verifyPrompt,
		Messages: []providers.Message{{Role: "user", Content: renderForReview(req, inv, transcript)}},
		Tools:    []providers.ToolSpec{submitVerdictsSpec()},
		// Force the tool: a reviewer that answers in prose silently skips the
		// honesty check (no verdicts ⇒ findings pass through unreviewed).
		ToolChoice: submitVerdictsName,
	})
	if err != nil {
		li.Log.Warn("verify pass failed; keeping findings as-is", "title", req.Title, "err", err)
		return inv, false
	}
	// Count the verify completion toward the per-investigation token/cost total.
	if totals != nil {
		addUsage(totals, resp.Usage)
	}
	var verds []verdict
	for _, tc := range resp.ToolCalls {
		if tc.Name == submitVerdictsName {
			verds, _ = parseVerdicts(tc.Args)
			break
		}
	}
	if len(verds) == 0 {
		// The model responded but produced no verdict for the reviewer to apply (wrong
		// tool called, or garbled/empty args) — no review actually happened, the same
		// practical outcome as an error. Logged distinctly (there is no `err` here) so
		// this degenerate case is not silently indistinguishable from a genuine approval.
		li.Log.Warn("verify pass returned no usable verdicts; keeping findings as-is", "title", req.Title)
		return inv, false
	}
	// Non-empty is not the same as "a review happened". applyVerdicts keeps any cause
	// the reviewer did not mention, so a response whose indices all fall outside
	// [0, len(RootCauses)) — {"verdicts":[{"index":7,"verdict":"reject"}]} against a
	// one-cause investigation — reviewed NOTHING while returning verified=true. On the
	// recall path that lets catalog text short-circuit an incident stamped
	// `Verified: true`; on the full path it launders an unreviewed finding.
	//
	// Reachable exactly where this repo already documents flaky forced tool_choice
	// (local OpenAI-compatible servers), which is when a fail-closed gate matters most.
	covered := 0
	for _, v := range verds {
		if v.Index >= 0 && v.Index < len(inv.RootCauses) {
			covered++
		}
	}
	if covered == 0 {
		li.Log.Warn("verify pass returned verdicts that reference no actual root cause; treating as unreviewed",
			"title", req.Title, "verdicts", len(verds), "root_causes", len(inv.RootCauses))
		return inv, false
	}
	return applyVerdicts(li, req, inv, verds), true
}

// applyVerdicts rewrites the investigation per the review: rejected root causes
// move to RuledOut and downgraded ones get a lower confidence. The verify pass
// is the honesty guarantee (docs/design.md:203) — it may only keep confidence
// equal or LOWER it, never raise. So a verdict's confidence is applied as a
// monotonic floor (min with the score the hypothesis entered with), both
// per-hypothesis and for the recomputed overall confidence.
func applyVerdicts(li *LoopInvestigator, req Request, inv providers.Investigation, verds []verdict) providers.Investigation {
	byIndex := map[int]verdict{}
	for _, v := range verds {
		byIndex[v.Index] = v
	}
	// Capture the pre-verify overall so the recompute below can only lower it.
	preVerifyOverall := inv.Confidence
	kept := make([]providers.Hypothesis, 0, len(inv.RootCauses))
	for i, rc := range inv.RootCauses {
		v, ok := byIndex[i]
		// Normalize before comparing. The switch below has no fall-through for an
		// unrecognized verdict, so "KEEP", "approve" or a stray space used to match
		// no case at all and DROP the root cause silently — on the full-investigation
		// path that discards a real, evidenced finding and forces the verdict to
		// inconclusive, with nothing logged to say why.
		verd := strings.ToLower(strings.TrimSpace(v.Verdict))
		switch {
		case !ok || verd == "keep":
			// A keep carrying a confidence may only lower the score, never raise
			// it; a keep with no/zero confidence leaves the original untouched.
			if ok && v.Confidence > 0 {
				rc.Confidence = min(rc.Confidence, clamp01(v.Confidence))
			}
			kept = append(kept, rc)
		case verd == "downgrade":
			if v.Confidence > 0 {
				rc.Confidence = min(rc.Confidence, clamp01(v.Confidence))
			} else {
				rc.Confidence /= 2
			}
			kept = append(kept, rc)
		case verd == "reject":
			// A rejected hypothesis is honesty about what was disproven, not an open
			// question for a human — it belongs in RuledOut (with the disproving
			// reason), not Unresolved.
			inv.RuledOut = append(inv.RuledOut, fmt.Sprintf("%s — %s", rc.Summary, v.Reason))
			li.Log.Info("verify: rejected root cause", "title", req.Title, "summary", rc.Summary, "reason", v.Reason)
		default:
			// An unrecognized verdict is a reviewer malfunction, not a judgement about
			// the finding. Keep the cause — silently deleting evidence because the
			// reviewer said something unparseable is the worst available answer — and
			// log loudly enough that the malfunction is visible.
			li.Log.Warn("verify: unrecognized verdict; keeping the root cause unchanged",
				"title", req.Title, "verdict", v.Verdict, "summary", rc.Summary)
			kept = append(kept, rc)
		}
	}
	inv.RootCauses = kept
	inv.Verified = len(kept) > 0
	if len(kept) == 0 && inv.Verdict != "" {
		// Everything the model concluded was refuted by the adversarial pass — an
		// actionable verdict would be a confident claim with no surviving support.
		inv.Verdict = providers.VerdictInconclusive
	}
	var maxc float64
	for _, rc := range kept {
		if rc.Confidence > maxc {
			maxc = rc.Confidence
		}
	}
	// Never raise the overall above what it was before the review.
	inv.Confidence = min(preVerifyOverall, maxc)
	// If every root cause was rejected, drop the proposed actions too — a
	// remediation motivated by a rejected hypothesis must not survive the review
	// (the exact failure the verify pass exists to prevent).
	if len(kept) == 0 {
		inv.Actions = nil
	}
	return inv
}

// renderForReview presents the incident + ranked root causes (with their evidence)
// for the adversarial reviewer to judge, followed by a bounded, redacted excerpt of
// the tool transcript so groundedness can be verified against the actual tool output.
func renderForReview(req Request, inv providers.Investigation, transcript []providers.Message) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Incident: %s. Workload: %s/%s. Reason: %s.\n\nProposed root causes to review:\n",
		req.Title, req.Workload.Namespace, req.Workload.Name, req.Reason)
	for i, rc := range inv.RootCauses {
		fmt.Fprintf(&b, "[%d] (confidence %.2f) %s\n", i, rc.Confidence, rc.Summary)
		if rc.ChangeRef != "" {
			fmt.Fprintf(&b, "    change_ref: %s\n", rc.ChangeRef)
		}
		for _, e := range rc.Evidence {
			fmt.Fprintf(&b, "    - %s\n", e)
		}
	}
	if ex := transcriptExcerpt(transcript); ex != "" {
		fmt.Fprintf(&b, "\nTool transcript excerpt (most recent tool results, may be truncated — "+
			"check each cited evidence against it):\n%s", ex)
	}
	return b.String()
}

// maxVerifyTranscriptBytes hard-caps the tool-transcript excerpt fed into the
// verify pass (and the eval judge). Feeding transcripts grows the prompt, so the
// budget is kept deliberately small and conservative: enough to check the handful
// of decision-relevant tool results without materially shifting verify verdicts or
// cost. Purely additive context — the excerpt never replaces the findings.
const maxVerifyTranscriptBytes = 4000

// transcriptExcerpt builds a bounded excerpt of the tool results in a loop's message
// history, for grounding the verify/judge pass. Tool output is ALREADY redacted when
// it enters history (loop.go's egress boundary); redact.Secrets is re-applied here as
// idempotent defense in depth so a caller passing an unredacted transcript still can't
// leak. The MOST RECENT tool results are preferred (they are the ones the model saw
// just before concluding, so the most decision-relevant) and the excerpt is assembled
// oldest-first up to maxVerifyTranscriptBytes. Returns "" when there are no tool
// results — a loop that concluded without calling a tool, or a recall whose confirm
// step gathered nothing (a confirmed recall DOES carry results; see confirmRecall).
func transcriptExcerpt(transcript []providers.Message) string {
	// Collect tool-result contents with a short call-name label (from the assistant
	// turn that requested them) so the reviewer can tell which tool produced what.
	names := map[string]string{} // ToolCallID -> tool name
	for _, m := range transcript {
		for _, tc := range m.ToolCalls {
			names[tc.ID] = tc.Name
		}
	}
	type entry struct{ label, content string }
	var entries []entry
	for _, m := range transcript {
		if m.Role != "tool" || strings.TrimSpace(m.Content) == "" {
			continue
		}
		label := names[m.ToolCallID]
		if label == "" {
			label = "tool"
		}
		entries = append(entries, entry{label: label, content: m.Content})
	}
	if len(entries) == 0 {
		return ""
	}
	// Walk newest→oldest, prepending each entry while it fits the byte budget, so the
	// excerpt keeps the most decision-relevant (latest) results and stays capped.
	var kept []string
	total := 0
	for i := len(entries) - 1; i >= 0; i-- {
		block := fmt.Sprintf("[%s] %s", entries[i].label, entries[i].content)
		if total+len(block) > maxVerifyTranscriptBytes {
			// Include a final truncated fragment of this block so the budget is used
			// fully and the cap is a hard ceiling (never exceeded).
			if remaining := maxVerifyTranscriptBytes - total; remaining > 0 {
				kept = append([]string{block[:remaining]}, kept...)
			}
			break
		}
		kept = append([]string{block}, kept...)
		total += len(block) + 1 // +1 for the join newline
	}
	return redact.Secrets(strings.Join(kept, "\n"))
}
