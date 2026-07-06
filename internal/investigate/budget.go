package investigate

import (
	"math"

	"github.com/Smana/runlore/internal/providers"
)

// budgetKillResult synthesises an unresolved investigation for use when the
// token-budget hard-kill fires (nudge fired, model did not submit findings in time).
func budgetKillResult(req Request) providers.Investigation {
	return providers.Investigation{
		Title:       req.Title,
		Resource:    req.Workload,
		Fingerprint: req.Fingerprint,
		Verdict:     providers.VerdictInconclusive,
		Unresolved: []string{
			"investigation stopped: token budget exceeded after nudge (model did not submit findings in time)",
		},
	}
}

// timeoutResult synthesises an unresolved investigation for use when the
// per-investigation deadline (LoopInvestigator.Timeout) fires before the loop
// submitted findings — so a hung git clone/diff or a slow model is reported, not
// silently retried into the same hang.
func timeoutResult(req Request) providers.Investigation {
	return providers.Investigation{
		Title:       req.Title,
		Resource:    req.Workload,
		Fingerprint: req.Fingerprint,
		Verdict:     providers.VerdictInconclusive,
		Unresolved: []string{
			"investigation stopped: per-investigation deadline exceeded before findings were submitted (e.g. a hung git clone/diff or a slow model)",
		},
	}
}

// refusalResult synthesises an unresolved investigation for when the model declines
// the turn — a provider safety/refusal stop reason (CompletionResponse.Refused()).
// The incident is reported as unresolved (no guessed root cause) rather than retried
// into the same refusal or misread as an empty prose turn.
func refusalResult(req Request) providers.Investigation {
	return providers.Investigation{
		Title:       req.Title,
		Resource:    req.Workload,
		Fingerprint: req.Fingerprint,
		Verdict:     providers.VerdictInconclusive,
		Unresolved: []string{
			"investigation stopped: the model declined to respond (safety-filtered or refused); no root cause was produced",
		},
	}
}

// estimateTokens approximates the request size (~4 chars/token) over everything
// re-sent each step: the system prompt, the full tool schemas (name +
// description + JSON Schema), and the message history — including the assistant
// tool-call JSON (m.ToolCalls[].Args), which also goes over the wire. Counting
// only m.Content systematically under-estimated a tool-heavy investigation,
// letting the hard-kill guard fire late or never. This estimate drives the
// PRE-request budget guard, so by itself it cannot use provider-reported usage
// (which only exists post-response, on CompletionResponse.Usage) — that is
// tokenCalibration's job: it scales this heuristic by the actual/heuristic ratio
// observed on the previous completion. Uncalibrated, this remains an
// under-estimate of the true wire size (it ignores JSON envelope/role overhead)
// but the right order of magnitude, which is what the hard-kill needs.
func estimateTokens(system string, msgs []providers.Message, tools []providers.ToolSpec) int {
	n := len(system)
	for _, t := range tools {
		n += len(t.Name) + len(t.Description) + len(t.Schema)
	}
	for _, m := range msgs {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Args)
		}
	}
	return n / 4
}

// tokenCalibration anchors the chars/4 pre-request heuristic to reality: after
// each completion whose provider reported usage, it records the ratio between the
// actual prompt size (Usage.InputTokens, which includes cached tokens) and the
// heuristic estimate computed for that same request, then scales subsequent
// estimates by it. A ratio survives compaction and append-only growth alike
// because it captures the heuristic's systematic bias (tokenizer density, JSON
// envelope/role overhead), not an absolute context size. The zero value is
// uncalibrated: estimates fall back to the pure heuristic, so providers that
// never report usage behave exactly as before.
type tokenCalibration struct {
	// ratio is actual InputTokens ÷ heuristic estimate from the most recent
	// completion that reported usage; 0 means uncalibrated. Floored at 1 on
	// observe, so the anchored estimate is never below the raw heuristic and the
	// budget guard can only fire EARLIER than uncalibrated, never later.
	ratio float64
}

// observe records the actual/heuristic ratio for a completed request: heuristic
// is estimateTokens computed for the request as sent, usage is the provider's
// report for it. Zero usage (provider didn't report) or a non-positive heuristic
// leaves the calibration unchanged.
func (c *tokenCalibration) observe(heuristic int, usage providers.Usage) {
	if heuristic <= 0 || usage.InputTokens <= 0 {
		return
	}
	r := float64(usage.InputTokens) / float64(heuristic)
	if r < 1 {
		r = 1 // safety floor: never estimate below the raw heuristic
	}
	c.ratio = r
}

// estimate returns the usage-anchored request-size estimate: estimateTokens
// scaled (rounded up) by the last observed actual/heuristic ratio. Uncalibrated,
// it is exactly the raw heuristic.
func (c *tokenCalibration) estimate(system string, msgs []providers.Message, tools []providers.ToolSpec) int {
	est := estimateTokens(system, msgs, tools)
	if c.ratio > 1 {
		est = int(math.Ceil(float64(est) * c.ratio))
	}
	return est
}

// heuristicTarget converts a token target expressed in anchored (real) tokens
// into the raw-heuristic space that compactHistory measures in, so a calibrated
// loop compacts down to a real target rather than an under-counted one.
// Uncalibrated, the target passes through unchanged.
func (c *tokenCalibration) heuristicTarget(target int) int {
	if c.ratio > 1 {
		return int(float64(target) / c.ratio)
	}
	return target
}

// overBudget reports whether est exceeds budget. budget <= 0 means unlimited.
func overBudget(est, budget int) bool { return budget > 0 && est > budget }

// defaultRecallTokensSavedEstimate is a conservative proxy used when
// MaxTokensPerInvestigation is unconfigured (0): ~50k tokens ≈ a medium-depth
// investigation (system prompt + ~20 tool turns).
const defaultRecallTokensSavedEstimate = 50_000

// budgetNudge is the single-use message injected once when the token estimate
// exceeds MaxTokensPerInvestigation, prompting the model to wrap up now.
const budgetNudge = "⚠️ token budget reached — call submit_findings now with your best current hypotheses and the evidence gathered so far."

// finalStepNudge is injected on the loop's LAST step (only one request remaining)
// when the model has not already been forced to conclude by the token-budget path.
// Paired with ToolChoice=submit_findings, it makes a non-converging model record a
// degraded verdict on its final turn instead of exhausting the step budget in
// silence (mitigates issue #234's blast radius).
const finalStepNudge = "This is your FINAL step — you have no tool calls remaining. Record your conclusion NOW by calling submit_findings: give your best current hypothesis with an honest (low, if unverified) confidence, or explicitly mark the incident unresolved. Do not answer in prose."
