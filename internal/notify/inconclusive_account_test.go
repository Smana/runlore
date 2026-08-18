// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestInconclusiveSummaryCardAccountsForItself pins the card contract behind the
// live 2026-08-18 mislabel: an `inconclusive` verdict must never reach the channel
// as a verdict badge and nothing else. The summary either shows a Why (the
// investigation did name a cause, whatever the verdict label says) or it states
// what blocked the run — and when the payload says neither, the card says THAT,
// so a reader can tell an incomplete run from a finding.
func TestInconclusiveSummaryCardAccountsForItself(t *testing.T) {
	const inconclusive = providers.VerdictInconclusive
	cases := []struct {
		name   string
		inv    providers.Investigation
		want   []string
		absent []string
	}{
		{
			name: "the budget hard-kill's ceiling reaches the card",
			inv: providers.Investigation{
				Title: "KubeNodeNotReady", AlertName: "KubeNodeNotReady", Verdict: inconclusive,
				Unresolved: []string{"investigation stopped: cumulative token budget (investigation.max_tokens_per_investigation) exceeded after nudge (model did not submit findings in time)"},
			},
			want:   []string{"Why this is inconclusive", "cumulative token budget"},
			absent: []string{"No account given"},
		},
		{
			name: "the per-investigation deadline reaches the card",
			inv: providers.Investigation{
				Title: "KubeNodeNotReady", AlertName: "KubeNodeNotReady", Verdict: inconclusive,
				Unresolved: []string{"investigation stopped: per-investigation deadline exceeded before findings were submitted (e.g. a hung git clone/diff or a slow model)"},
			},
			want:   []string{"Why this is inconclusive", "per-investigation deadline exceeded"},
			absent: []string{"No account given"},
		},
		{
			name: "a model refusal reaches the card",
			inv: providers.Investigation{
				Title: "KubeNodeNotReady", AlertName: "KubeNodeNotReady", Verdict: inconclusive,
				Unresolved: []string{"investigation stopped: the model declined to respond (safety-filtered or refused); no root cause was produced"},
			},
			want:   []string{"Why this is inconclusive", "the model declined to respond"},
			absent: []string{"No account given"},
		},
		{
			name: "a data gap accounts for it just as well as an open question",
			inv: providers.Investigation{
				Title: "KubeNodeNotReady", AlertName: "KubeNodeNotReady", Verdict: inconclusive,
				DataGaps: []string{"investigation exhausted its 12-step budget without concluding"},
			},
			want:   []string{"Why this is inconclusive", "12-step budget"},
			absent: []string{"No account given"},
		},
		{
			name: "a mislabelled inconclusive still renders its conclusion and evidence",
			inv: providers.Investigation{
				Title:      "KubeNodeUnreachable on ip-10-20-24-144: self-healed Karpenter node churn",
				AlertName:  "KubeNodeUnreachable",
				Verdict:    inconclusive,
				Confidence: 0.75,
				RootCauses: []providers.Hypothesis{{
					Summary:         "Karpenter drained and replaced the node; the kubelet never came back on the old one",
					Confidence:      0.75,
					Evidence:        []string{"karpenter logs: disrupting nodeclaim via expiration"},
					SuggestedAction: "nothing to do — the replacement node registered 90s later",
				}},
			},
			want:   []string{"*Why:*", "Karpenter drained and replaced the node", "Suggested next steps"},
			absent: []string{"No account given", "Why this is inconclusive"},
		},
		{
			// The verify pass is the path that reaches this shape: applyVerdicts moves
			// every rejected hypothesis into RuledOut, empties RootCauses and forces
			// `inconclusive`, without writing Unresolved or DataGaps. That run DID
			// account for itself, so calling it an incomplete run is a lie the reader
			// can check — the refutations are in the thread reply.
			name: "a verify pass that refuted everything is accounted for, not unaccounted",
			inv: providers.Investigation{
				Title: "KubeNodeNotReady", AlertName: "KubeNodeNotReady", Verdict: inconclusive,
				RuledOut: []string{
					"the kubelet OOMed — disproven: no oom_kill in the node's kernel log",
					"a bad CNI rollout — disproven: the DaemonSet generation predates the alert by 6d",
				},
			},
			want:   []string{"Why this is inconclusive", "no oom_kill in the node's kernel log"},
			absent: []string{"No account given"},
		},
		{
			// …but open questions and data gaps still outrank it: RuledOut is what the
			// cause is NOT, and it earns summary fold space only when nothing answers
			// "why don't you know?".
			name: "an open question outranks the ruled-out list for the summary slot",
			inv: providers.Investigation{
				Title: "KubeNodeNotReady", AlertName: "KubeNodeNotReady", Verdict: inconclusive,
				Unresolved: []string{"why the kubelet never re-registered"},
				RuledOut:   []string{"the kubelet OOMed — disproven: no oom_kill in the kernel log"},
			},
			want:   []string{"Why this is inconclusive", "why the kubelet never re-registered"},
			absent: []string{"No account given", "disproven"},
		},
		{
			name: "the live card: a conclusion in the title, nothing behind it",
			inv: providers.Investigation{
				Title:     "KubeNodeNotReady on ip-10-20-24-144.ec2.internal (tmem175-0): transient, self-healed Karpenter node churn — no application impact",
				AlertName: "KubeNodeNotReady",
				Verdict:   inconclusive,
				Cluster:   "tmem175",
			},
			// The absent confidence must STILL not render as a red 0% (#475).
			want:   []string{"No account given", "confidence not stated"},
			absent: []string{"Why this is inconclusive", "Low confidence"},
		},
		{
			name: "a conclusive verdict is never annotated, however thin",
			inv: providers.Investigation{
				Title: "harbor recovered on its own", AlertName: "HarborDown",
				Verdict: providers.VerdictNoAction,
			},
			absent: []string{"No account given", "Why this is inconclusive"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			txt := blocksText(t, summaryBlocks(c.inv))
			for _, w := range c.want {
				if !strings.Contains(txt, w) {
					t.Errorf("summary card is missing %q\n%s", w, txt)
				}
			}
			for _, a := range c.absent {
				if strings.Contains(txt, a) {
					t.Errorf("summary card must not carry %q\n%s", a, txt)
				}
			}
		})
	}
}

