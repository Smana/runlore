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

// recallUnconfirmedCap is the recall-confidence ceiling applied when current cluster
// state could not be gathered to confront the recalled entry.
const recallUnconfirmedCap = 0.70

// recallConfirmTools are the read-only, namespace-scoped checks used to confront a
// recalled finding with current cluster state, in priority order. They are the same
// tools the agent uses, resolved from the loop's tool set.
var recallConfirmTools = []string{"pod_status", "kube_events"}

// confirmRecall gathers current cluster state for the recalled workload and appends
// it to the top hypothesis's evidence, so the verify pass can judge the recalled
// cause against reality rather than a tautology. Best-effort: a missing namespace,
// absent tools, or a tool error gathers nothing. Empty output does not count, but
// "no pods"/"no events" does — that IS real current state.
//
// It returns those results in two forms, which are not redundant:
//
//   - the evidence bullets are what the DELIVERED card shows a human;
//   - the transcript is what the REVIEWER is allowed to treat as verified fact.
//
// The distinction matters because verifyPrompt's groundedness rule (verify.go)
// requires each cited piece of evidence to trace to a tool result in the transcript
// excerpt, and treats what it cannot find there as unverified. The recall path used
// to pass a nil transcript, so renderForReview emitted no excerpt at all and that
// rule was unsatisfiable BY CONSTRUCTION: every recalled finding was, correctly by
// its own instructions, downgraded. The confirm output was sitting right there in the
// evidence list, but as an assertion by the author rather than as tool output the
// reviewer could check against — precisely the distinction the rule draws.
//
// Handing over the same results as a transcript makes the review sharper, not softer:
// a reviewer that can read current state can now CONTRADICT a stale or poisoned entry
// (entry says pods are OOMKilling, pod_status shows them Running) instead of being
// reduced to "cannot verify" on everything.
//
// A nil transcript means nothing was gathered — the only state the caller needs, and
// the case it de-rates via recallUnconfirmedCap.
func (li *LoopInvestigator) confirmRecall(ctx context.Context, req Request, inv providers.Investigation) (providers.Investigation, []providers.Message) {
	if req.Workload.Namespace == "" || len(inv.RootCauses) == 0 {
		return inv, nil
	}
	byName := make(map[string]Tool, len(li.Tools))
	for _, t := range li.Tools {
		byName[t.Name()] = t
	}
	// Invariant across the loop (it encodes the namespace only). Recording the SAME
	// string the call was made with also keeps the transcript's args honest by
	// construction, rather than resting on two marshals agreeing.
	args := confirmArgs(req.Workload)
	// One synthetic assistant turn carrying the calls, then one tool turn per result —
	// the shape transcriptExcerpt walks to label each result with the tool that
	// produced it. Call IDs are deterministic (no loop ran, so nothing else mints them).
	var calls []providers.ToolCall
	var results []providers.Message
	for _, name := range recallConfirmTools {
		t, ok := byName[name]
		if !ok {
			continue
		}
		out, err := t.Call(ctx, args)
		if err != nil {
			if li.Log != nil {
				li.Log.Debug("recall confirm tool failed", "tool", name, "err", err)
			}
			continue
		}
		if out = strings.TrimSpace(out); out == "" {
			continue
		}
		// The same LLM-vendor egress boundary dispatchTools applies to every tool
		// result. This path calls the tool directly rather than through runTool, so
		// this is the only place it can be applied — and it must be, because BOTH
		// forms below reach the verify model, and redactInvestigation does not run
		// until deliver(), long after verify. Redact before truncating so a secret
		// near the cap is still masked.
		out, _ = truncateOutput(redact.Secrets(out), li.MaxToolOutputBytes)
		inv.RootCauses[0].Evidence = append(inv.RootCauses[0].Evidence,
			fmt.Sprintf("current state — %s:\n%s", name, out))
		id := "recall-confirm-" + name
		calls = append(calls, providers.ToolCall{ID: id, Name: name, Args: args})
		results = append(results, providers.Message{Role: "tool", ToolCallID: id, Content: out})
	}
	if len(results) == 0 {
		return inv, nil
	}
	return inv, append([]providers.Message{{Role: "assistant", ToolCalls: calls}}, results...)
}

// confirmArgs builds the JSON args for a confirmatory tool: namespace-scoped, but
// deliberately NOT scoped to the workload object.
//
// A recalled incident's ROOT CAUSE frequently lives on a SIBLING resource in the same
// namespace — a Crossplane AccessKey, a dependency, an upstream — not on the alerting
// pod itself. Scoping kube_events to the pod (its old behaviour) captured the SYMPTOM
// ("pod failing, secret key missing") but HID the cause (the AccessKey's
// "LimitExceeded: AccessKeysPerUser: 2" Warning lives on a different object). The verify
// pass, judging the recalled cause only on the gathered evidence, then couldn't confirm
// it end-to-end and systematically DOWNGRADED a correct recall. kube_events is
// Warning-only + namespace-wide by default ("causes that live nearby"), which is exactly
// the causal context verify needs to keep a right recall's confidence intact.
func confirmArgs(w providers.Workload) string {
	b, _ := json.Marshal(map[string]string{"namespace": w.Namespace})
	return string(b)
}

// capRecallConfidence lowers the investigation's overall and per-hypothesis
// confidence to at most ceiling (it never raises any value).
func capRecallConfidence(inv providers.Investigation, ceiling float64) providers.Investigation {
	if inv.Confidence > ceiling {
		inv.Confidence = ceiling
	}
	for i := range inv.RootCauses {
		if inv.RootCauses[i].Confidence > ceiling {
			inv.RootCauses[i].Confidence = ceiling
		}
	}
	return inv
}
