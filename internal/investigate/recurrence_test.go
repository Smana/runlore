package investigate

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

type fakeRecurrenceStats struct{ r outcome.TriggerRecurrence }

func (f fakeRecurrenceStats) Recurrence(string) outcome.TriggerRecurrence { return f.r }

// TestRecurrenceGateDecisions pins the full suppression matrix: suppress ONLY
// when the trigger was investigated within the cooldown, the prior verdict was
// conclusive, and no human currently contests it — every other combination
// re-investigates.
func TestRecurrenceGateDecisions(t *testing.T) {
	now := time.Unix(50000, 0)
	recent := now.Add(-5 * time.Minute)
	stale := now.Add(-2 * time.Hour)
	req := Request{Title: "t", TriggerKey: "k"}
	cases := []struct {
		name string
		gate *RecurrenceGate
		req  Request
		want bool
	}{
		{"suppresses a fresh conclusive uncontested recurrence",
			&RecurrenceGate{Outcome: fakeRecurrenceStats{outcome.TriggerRecurrence{Count: 1, Last: recent, Verdict: "no_action"}}, Cooldown: time.Hour}, req, true},
		{"action_required is conclusive too",
			&RecurrenceGate{Outcome: fakeRecurrenceStats{outcome.TriggerRecurrence{Count: 2, Last: recent, Verdict: "action_required"}}, Cooldown: time.Hour}, req, true},
		{"nil gate never suppresses", nil, req, false},
		{"cooldown 0 (off) never suppresses",
			&RecurrenceGate{Outcome: fakeRecurrenceStats{outcome.TriggerRecurrence{Count: 1, Last: recent, Verdict: "no_action"}}}, req, false},
		{"no trigger key never suppresses",
			&RecurrenceGate{Outcome: fakeRecurrenceStats{outcome.TriggerRecurrence{Count: 1, Last: recent, Verdict: "no_action"}}, Cooldown: time.Hour}, Request{Title: "t"}, false},
		{"never investigated",
			&RecurrenceGate{Outcome: fakeRecurrenceStats{}, Cooldown: time.Hour}, req, false},
		{"cooldown expired — re-investigate",
			&RecurrenceGate{Outcome: fakeRecurrenceStats{outcome.TriggerRecurrence{Count: 3, Last: stale, Verdict: "no_action"}}, Cooldown: time.Hour}, req, false},
		{"inconclusive prior — retry, there is no answer worth repeating",
			&RecurrenceGate{Outcome: fakeRecurrenceStats{outcome.TriggerRecurrence{Count: 1, Last: recent, Verdict: "inconclusive"}}, Cooldown: time.Hour}, req, false},
		{"pre-verdict prior (old events) — retry",
			&RecurrenceGate{Outcome: fakeRecurrenceStats{outcome.TriggerRecurrence{Count: 1, Last: recent}}, Cooldown: time.Hour}, req, false},
		{"a standing 👎 breaks the cooldown — the human re-arms investigation",
			&RecurrenceGate{Outcome: fakeRecurrenceStats{outcome.TriggerRecurrence{Count: 1, Last: recent, Verdict: "no_action", FeedbackDown: 1}}, Cooldown: time.Hour}, req, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if _, got := c.gate.suppress(c.req, now); got != c.want {
				t.Fatalf("suppress = %v, want %v", got, c.want)
			}
		})
	}
}

// TestInvestigateSuppressedRecurrenceSkipsModelAndDelivery: a suppressed
// recurrence must cost nothing and say nothing — no model call, no OnComplete
// (no notification, no curation, no ledger open), nil error. The previous
// notification remains THE answer until the cooldown lapses or a 👎 lands.
func TestInvestigateSuppressedRecurrenceSkipsModelAndDelivery(t *testing.T) {
	model := &blockingModel{}
	delivered := 0
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(providers.Investigation) { delivered++ },
		Recurrence: &RecurrenceGate{
			Outcome:  fakeRecurrenceStats{outcome.TriggerRecurrence{Count: 1, Last: time.Now().Add(-time.Minute), Verdict: "no_action"}},
			Cooldown: time.Hour,
		},
	}
	if err := li.Investigate(context.Background(), Request{Title: "t", TriggerKey: "k"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if model.calls != 0 {
		t.Fatalf("model called %d times during a suppressed recurrence, want 0", model.calls)
	}
	if delivered != 0 {
		t.Fatalf("OnComplete called %d times during a suppressed recurrence, want 0", delivered)
	}
}