// TestFormatSaysWhenNothingAccountsForInconclusive keeps the flat CLI/Matrix
// message honest about the same payload. Format already prints the open questions
// and data gaps a genuine inconclusive carries, so only the self-contradicting
// shape needs saying out loud — otherwise that message, too, is a verdict label
// with nothing behind it.
func TestFormatSaysWhenNothingAccountsForInconclusive(t *testing.T) {
	out := Format(providers.Investigation{
		Title:   "KubeNodeNotReady on ip-10-20-24-144: transient, self-healed Karpenter node churn",
		Verdict: providers.VerdictInconclusive,
	})
	if !strings.Contains(out, "No account given") {
		t.Errorf("Format must say an inconclusive verdict has nothing behind it\n%s", out)
	}
	// An accounted-for inconclusive already reads honestly; a second notice would
	// only train the reader to skip the one that matters.
	out = Format(providers.Investigation{
		Title:    "KubeNodeNotReady",
		Verdict:  providers.VerdictInconclusive,
		DataGaps: []string{"pod_logs: RBAC denied"},
	})
	if strings.Contains(out, "No account given") {
		t.Errorf("an accounted-for inconclusive must not carry the notice\n%s", out)
	}
	// The verify-pass shape: Format prints *Ruled out:* a few lines below, so the
	// notice here would deny, in the same message, the reasons that message goes on
	// to list. Verify is on by default, so this is the common path, not a corner.
	out = Format(providers.Investigation{
		Title:    "KubeNodeNotReady",
		Verdict:  providers.VerdictInconclusive,
		RuledOut: []string{"the kubelet OOMed — disproven: no oom_kill in the kernel log"},
	})
	if strings.Contains(out, "No account given") {
		t.Errorf("a run whose reviewer refuted every hypothesis is accounted for by its Ruled out list\n%s", out)
	}
	if !strings.Contains(out, "Ruled out") {
		t.Errorf("Format must still print the ruled-out list\n%s", out)
	}
}
