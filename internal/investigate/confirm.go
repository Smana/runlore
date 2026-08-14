// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
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
// absent tools, or a tool error yields gathered=false. gathered is true when at
// least one confirm tool returned non-empty output (including "no pods"/"no events"
// — still real current state).
//
// It ALSO returns those results shaped as a tool transcript, which the caller passes
// to verifyFindings. Both forms are needed and they are not redundant:
//
//   - the evidence bullets are what the DELIVERED card shows a human;
//   - the transcript is what the REVIEWER is allowed to treat as verified fact.
//
// The verify prompt's central rule is "each cited piece of evidence must trace to a
// tool result in the transcript excerpt below … if it cannot be found in the
// transcript, treat it as unverified — reject or downgrade it". The recall path used
// to pass a nil transcript, so renderForReview emitted no excerpt at all and that
// rule was unsatisfiable BY CONSTRUCTION: every recalled finding was, correctly by
// its own instructions, downgraded. The confirm output was sitting right there in the
// evidence list, but as an assertion by the author rather than as tool output the
// reviewer could check against — which is precisely the distinction the rule draws.
//
// Handing over the same results as a transcript makes the review sharper, not softer:
// a reviewer that can read current state can now CONTRADICT a stale or poisoned entry
// (entry says pods are OOMKilling, pod_status shows them Running) instead of being
// reduced to "cannot verify" on everything.
func (li *LoopInvestigator) confirmRecall(ctx context.Context, req Request, inv providers.Investigation) (providers.Investigation, []providers.Message, bool) {
	if req.Workload.Namespace == "" || len(inv.RootCauses) == 0 {
		return inv, nil, false
	}
	byName := make(map[string]Tool, len(li.Tools))
	for _, t := range li.Tools {
		byName[t.Name()] = t
	}
	gathered := false
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
		out, err := t.Call(ctx, confirmArgs(req.Workload))
		if err != nil {
			if li.Log != nil {
				li.Log.Debug("recall confirm tool failed", "tool", name, "err", err)
			}
			continue
		}
		if out = strings.TrimSpace(out); out == "" {
			continue
		}
		inv.RootCauses[0].Evidence = append(inv.RootCauses[0].Evidence,
			fmt.Sprintf("current state — %s:\n%s", name, out))
		id := "recall-confirm-" + name
		calls = append(calls, providers.ToolCall{ID: id, Name: name, Args: confirmArgs(req.Workload)})
		results = append(results, providers.Message{Role: "tool", ToolCallID: id, Content: out})
		gathered = true
	}
	if !gathered {
		return inv, nil, false
	}
	transcript := append([]providers.Message{{Role: "assistant", ToolCalls: calls}}, results...)
	return inv, transcript, true
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
