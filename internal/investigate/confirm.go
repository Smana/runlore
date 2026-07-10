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
func (li *LoopInvestigator) confirmRecall(ctx context.Context, req Request, inv providers.Investigation) (providers.Investigation, bool) {
	if req.Workload.Namespace == "" || len(inv.RootCauses) == 0 {
		return inv, false
	}
	byName := make(map[string]Tool, len(li.Tools))
	for _, t := range li.Tools {
		byName[t.Name()] = t
	}
	gathered := false
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
		gathered = true
	}
	return inv, gathered
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
