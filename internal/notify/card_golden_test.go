// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// The Slack incident card is the one thing this project ships a PICTURE of:
// README.md and the website home page embed two captures of it. Pictures cannot
// be diffed, so nothing stopped a card refactor from turning them into
// advertisements for a layout that no longer exists — which is exactly what
// happened on 2026-08-02 (see hack/check-screenshots-fresh.sh).
//
// This golden is the machine-readable half of that guard. It records the Block Kit
// payload the renderer actually produces, so:
//
//   - any change to what the card LOOKS like fails this test until the golden is
//     regenerated, and regenerating it changes the file's digest;
//   - hack/check-screenshots-fresh.sh compares that digest against the card the
//     shipped screenshots depict, and fails when they have diverged.
//
// The two halves are load-bearing together and neither works alone: without this
// test the golden could be hand-edited to match any digest; without the digest
// check a regenerated golden would silently accept stale screenshots.
//
// It is deliberately NOT a readability assertion. Everything about whether the
// card says the right thing is pinned by the tests in slack_test.go; this file
// only answers "is it still the same picture?".
const cardGoldenPath = "testdata/incident-card.golden.json"

var updateCardGolden = flag.Bool("update-card-golden", false,
	"rewrite "+cardGoldenPath+" from the current renderer")

// cardGoldenFixtures are the investigations the golden renders. Each one exists to
// hold open a distinct arm of summaryBlocks/detailBlocks, because a fixture set
// that misses an arm is the dangerous failure: the card changes, the digest does
// not, and the guard passes while the screenshots go stale.
//
// The arms currently covered, one fixture each:
//
//	action_required        the full alert-driven card the README's first capture shows —
//	                       verdict + restated title, stated confidence, top cause with
//	                       overflow evidence, next steps with overflow, metadata fields,
//	                       verified/entry-link/usage footer, approval buttons, feedback
//	                       buttons, and a detail section (extra hypotheses, open
//	                       questions, data gaps, ruled out)
//	instant_recall         the README's second capture — Prior + Recalled: the ⚡ banner,
//	                       known cause / validated resolution labels, resolve rate, the
//	                       recall confidence note, no verdict
//	seen_before            Prior WITHOUT Recalled: the 📚 counter branch and its
//	                       prior-cause labels, which the recall branch replaces
//	matched_runbook        MatchedKnowledge with Prior nil — the block that is
//	                       suppressed whenever either fixture above renders
//	no_confidence_stated   the variant #475 added: a finding that states no confidence
//	                       at all, which bolds the whole phrase instead of a level
//	bare_recall            a recall with no Prior and no alert name: the header falls
//	                       back to the title and the whole card collapses to its minimum
//	curate_failed          #506's card: a finding well above the curation bar whose KB
//	                       write FAILED, so the footer carries the ⚠️ reason next to the
//	                       prior entry's link — the arm that distinguishes a broken
//	                       learning loop from a deliberately skipped write
//
// Times are fixed constants: slackDate emits a <!date^…> token built from a Unix
// timestamp, so a time.Now() anywhere here would rewrite the golden on every run.
func cardGoldenFixtures() []struct {
	Name string
	Inv  providers.Investigation
} {
	started := time.Date(2026, 7, 3, 10, 0, 0, 0, time.UTC)
	lastSeen := time.Date(2026, 6, 1, 9, 0, 0, 0, time.UTC)

	return []struct {
		Name string
		Inv  providers.Investigation
	}{
		{"action_required", providers.Investigation{
			Title:       "harbor-db crash-looping after the chart 1.15 migration",
			AlertName:   "HarborDown",
			Verdict:     providers.VerdictActionRequired,
			Confidence:  0.92,
			Severity:    "critical",
			Environment: "prod",
			Cluster:     "eu-west-1",
			Tenant:      "platform",
			Resource:    providers.Workload{Kind: "HelmRelease", Namespace: "tooling", Name: "harbor"},
			StartedAt:   started,
			RootCauses: []providers.Hypothesis{
				{
					Summary:    "chart 1.15 enabled DB migrations; harbor-db is in CrashLoopBackOff",
					Confidence: 0.92,
					Evidence: []string{
						"pg_up=0 since 10:02",
						"migration lock timeout in harbor-db logs",
						"HelmRelease revision moved 1.14.2 -> 1.15.0",
						"registry pod readiness probe failing on /api/v2.0/ping",
					},
					ChangeRef:       "flux-system/apps: abc123..def456",
					SuggestedAction: "flux rollback hr/harbor to 1.14.2",
					Reversible:      true,
				},
				{
					Summary:    "the shared postgres volume filled up",
					Confidence: 0.11,
					Evidence:   []string{"no disk metrics available for harbor-db"},
				},
			},
			Actions: []providers.Action{
				{Description: "suspend the harbor HelmRelease", Op: "suspend", Reversible: true, ApprovalID: "apr-7f3"},
			},
			Unresolved:     []string{"why the migration lock never released"},
			DataGaps:       []string{"harbor-db disk metrics unavailable (scrape target down)"},
			RuledOut:       []string{"network partition disproven by pg reachable from the api pod"},
			Verified:       true,
			CuratedURL:     "https://github.com/o/kb/pull/312",
			TriggerKey:     "HarborDown/tooling/harbor",
			Occurrences:    3,
			LastOccurrence: lastSeen,
			Usage: providers.UsageTotals{
				ModelCalls: 7, InputTokens: 121758, OutputTokens: 9596,
			},
		}},

		{"instant_recall", providers.Investigation{
			Title:      "harbor-db crash-looping after the chart 1.15 migration",
			AlertName:  "HarborDown",
			Confidence: 0.78,
			Recalled:   true,
			Resource:   providers.Workload{Kind: "HelmRelease", Namespace: "tooling", Name: "harbor"},
			Cluster:    "eu-west-1",
			Tenant:     "platform",
			StartedAt:  started,
			RootCauses: []providers.Hypothesis{{
				Summary:    "chart 1.15 enabled DB migrations; harbor-db is in CrashLoopBackOff",
				Confidence: 0.94,
			}},
			Prior: &providers.PriorKnowledge{
				Cause:      "chart 1.15 enables DB migrations that need a manual lock release",
				Resolution: "roll back to 1.14.2, clear the migration lock, then re-apply",
				EntryPath:  "entries/harbor-db-migration-lock.md",
				Recalls:    4,
				Resolved:   3,
			},
			RecalledEntry:  "entries/harbor-db-migration-lock.md",
			Verified:       true,
			PrevCuratedURL: "https://github.com/o/kb/pull/298",
			TriggerKey:     "HarborDown/tooling/harbor",
			Occurrences:    5,
			LastOccurrence: lastSeen,
			Usage: providers.UsageTotals{
				ModelCalls: 2, InputTokens: 4764, OutputTokens: 3525,
			},
		}},

		{"seen_before", providers.Investigation{
			Title:      "harbor-db crash-looping again after the chart bump",
			AlertName:  "HarborDown",
			Verdict:    providers.VerdictActionSuggested,
			Confidence: 0.64,
			Resource:   providers.Workload{Kind: "HelmRelease", Namespace: "tooling", Name: "harbor"},
			Cluster:    "eu-west-1",
			StartedAt:  started,
			RootCauses: []providers.Hypothesis{{
				Summary:    "the same migration lock, not released by the previous rollback",
				Confidence: 0.64,
				Evidence:   []string{"lock row still present in the migrations table"},
			}},
			Prior: &providers.PriorKnowledge{
				Cause:      "chart 1.15 enables DB migrations that need a manual lock release",
				Resolution: "roll back to 1.14.2, clear the migration lock, then re-apply",
				EntryPath:  "entries/harbor-db-migration-lock.md",
				Recalls:    4,
				Resolved:   3,
			},
			Occurrences:    2,
			LastOccurrence: lastSeen,
			PrevCuratedURL: "https://github.com/o/kb/pull/298",
			TriggerKey:     "HarborDown/tooling/harbor",
		}},

		{"matched_runbook", providers.Investigation{
			Title:      "image pulls failing across the gallery namespace",
			AlertName:  "ImageGalleryUnavailable",
			Verdict:    providers.VerdictActionRequired,
			Confidence: 0.71,
			Resource:   providers.Workload{Kind: "Deployment", Namespace: "apps", Name: "xplane-image-gallery"},
			Tenant:     "apps",
			StartedAt:  started,
			RootCauses: []providers.Hypothesis{{
				Summary:    "a manual AWS Secrets Manager DeleteSecret removed the registry credentials",
				Confidence: 0.71,
				Evidence:   []string{"CloudTrail DeleteSecret at 09:58 by an IAM user"},
				ChangeRef:  "aws: DeleteSecret registry/pull-token",
			}},
			MatchedKnowledge: &providers.MatchedEntry{
				Path:  "entries/registry-credentials-deleted.md",
				Title: "Registry pull credentials deleted out of band",
				URL:   "https://github.com/o/kb/blob/main/entries/registry-credentials-deleted.md",
				Score: 6.4,
			},
			Fingerprint: "fp-9a1",
			Usage: providers.UsageTotals{
				ModelCalls: 4, InputTokens: 38210, OutputTokens: 2914,
			},
		}},

		{"no_confidence_stated", providers.Investigation{
			Title:     "could not determine why the gallery is unavailable",
			AlertName: "ImageGalleryUnavailable",
			Verdict:   providers.VerdictInconclusive,
			Resource:  providers.Workload{Kind: "Deployment", Namespace: "apps", Name: "xplane-image-gallery"},
			StartedAt: started,
			RootCauses: []providers.Hypothesis{{
				Summary: "no signal separated a registry outage from a credentials change",
			}},
			DataGaps:    []string{"CloudTrail lookup returned no events in the window"},
			Fingerprint: "fp-9a2",
		}},

		{"bare_recall", providers.Investigation{
			Title:      "harbor-db migration lock",
			Confidence: 0.55,
			Recalled:   true,
			RootCauses: []providers.Hypothesis{{Summary: "the migration lock again"}},
		}},

		// The live #506 card: 85% confidence, action_suggested — comfortably above
		// curate.min_confidence and not in skip_verdicts — whose KB PR got a 403. It
		// carries PrevCuratedURL so the footer holds BOTH arms open at once: the prior
		// entry's link, which on its own would read as this run's, and the ⚠️ reason
		// that says it is not.
		{"curate_failed", providers.Investigation{
			Title:      "argocd-repo-server OOMKilled after the 2.11 bump",
			AlertName:  "ArgoCDRepoServerDown",
			Verdict:    providers.VerdictActionSuggested,
			Confidence: 0.85,
			Severity:   "warning",
			Cluster:    "eu-west-1",
			Resource:   providers.Workload{Kind: "Deployment", Namespace: "argocd", Name: "argocd-repo-server"},
			StartedAt:  started,
			RootCauses: []providers.Hypothesis{{
				Summary:         "2.11 raised the manifest cache footprint past the 512Mi limit",
				Confidence:      0.85,
				Evidence:        []string{"container exit code 137 at 14:17"},
				SuggestedAction: "raise the repo-server memory limit to 1Gi",
				Reversible:      true,
			}},
			CurateError:    `open PR: github GET /repos/o/kb/git/ref/heads/main: status 403: {"message":"Resource not accessible by integration"}`,
			PrevCuratedURL: "https://github.com/o/kb/pull/271",
			TriggerKey:     "ArgoCDRepoServerDown/argocd/argocd-repo-server",
			Occurrences:    2,
			LastOccurrence: lastSeen,
			Usage: providers.UsageTotals{
				ModelCalls: 5, InputTokens: 51204, OutputTokens: 3117,
			},
		}},
	}
}

