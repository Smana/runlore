package investigate

import "github.com/Smana/runlore/internal/providers"

// budgetKillResult synthesises an unresolved investigation for use when the
// token-budget hard-kill fires (nudge fired, model did not submit findings in time).
func budgetKillResult(req Request) providers.Investigation {
	return providers.Investigation{
		Title:       req.Title,
		Resource:    req.Workload,
		Fingerprint: req.Fingerprint,
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
		Unresolved: []string{
			"investigation stopped: per-investigation deadline exceeded before findings were submitted (e.g. a hung git clone/diff or a slow model)",
		},
	}
}

// estimateTokens approximates the request size (~4 chars/token) over the system
// prompt plus the full message history — the cost actually re-sent each step.
// Provider-reported usage is not exposed in CompletionResponse today.
func estimateTokens(system string, msgs []providers.Message) int {
	n := len(system)
	for _, m := range msgs {
		n += len(m.Content)
	}
	return n / 4
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
