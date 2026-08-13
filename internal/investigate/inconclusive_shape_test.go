// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestUnaccountedInconclusive pins the shape test: "I could not determine the
// cause" is only honest when something says what blocked it. A root cause, an open
// question for a human, or a data gap all account for the verdict; none of the
// three means the payload contradicts itself, and the card ships empty (#471).
func TestUnaccountedInconclusive(t *testing.T) {
	inconclusive := func(mut func(*providers.Investigation)) providers.Investigation {
		inv := providers.Investigation{Title: "t", Verdict: providers.VerdictInconclusive}
		mut(&inv)
		return inv
	}
	cases := []struct {
		name string
		inv  providers.Investigation
		want bool
	}{
		{"inconclusive with nothing to show for it", inconclusive(func(*providers.Investigation) {}), true},
		{"a data gap accounts for it", inconclusive(func(i *providers.Investigation) {
			i.DataGaps = []string{"pod_logs: RBAC denied"}
		}), false},
		{"an open question accounts for it", inconclusive(func(i *providers.Investigation) {
			i.Unresolved = []string{"was the migration reverted by hand?"}
		}), false},
		{"a named cause accounts for it", inconclusive(func(i *providers.Investigation) {
			i.RootCauses = []providers.Hypothesis{{Summary: "broken down-migration"}}
		}), false},
		{"a conclusive verdict is never flagged, however thin",
			providers.Investigation{Title: "t", Verdict: providers.VerdictActionRequired}, false},
		{"an omitted verdict is a parse concern, not this one",
			providers.Investigation{Title: "t"}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := unaccountedInconclusive(c.inv); got != c.want {
				t.Fatalf("unaccountedInconclusive = %v, want %v", got, c.want)
			}
		})
	}
}

// TestUnaccountedInconclusiveIsLoud: the mislabel that started #471 must not pass
// through the loop silently. A submitted finding claiming inconclusive with no
// cause, no question and no gap produces a card with no Why and no next steps — the
// warning is the only thing that says so at the source, where the payload is still
// attributable to the model call that produced it.
func TestUnaccountedInconclusiveIsLoud(t *testing.T) {
	var buf bytes.Buffer
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
			Args: `{"title":"pre-existing, not a new incident","confidence":0,"verdict":"inconclusive","root_causes":[]}`}}},
	}}
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})),
		OnComplete: func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "KubePodCrashLooping", TriggerKey: "tk"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got := buf.String(); !strings.Contains(got, "inconclusive") || !strings.Contains(got, "tk") {
		t.Fatalf("an unaccounted inconclusive submission logged no warning naming the trigger; got:\n%s", got)
	}

	// …and a well-shaped inconclusive submission stays quiet: a warning on every
	// honest "I could not determine, here is what blocked me" would train the
	// operator to ignore the one that matters.
	buf.Reset()
	li.Model = &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
			Args: `{"title":"t","verdict":"inconclusive","root_causes":[],"data_gaps":["pod_logs: RBAC denied"]}`}}},
	}}
	if err := li.Investigate(context.Background(), Request{Title: "KubePodCrashLooping", TriggerKey: "tk"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got := buf.String(); strings.Contains(got, "inconclusive") {
		t.Fatalf("an accounted-for inconclusive submission must not warn; got:\n%s", got)
	}
}
