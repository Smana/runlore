package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
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
// ones before delivery/curation. Best-effort: on any verifier error the findings
// pass through unchanged (verification must never lose a real finding). The verify
// completion's token usage is accumulated into totals (when non-nil) so the
// per-investigation cost includes the verify pass.
func (li *LoopInvestigator) verifyFindings(ctx context.Context, req Request, inv providers.Investigation, totals *providers.UsageTotals) providers.Investigation {
	if len(inv.RootCauses) == 0 {
		return inv
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
		Messages: []providers.Message{{Role: "user", Content: renderForReview(req, inv)}},
		Tools:    []providers.ToolSpec{submitVerdictsSpec()},
		// Force the tool: a reviewer that answers in prose silently skips the
		// honesty check (no verdicts ⇒ findings pass through unreviewed).
		ToolChoice: submitVerdictsName,
	})
	if err != nil {
		li.Log.Warn("verify pass failed; keeping findings as-is", "title", req.Title, "err", err)
		return inv
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
		return inv
	}
	return applyVerdicts(li, req, inv, verds)
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
		switch {
		case !ok || v.Verdict == "keep":
			// A keep carrying a confidence may only lower the score, never raise
			// it; a keep with no/zero confidence leaves the original untouched.
			if ok && v.Confidence > 0 {
				rc.Confidence = min(rc.Confidence, clamp01(v.Confidence))
			}
			kept = append(kept, rc)
		case v.Verdict == "downgrade":
			if v.Confidence > 0 {
				rc.Confidence = min(rc.Confidence, clamp01(v.Confidence))
			} else {
				rc.Confidence /= 2
			}
			kept = append(kept, rc)
		case v.Verdict == "reject":
			// A rejected hypothesis is honesty about what was disproven, not an open
			// question for a human — it belongs in RuledOut (with the disproving
			// reason), not Unresolved.
			inv.RuledOut = append(inv.RuledOut, fmt.Sprintf("%s — %s", rc.Summary, v.Reason))
			li.Log.Info("verify: rejected root cause", "title", req.Title, "summary", rc.Summary, "reason", v.Reason)
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
// for the adversarial reviewer to judge.
func renderForReview(req Request, inv providers.Investigation) string {
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
	return b.String()
}