// renderCardGolden renders every fixture through slackMessageWith — the exact
// entry point the bot path posts (summary + detail + feedback buttons) — and
// encodes the result with sorted map keys, so the bytes are a pure function of
// the renderer.
//
// HTML escaping is off: Slack's own mrkdwn vocabulary is built from <, > and &
// (<!date^…>, <url|label>, &gt; escapes), and < everywhere would make the
// golden unreadable in review — the one place a human is meant to SEE the card
// change in a diff.
func renderCardGolden(t *testing.T) []byte {
	t.Helper()
	type card struct {
		Fixture string         `json:"fixture"`
		Card    map[string]any `json:"card"`
	}
	cards := make([]card, 0, len(cardGoldenFixtures()))
	for _, f := range cardGoldenFixtures() {
		cards = append(cards, card{Fixture: f.Name, Card: slackMessageWith(f.Inv, true)})
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(cards); err != nil {
		t.Fatalf("marshal golden: %v", err)
	}
	return buf.Bytes()
}

// TestSlackCardMatchesTheShippedGolden is the falsifiable half of the screenshot
// guard: it makes the golden track the renderer, so the golden's digest is a
// trustworthy stand-in for "what the card looks like".
//
// Regenerate with:
//
//	go test ./internal/notify -run TestSlackCardMatchesTheShippedGolden -update-card-golden
//
// and expect hack/check-screenshots-fresh.sh to fail afterwards — that is the
// guard doing its job, not collateral damage. Retake the two captures, or
// acknowledge the drift in that script with a reason.
func TestSlackCardMatchesTheShippedGolden(t *testing.T) {
	got := renderCardGolden(t)

	if *updateCardGolden {
		if err := os.WriteFile(cardGoldenPath, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("wrote %s — hack/check-screenshots-fresh.sh now needs the new digest", cardGoldenPath)
		return
	}

	want, err := os.ReadFile(cardGoldenPath)
	if err != nil {
		t.Fatalf("read golden: %v (regenerate with -update-card-golden)", err)
	}
	if string(got) != string(want) {
		t.Fatalf("the Slack incident card no longer matches %s.\n\n"+
			"If the change is intended, the two shipped screenshots (assets/slack-notification.png,\n"+
			"assets/recall-notification.png) now show a card RunLore does not draw. Regenerate with\n"+
			"  go test ./internal/notify -run %s -update-card-golden\n"+
			"then retake the captures, or acknowledge the drift in hack/check-screenshots-fresh.sh.\n\n"+
			"got:\n%s\n\nwant:\n%s", cardGoldenPath, t.Name(), got, want)
	}
}

// TestCardGoldenCoversEveryRenderedBranch keeps the fixture set honest. The golden
// is only as good as its coverage: an arm no fixture reaches can be rewritten
// without moving the digest, which is a silent pass on exactly the drift the guard
// exists to catch.
//
// Each string below is scaffolding the renderer emits for one branch — literal
// text no fixture could produce as data. It fails loudly when a fixture stops
// reaching its arm; it cannot detect a branch added later, which is why the
// fixture doc comment names what each one holds open.
func TestCardGoldenCoversEveryRenderedBranch(t *testing.T) {
	rendered := string(renderCardGolden(t))
	for _, want := range []string{
		"Action required",                      // verdictBadge, conclusive
		"Inconclusive",                         // verdictBadge, inconclusive arm
		"confidence not stated",                // the #475 unstated variant
		"confidence*",                          // the ordinary stated variant (level bolded, pct outside)
		"Instant recall",                       // Prior + Recalled
		"Known cause",                          // the recall labels
		"Validated resolution",                 //
		"Seen before",                          // Prior without Recalled
		"Prior cause",                          // the recurrence labels
		"resolve rate",                         // PriorKnowledge.Recalls
		"Matches known runbook",                // MatchedKnowledge with Prior nil
		"Why:",                                 // top root cause
		"more hypothesis below",                // the deeper-hypotheses pointer
		"Suggested next steps",                 // nextSteps
		"Full analysis",                        // detailBlocks
		"What changed:",                        // Hypothesis.ChangeRef
		"Open questions",                       // Unresolved
		"Data gaps",                            // DataGaps
		"Ruled out",                            // RuledOut
		"verified",                             // the footer's verify marker
		"could not save to the knowledge base", // the failed-curate footer arm (#506)
		"model calls",                          // usageFooter
		"Proposed action",                      // an action awaiting approval
		"Approve",                              //
		"👍 Accurate",                           // feedbackBlocks
		"…1 more",                              // evidence / next-step overflow
		"<!date^",                              // slackDate, which is why the fixtures fix their times
	} {
		if !contains(rendered, want) {
			t.Errorf("no fixture reaches the branch that renders %q — the golden's digest "+
				"would not move if that branch changed", want)
		}
	}
}
