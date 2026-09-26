// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestShippedToolOutputsCarryNoEditorial pins that no recorded tool output in the
// shipped corpus contains a comment line. A `# ...` line inside a `tools:` block
// scalar is not a YAML comment — it is text the model is handed as evidence — and
// the held-out case shipped with five of them stating its own verdict ("pinned at
// its autoscaler's upper bound and saturated", "the neighbour that DID change is
// healthy and unrelated"). A model that copies them passes must_contain without
// reasoning, so the one case the prompt is not tuned against measured quoting.
//
// Real tool outputs never carry comment lines: a PromQL result, a pod table, a log
// line and a Flux diff have no `#` rows. Case commentary belongs above the key,
// outside the scalar, where YAML drops it.
func TestShippedToolOutputsCarryNoEditorial(t *testing.T) {
	cases, err := Load(filepath.Join("..", "..", "examples", "eval"))
	if err != nil {
		t.Fatalf("Load examples/eval: %v", err)
	}
	for _, c := range cases {
		for tool, out := range c.Tools {
			for i, line := range strings.Split(out, "\n") {
				if strings.HasPrefix(strings.TrimSpace(line), "#") {
					t.Errorf("%s: tools.%s line %d is a comment INSIDE the block scalar, so the model reads it as evidence: %q",
						c.Name, tool, i+1, strings.TrimSpace(line))
				}
			}
		}
	}
}

// TestRunOneSeedsTheAlertTitleNotTheSlug: the replay runner must seed the loop with
// the title a real trigger would carry, not the case's file slug. Case.AlertTitle's
// doc already promised that ("the loop uses DisplayName as the Request title") and
// `lore demo` honours it, but runOne passed c.Name — so the held-out case opened its
// prompt with "Incident: hpa-ceiling-saturation", three words that name the answer
// before a single tool is called.
func TestRunOneSeedsTheAlertTitleNotTheSlug(t *testing.T) {
	c := Case{
		Name:       "hpa-ceiling-saturation",
		AlertTitle: "HighRequestLatency",
		Prompt:     "pricing-api p99 latency above SLO",
		Tools:      map[string]string{"pod_status": "pricing-api-abc  Running  ready=1/1"},
		Expected:   Expected{MustContain: []string{"latency"}},
	}
	model := &recordingModel{resp: []providers.CompletionResponse{
		findings("latency from saturation"),
		verdict("keep"),
	}}
	(&Runner{Model: model, Log: discardLog()}).runOne(context.Background(), c)
	if len(model.reqs) == 0 {
		t.Fatal("the model was never called")
	}
	seed := firstUserMessage(model.reqs[0])
	if !strings.Contains(seed, c.AlertTitle) {
		t.Errorf("the seed prompt does not carry the alert title %q:\n%s", c.AlertTitle, seed)
	}
	if strings.Contains(seed, c.Name) {
		t.Errorf("the seed prompt leaks the case slug %q, which names the answer:\n%s", c.Name, seed)
	}
}

// firstUserMessage returns the first user-role message of a completion request: the
// seed prompt on the loop's opening call.
func firstUserMessage(req providers.CompletionRequest) string {
	for _, m := range req.Messages {
		if m.Role == "user" {
			return m.Content
		}
	}
	return ""
}

// TestNotInClaimAcceptsAnySpelling: a must_contain term may list alternatives
// separated by "|", and the claim satisfies the term when it contains ANY of them.
// The held-out case's one mechanism term was "autoscal" — which "HPA", the
// abbreviation in the case's own filename and the most natural correct spelling,
// does not contain — so a fully right claim scored as a miss in the one case that
// cannot be re-tuned after its first result.
func TestNotInClaimAcceptsAnySpelling(t *testing.T) {
	const term = "autoscal|hpa"
	for claim, wantMissing := range map[string]bool{
		"pricing-api's hpa is at maxreplicas=4":           false,
		"pinned at its autoscaling ceiling":               false,
		"the horizontalpodautoscaler cannot scale past 4": false,
		"the promo-banner bump degraded it":               true,
	} {
		got := notInClaim(claim, []string{term})
		if missing := len(got) > 0; missing != wantMissing {
			t.Errorf("notInClaim(%q, [%q]) = %v, want missing=%v", claim, term, got, wantMissing)
		}
	}
	// A miss reports the whole term, so the scorecard shows every spelling tried.
	if got := notInClaim("nothing", []string{term}); !slices.Equal(got, []string{term}) {
		t.Errorf("a miss must report the full term, got %v", got)
	}
}
