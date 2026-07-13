// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// rejectedRecall builds a Recall with TWO structurally-agreeing candidates:
//
//   - the WRONG one scores high enough to clear every gate and fire instant recall;
//   - the RIGHT one sits below it in lexical order.
//
// This is the live Harbor incident, reduced. The alert says only "HelmRelease
// InstallFailed"; the KB holds a past capacity-shortage incident with exactly that
// symptom, and — separately — the incident that actually explains this one. Recall
// picks the capacity entry on symptom tokens alone, because at recall time the alert
// text is all there is.
func rejectedRecall() *Recall {
	return &Recall{
		MinScore: 1.0, MarginGap: 0.5, SoloFloor: 1.0,
		Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{
				Entry: catalog.Entry{
					Title:    "Harbor HelmRelease stuck InstallFailed after a cluster capacity shortage",
					Path:     "capacity.md",
					Resource: "tooling/harbor",
					Body:     "## Cause\nThe cluster had no schedulable capacity.\n\n## Resolution\nWait for Karpenter.\n",
				},
				Score: 9.0, // clears every gate → instant recall fires on the WRONG entry
			},
			{
				Entry: catalog.Entry{
					Title:    "Harbor Registry Down due to IAM Access Key Quota Limit",
					Path:     "iam.md",
					Resource: "tooling/harbor",
					Body:     "## Cause\nAccessKey/xplane-harbor hit AccessKeysPerUser: 2.\n\n## Resolution\nDelete an unused IAM access key.\n",
				},
				Score: 2.0,
			},
		}},
	}
}

func rejectedReq() Request {
	return Request{
		Title:       "HelmRelease/harbor InstallFailed",
		Fingerprint: "fp-reject",
		Workload:    providers.Workload{Namespace: "tooling", Name: "harbor"},
		Labels:      map[string]string{"alertname": "HelmReleaseInstallFailed"},
	}
}

// TestNearMissSurfacedAfterVerifyRejection is the regression test for the inverted
// incentive: a recall that FIRED and was then refuted by verify must not leave the
// loop with LESS context than a recall that never fired at all.
//
// Before this fix, the near-miss lookup lived only in the `entry == nil` branch of
// tryRecall, and the verify-rejection path did a bare `return nil, false`. So a
// confidently-WRONG catalog entry SUPPRESSED the lead that a merely-weak one would
// have surfaced: the loop restarted from zero beside a catalog that still held a
// relevant incident. That is exactly what happened to the live Harbor investigation.
func TestNearMissSurfacedAfterVerifyRejection(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		// verify pass over the RECALLED finding: refute it (the recalled cause is
		// "capacity shortage"; live state says otherwise).
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitVerdictsName,
			Args: `{"verdicts":[{"index":0,"verdict":"reject","confidence":0.1,"reason":"contradicts evidence: the pod fails with CreateContainerConfigError, not scheduling"}]}`}}},
		// the full investigation the loop now falls through to
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitFindingsName,
			Args: `{"confidence":0.85,"verdict":"action_suggested","root_causes":[{"summary":"IAM access key quota","confidence":0.85}]}`}}},
		// verify pass over THAT finding: accept
		{ToolCalls: []providers.ToolCall{{ID: "3", Name: submitVerdictsName,
			Args: `{"verdicts":[{"index":0,"verdict":"accept","confidence":0.85,"reason":"grounded in pod_status + kube_events"}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall:     rejectedRecall(),
		Verify:     true,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), rejectedReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	// The loop must have run: the recall fired but verify refuted it, so the
	// short-circuit must NOT have delivered a finding.
	if got == nil {
		t.Fatal("OnComplete not called")
	}
	if got.Recalled {
		t.Fatal("a verify-rejected recall must not be delivered as a recall")
	}

	// THE POINT OF THIS TEST: the loop's seed must carry the OTHER agreeing entry as
	// a framed, unverified lead — not nothing.
	// The LOOP's seed, not a verify prompt: verify requests also carry the incident
	// title, but they are the ones asking the reviewer to judge "Proposed root causes".
	var seed string
	for _, r := range model.reqs {
		if len(r.Messages) == 0 {
			continue
		}
		c := r.Messages[0].Content
		if strings.Contains(c, "HelmRelease/harbor InstallFailed") && !strings.Contains(c, "Proposed root causes to review") {
			seed = c
			break
		}
	}
	if seed == "" {
		t.Fatal("no loop request carried the incident seed (only verify prompts?)")
	}
	for _, want := range []string{
		"possibly-related past incident",
		"UNVERIFIED",
		"Harbor Registry Down due to IAM Access Key Quota Limit",
		"Delete an unused IAM access key.",
	} {
		if !strings.Contains(seed, want) {
			t.Fatalf("after a verify-rejected recall the seed must still carry the near-miss lead; missing %q.\nseed:\n%s", want, seed)
		}
	}

	// The REFUTED entry must never be re-offered as the lead — verify just disproved
	// it against live state; handing it back as "possibly related" would re-inject the
	// exact hypothesis that was ruled out.
	if strings.Contains(seed, "cluster capacity shortage") {
		t.Fatalf("the verify-REJECTED entry must be excluded from the near-miss lead; seed:\n%s", seed)
	}
}

// TestNoNearMissWhenRejectedEntryIsTheOnlyCandidate proves the exclusion is not a
// silent no-op: when the refuted entry is the ONLY structurally-agreeing candidate
// there is nothing left to surface, and the loop runs with a clean seed rather than
// being re-handed the disproven hypothesis.
func TestNoNearMissWhenRejectedEntryIsTheOnlyCandidate(t *testing.T) {
	r := &Recall{
		MinScore: 1.0, MarginGap: 0.5, SoloFloor: 1.0,
		Catalog: fakeScored{hits: []catalog.ScoredEntry{{
			Entry: catalog.Entry{
				Title:    "Harbor HelmRelease stuck InstallFailed after a cluster capacity shortage",
				Path:     "capacity.md",
				Resource: "tooling/harbor",
				Body:     "## Cause\nNo schedulable capacity.\n\n## Resolution\nWait.\n",
			},
			Score: 9.0,
		}}},
	}
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitVerdictsName,
			Args: `{"verdicts":[{"index":0,"verdict":"reject","confidence":0.1,"reason":"contradicts evidence"}]}`}}},
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitFindingsName,
			Args: `{"confidence":0.8,"root_causes":[{"summary":"fresh finding","confidence":0.8}]}`}}},
		{ToolCalls: []providers.ToolCall{{ID: "3", Name: submitVerdictsName,
			Args: `{"verdicts":[{"index":0,"verdict":"accept","confidence":0.8,"reason":"grounded"}]}`}}},
	}}
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall:     r,
		Verify:     true,
		OnComplete: func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), rejectedReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	for _, rq := range model.reqs {
		if len(rq.Messages) == 0 {
			continue
		}
		if strings.Contains(rq.Messages[0].Content, "possibly-related past incident") {
			t.Fatalf("the sole candidate was refuted by verify; it must not come back as a near-miss lead:\n%s", rq.Messages[0].Content)
		}
	}
}
