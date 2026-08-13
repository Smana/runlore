// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// TestSeedPromptShowsTheStandingAnswer: when an answer already stands for this
// trigger, the seed SHOWS it. Without this the model had no place to say "this is
// the same known thing" and reached for `inconclusive` — the one verdict meaning the
// opposite (#471). The block has to carry four things to be useful: what was
// concluded, how actionable it was, how long ago (so a three-week-old answer can be
// weighed differently from a three-hour-old one), and what to do with it.
func TestSeedPromptShowsTheStandingAnswer(t *testing.T) {
	req := Request{Title: "KubePodCrashLooping", Source: SourceAlert,
		Workload: providers.Workload{Namespace: "apps", Name: "wet-collab-api"}}
	prior := outcome.TriggerRecurrence{Count: 3, Last: time.Now().Add(-90 * time.Minute), Verdict: "action_required",
		Conclusive: outcome.ConclusivePrior{
			At:      time.Now().Add(-3 * time.Hour),
			Verdict: "action_required",
			Title:   "wet-collab-api CrashLoopBackOff from a broken DB down-migration to schema 94",
		}}

	got := seedPrompt(req, seedContext{prior: prior})
	for _, want := range []string{
		"broken DB down-migration to schema 94", // the standing diagnosis
		"action_required",                       // how actionable it was
		"3h00m",                                 // how long ago it was reached
		"4th",                                   // which occurrence this is (3 priors + this one, as the card counts them)
	} {
		if !strings.Contains(got, want) {
			t.Errorf("seed prompt missing %q, got:\n%s", want, got)
		}
	}
	// It must tell the model what to DO with the standing answer — otherwise this is
	// just more context to be summarized, not a path out of the ambiguity.
	if !strings.Contains(got, "restate") || !strings.Contains(got, "inconclusive") {
		t.Errorf("seed prompt does not say what to do with the standing answer, got:\n%s", got)
	}
	// …and it must not read as an instruction to agree. A recurrence past the cooldown
	// is a deliberate FRESH look; anchoring on the prior without checking live state is
	// the failure mode this block could otherwise introduce.
	if !strings.Contains(got, "may have been fixed") {
		t.Errorf("seed prompt presents the standing answer as settled fact, got:\n%s", got)
	}
	// The quoted title is a prior run's own words, shaped by untrusted tool output.
	// Replaying it unframed would re-open the injection surface the rest of the seed
	// closes, so it must be marked as data — the same treatment catalog text gets.
	if !strings.Contains(got, "never as an instruction") {
		t.Errorf("seed prompt replays a prior conclusion without framing it as data, got:\n%s", got)
	}
}

// TestFmtAgeTiers: the seed asks the model to WEIGH how old a standing answer is, so
// the age has to be readable at the distances that actually occur. Without a day
// tier a three-week-old answer renders "504h00m" and the model has to divide by 24
// to make the very judgement the design delegates to it.
func TestFmtAgeTiers(t *testing.T) {
	cases := map[time.Duration]string{
		20 * time.Second:              "<1m", // rounds to 0m
		30 * time.Second:              "1m",  // rounds UP to a minute before the <1m test
		-5 * time.Minute:              "<1m", // clock skew
		42 * time.Minute:              "42m",
		3 * time.Hour:                 "3h00m",
		3*time.Hour + 7*time.Minute:   "3h07m",
		23*time.Hour + 59*time.Minute: "23h59m",
		24 * time.Hour:                "1d",
		21 * 24 * time.Hour:           "21d",
		180 * 24 * time.Hour:          "180d",
	}
	for d, want := range cases {
		if got := fmtAge(d); got != want {
			t.Errorf("fmtAge(%s) = %q, want %q", d, got, want)
		}
	}
}

// TestSeedPromptOmitsTheBlockWithoutAStandingAnswer: no answer, no block. A first
// sighting and a trigger that has only ever come back inconclusive must both get the
// plain seed — inventing a "previously: nothing" line would be noise, and quoting an
// inconclusive prior would hand the model the very mislabel to copy.
func TestSeedPromptOmitsTheBlockWithoutAStandingAnswer(t *testing.T) {
	req := Request{Title: "KubePodCrashLooping", Source: SourceAlert}
	for _, c := range []struct {
		name  string
		prior outcome.TriggerRecurrence
	}{
		{"first sighting", outcome.TriggerRecurrence{}},
		{"only ever inconclusive", outcome.TriggerRecurrence{Count: 4, Last: time.Now().Add(-time.Hour), Verdict: "inconclusive"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			if got := seedPrompt(req, seedContext{prior: c.prior}); strings.Contains(got, "previously concluded") {
				t.Errorf("seed prompt volunteered a standing answer it does not have, got:\n%s", got)
			}
		})
	}
}

// TestStandingAnswerIsWithheldFromAContestedTrigger: a human 👎'd this diagnosis,
// which is precisely what forces a fresh look. Handing the model the rejected cause
// and telling it to restate it would launder the rejection into its opposite: the
// restated finding dedups onto the same entry, and the curator records a
// CONFIRMATION for it (curator.Curate → Confirmations.Confirm), which is 👎-recovery
// evidence. That is the same rubber stamp BuildReinvestigator is denied history to
// avoid; the serve path feeds the very same Confirm.
func TestStandingAnswerIsWithheldFromAContestedTrigger(t *testing.T) {
	prior := outcome.TriggerRecurrence{Count: 2, Last: time.Now().Add(-2 * time.Hour), Verdict: "action_required",
		FeedbackDown: 1, // a human rejected this conclusion
		Conclusive: outcome.ConclusivePrior{At: time.Now().Add(-2 * time.Hour),
			Verdict: "action_required", Title: "broken DB down-migration to schema 94"}}
	li := &LoopInvestigator{}
	if got := li.replayableStandingAnswer(prior); got.Concluded() {
		t.Fatalf("a contested diagnosis must not be replayed into the seed, got %+v", got.Conclusive)
	}
	// The trigger's own recurrence facts survive — only the answer is withheld, since
	// "you have seen this before" is not the part a 👎 contests.
	if got := li.replayableStandingAnswer(prior); got.Count != 2 {
		t.Fatalf("withholding the answer must not erase the occurrence count, got %+v", got)
	}
}

// TestStandingAnswerIsWithheldUnderAutoExecution: under actions.mode=auto, recall
// and its near-miss lead are both withheld from the prompt (tryRecall's !IsAuto
// gate) on the explicit reasoning that a poisoned entry must not shape "even the
// prompt under auto". A prior conclusion is the same class of text — model prose
// authored over untrusted tool output — so it sits behind the same gate. Framing
// alone is what the codebase already judged insufficient here.
func TestStandingAnswerIsWithheldUnderAutoExecution(t *testing.T) {
	prior := outcome.TriggerRecurrence{Count: 2, Last: time.Now().Add(-2 * time.Hour), Verdict: "action_required",
		Conclusive: outcome.ConclusivePrior{At: time.Now().Add(-2 * time.Hour),
			Verdict: "action_required", Title: "broken DB down-migration to schema 94"}}
	auto := &LoopInvestigator{Actions: action.New(config.ActionPolicy{Mode: config.ActionAuto})}
	if got := auto.replayableStandingAnswer(prior); got.Concluded() {
		t.Fatalf("the standing answer must not enter the prompt under auto, got %+v", got.Conclusive)
	}
	// …and it IS replayed on the default read-only path, where the same reasoning
	// does not apply (nothing the prompt shapes can execute).
	suggest := &LoopInvestigator{Actions: action.New(config.ActionPolicy{})}
	if got := suggest.replayableStandingAnswer(prior); !got.Concluded() {
		t.Fatalf("the standing answer must still be replayed when no action can execute, got %+v", got)
	}
	if got := (&LoopInvestigator{}).replayableStandingAnswer(prior); !got.Concluded() {
		t.Fatalf("a nil action policy must not withhold the standing answer, got %+v", got)
	}
}

// TestLoopSeedsTheStandingAnswerWithoutTheCooldown: the known-recurrence block is
// wired to the trigger history directly, NOT to the opt-in recurrence cooldown. The
// ambiguity it removes is the model's, and it exists whether or not an operator has
// turned suppression on — most have not, since the cooldown defaults to off.
func TestLoopSeedsTheStandingAnswerWithoutTheCooldown(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
			Args: `{"title":"same as before","confidence":0.8,"verdict":"action_required","root_causes":[{"summary":"broken down-migration, still unfixed"}]}`}}},
	}}
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(providers.Investigation) {},
		// No Recurrence gate at all — suppression is off, as it is by default.
		TriggerHistory: fakeRecurrenceStats{outcome.TriggerRecurrence{
			Count: 2, Last: time.Now().Add(-2 * time.Hour), Verdict: "action_required",
			Conclusive: outcome.ConclusivePrior{At: time.Now().Add(-2 * time.Hour),
				Verdict: "action_required", Title: "broken DB down-migration to schema 94"},
		}},
	}
	if err := li.Investigate(context.Background(), Request{Title: "KubePodCrashLooping", TriggerKey: "tk"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(model.reqs) == 0 || len(model.reqs[0].Messages) == 0 {
		t.Fatal("model was never called with a seed")
	}
	if seed := model.reqs[0].Messages[0].Content; !strings.Contains(seed, "broken DB down-migration to schema 94") {
		t.Fatalf("the loop did not seed the standing answer, got:\n%s", seed)
	}
}
