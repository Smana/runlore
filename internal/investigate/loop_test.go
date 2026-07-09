// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
	"github.com/Smana/runlore/internal/telemetry"
)

type fakeScored struct{ hits []catalog.ScoredEntry }

func (f fakeScored) SearchScored(string, int) ([]catalog.ScoredEntry, error) { return f.hits, nil }

func TestInstantRecallHit(t *testing.T) {
	model := &scriptModel{} // no responses scripted: a call would panic, proving the loop is skipped
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model: model,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall: &Recall{MinScore: 2.0, Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{Entry: catalog.Entry{Title: "Known incident", Description: "chart bump", Path: "known.md", Resource: "tooling/harbor"}, Score: 5.0}}}},
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "HarborProbeFailure", Fingerprint: "fp-recall", TriggerKey: "tk-recall", Workload: providers.Workload{Namespace: "tooling", Name: "harbor"}}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if model.i != 0 {
		t.Fatalf("model was called %d times; a recall hit must skip the loop", model.i)
	}
	if got == nil || len(got.RootCauses) != 1 || !strings.Contains(got.RootCauses[0].Summary, "Known incident") {
		t.Fatalf("unexpected recalled investigation: %+v", got)
	}
	if !got.Recalled {
		t.Fatal("a recalled investigation must be flagged Recalled so the curator skips it")
	}
	if got.Fingerprint != "fp-recall" {
		t.Fatalf("recall path must carry the alert fingerprint for outcome attribution, got %q", got.Fingerprint)
	}
	if got.TriggerKey != "tk-recall" {
		t.Fatalf("recall path must propagate the trigger key for dedup, got %q", got.TriggerKey)
	}
}

func TestInstantRecallBelowThreshold(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.5,"root_causes":[{"summary":"freshly investigated"}]}`}}},
	}}
	li := &LoopInvestigator{
		Model: model,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall: &Recall{MinScore: 2.0, Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{Entry: catalog.Entry{Title: "weak"}, Score: 0.5}}}}, // below threshold → loop runs
		OnComplete: func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if model.i == 0 {
		t.Fatal("model not called; a below-threshold score must run the loop")
	}
}

func TestRecallRejectedByVerifyFallsThrough(t *testing.T) {
	// A recall hits a strong entry, but the adversarial verify pass rejects every
	// root cause (a stale/poisoned catalog entry — the exact case verify exists to
	// catch). The loop must NOT publish the now-empty recall; it must fall through
	// to a real investigation. Regression: it previously delivered an empty "recall"
	// result and returned.
	model := &scriptModel{responses: []providers.CompletionResponse{
		// 1) verify the recalled finding → reject it entirely (empties root causes).
		{ToolCalls: []providers.ToolCall{{ID: "v1", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"reject","reason":"correlation only"}]}`}}},
		// 2) the fall-through loop investigates and submits a fresh finding.
		{ToolCalls: []providers.ToolCall{{ID: "f1", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"freshly investigated","confidence":0.8}]}`}}},
		// 3) the loop verifies its own fresh finding → keep it.
		{ToolCalls: []providers.ToolCall{{ID: "v2", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"keep","confidence":0.8}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:  model,
		Verify: true,
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall: &Recall{MinScore: 2.0, Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{Entry: catalog.Entry{Title: "Stale entry", Description: "no longer applies", Path: "stale.md", Resource: "tooling/harbor"}, Score: 5.0}}}},
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "HarborProbeFailure", Fingerprint: "fp-x", Workload: providers.Workload{Namespace: "tooling", Name: "harbor"}}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("nothing delivered")
	}
	if got.Recalled {
		t.Fatalf("a verify-rejected recall must fall through to a real investigation, not deliver an empty recall: %+v", got)
	}
	if len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "freshly investigated" {
		t.Fatalf("expected the fall-through loop's fresh finding, got %+v", got)
	}
	if model.i < 2 {
		t.Fatalf("expected the loop to run after the recall was rejected (>=2 model calls), got %d", model.i)
	}
}

func TestOnRecallReportsDecision(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	strongHit := fakeScored{hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Title: "Known incident", Description: "chart bump", Path: "known.md", Resource: "tooling/harbor"}, Score: 5.0}}}
	wl := providers.Workload{Namespace: "tooling", Name: "harbor"}

	t.Run("short_circuit", func(t *testing.T) {
		// A strong hit with verify off (or accepting): recall fires and short-circuits.
		var d RecallDecision
		li := &LoopInvestigator{
			Model:      &scriptModel{}, // never called: a recall short-circuit skips the loop
			Log:        discard,
			Recall:     &Recall{MinScore: 2.0, Catalog: strongHit},
			OnRecall:   func(got RecallDecision) { d = got },
			OnComplete: func(providers.Investigation) {},
		}
		if err := li.Investigate(context.Background(), Request{Title: "HarborProbeFailure", Workload: wl}); err != nil {
			t.Fatalf("Investigate: %v", err)
		}
		if !d.Fired || !d.ShortCircuited || d.Entry != "known.md" {
			t.Fatalf("expected fired+short-circuited on known.md, got %+v", d)
		}
	})

	t.Run("withdrawn_by_verify", func(t *testing.T) {
		// Recall fires but the verify pass rejects it; the loop falls through and the
		// decision must report Fired=true, ShortCircuited=false.
		model := &scriptModel{responses: []providers.CompletionResponse{
			{ToolCalls: []providers.ToolCall{{ID: "v1", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"reject","reason":"correlation only"}]}`}}},
			{ToolCalls: []providers.ToolCall{{ID: "f1", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"freshly investigated","confidence":0.8}]}`}}},
			{ToolCalls: []providers.ToolCall{{ID: "v2", Name: submitVerdictsName, Args: `{"verdicts":[{"index":0,"verdict":"keep","confidence":0.8}]}`}}},
		}}
		var d RecallDecision
		li := &LoopInvestigator{
			Model:      model,
			Verify:     true,
			Log:        discard,
			Recall:     &Recall{MinScore: 2.0, Catalog: strongHit},
			OnRecall:   func(got RecallDecision) { d = got },
			OnComplete: func(providers.Investigation) {},
		}
		if err := li.Investigate(context.Background(), Request{Title: "HarborProbeFailure", Workload: wl}); err != nil {
			t.Fatalf("Investigate: %v", err)
		}
		if !d.Fired || d.ShortCircuited || d.Entry != "known.md" {
			t.Fatalf("expected fired+withdrawn on known.md, got %+v", d)
		}
	})

	t.Run("did_not_fire", func(t *testing.T) {
		// A below-threshold hit: recall is consulted but no gate clears; the decision
		// must report Fired=false and the loop runs.
		model := &scriptModel{responses: []providers.CompletionResponse{
			{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.5,"root_causes":[{"summary":"fresh"}]}`}}},
		}}
		var d RecallDecision
		fired := false
		li := &LoopInvestigator{
			Model:      model,
			Log:        discard,
			Recall:     &Recall{MinScore: 2.0, Catalog: fakeScored{hits: []catalog.ScoredEntry{{Entry: catalog.Entry{Title: "weak", Resource: "tooling/harbor"}, Score: 0.5}}}},
			OnRecall:   func(got RecallDecision) { d = got; fired = true },
			OnComplete: func(providers.Investigation) {},
		}
		if err := li.Investigate(context.Background(), Request{Title: "x", Workload: wl}); err != nil {
			t.Fatalf("Investigate: %v", err)
		}
		if !fired {
			t.Fatal("OnRecall must be called even when recall does not fire")
		}
		if d.Fired || d.ShortCircuited {
			t.Fatalf("expected did-not-fire decision, got %+v", d)
		}
	})
}

func TestLoopRefusalUnresolved(t *testing.T) {
	// The model declines the turn (a safety/refusal stop reason, empty content). The
	// loop must deliver a first-class `unresolved` result and STOP after one call —
	// not misread the empty response as a prose turn (which would burn a nudge) nor
	// retry into the same refusal.
	model := &scriptModel{responses: []providers.CompletionResponse{
		{StopReason: "refusal"}, // no text, no tool calls
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "PodCrashLooping", Fingerprint: "fp-refuse"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if model.i != 1 {
		t.Fatalf("model called %d times; a refusal must stop after one call (no nudge/retry)", model.i)
	}
	if got == nil {
		t.Fatal("nothing delivered")
	}
	if len(got.RootCauses) != 0 || got.Confidence != 0 {
		t.Fatalf("a refusal must produce no root cause: %+v", got)
	}
	if len(got.Unresolved) == 0 || !strings.Contains(strings.ToLower(got.Unresolved[0]), "declined") {
		t.Fatalf("refusal must be reported as unresolved with a note, got %+v", got.Unresolved)
	}
	if got.Fingerprint != "fp-refuse" {
		t.Fatalf("refusal result must carry the fingerprint for outcome attribution, got %q", got.Fingerprint)
	}
}

func TestLoopInvestigatorActions(t *testing.T) {
	// submit_findings proposes a reversible and an irreversible action.
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "submit_findings", Args: `{"confidence":0.9,"root_causes":[{"summary":"x"}],"actions":[{"description":"flux rollback hr/harbor","reversible":true},{"description":"delete the pvc","reversible":false}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Actions:    action.New(config.ActionPolicy{Mode: config.ActionSuggest, Allow: config.ActionAllow{ReversibleOnly: true}}),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "t", Fingerprint: "fp-loop", TriggerKey: "tk-loop"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// Only the reversible action is surfaced (suggest mode, reversible_only); none executed.
	if got == nil || len(got.Actions) != 1 || got.Actions[0].Description != "flux rollback hr/harbor" {
		t.Fatalf("expected only the reversible action surfaced, got %+v", got)
	}
	if got.Fingerprint != "fp-loop" {
		t.Fatalf("full-loop path must carry the alert fingerprint for outcome attribution, got %q", got.Fingerprint)
	}
	if got.TriggerKey != "tk-loop" {
		t.Fatalf("full-loop path must propagate the trigger key for dedup, got %q", got.TriggerKey)
	}
}

func TestLoopStampsAlertMetadata(t *testing.T) {
	// The six trigger-time facts come verbatim from the Request (the model never
	// sets them) and must ride the delivered investigation for notification rendering.
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "submit_findings", Args: `{"confidence":0.9,"root_causes":[{"summary":"x"}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	start := time.Now().Add(-5 * time.Minute)
	req := Request{
		Title:       "t",
		Severity:    "warning",
		Environment: "prod",
		At:          start,
		Labels:      map[string]string{"alertname": "BackupJobsMissing", "cluster": "sanofi-003", "tenant": "sanofi-003"},
	}
	if err := li.Investigate(context.Background(), req); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("nothing delivered")
	}
	if got.Severity != "warning" || got.Environment != "prod" {
		t.Fatalf("severity/environment not stamped: %+v", got)
	}
	if got.Cluster != "sanofi-003" || got.Tenant != "sanofi-003" || got.AlertName != "BackupJobsMissing" {
		t.Fatalf("cluster/tenant/alertname not stamped: %+v", got)
	}
	if !got.StartedAt.Equal(start) {
		t.Fatalf("StartedAt not stamped: got %v want %v", got.StartedAt, start)
	}
}

func TestLoopInvestigatorActionsDisabled(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "submit_findings", Args: `{"confidence":0.9,"root_causes":[{"summary":"x"}],"actions":[{"description":"flux rollback","reversible":true}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{ // no Actions policy => read-only, actions dropped
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	_ = li.Investigate(context.Background(), Request{Title: "t"})
	if got == nil || len(got.Actions) != 0 {
		t.Fatalf("expected no actions surfaced when policy disabled, got %+v", got)
	}
}

func TestLoopNudgesOnProseTurn(t *testing.T) {
	// Some models (Gemini in particular) answer the final turn in prose instead of
	// calling submit_findings. The loop must nudge once and recover, not give up.
	model := &scriptModel{responses: []providers.CompletionResponse{
		{Text: "Based on the evidence, the chart bump broke the DB. Confidence ~0.8."}, // prose, no tool call
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.8,"root_causes":[{"summary":"chart bump broke db"}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil || len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "chart bump broke db" {
		t.Fatalf("nudge did not recover findings: %+v", got)
	}
	if model.i != 2 {
		t.Fatalf("expected 2 model calls (prose + nudged submit), got %d", model.i)
	}
}

func TestLoopInconclusiveAfterNudge(t *testing.T) {
	// If the model still won't call a tool after the nudge, give up — don't loop forever.
	model := &scriptModel{responses: []providers.CompletionResponse{
		{Text: "I think it's the database."},
		{Text: "Still just the database, no tool call."},
	}}
	var delivered bool
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(providers.Investigation) { delivered = true },
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if delivered {
		t.Fatal("expected no delivery when the model never calls submit_findings")
	}
	if model.i != 2 {
		t.Fatalf("expected exactly 2 model calls (initial + one nudge), got %d", model.i)
	}
}

// scriptModel returns a fixed sequence of responses, recording every request it
// saw (so tests can assert what was sent, e.g. a forced ToolChoice).
type scriptModel struct {
	responses []providers.CompletionResponse
	reqs      []providers.CompletionRequest
	i         int
}

func (m *scriptModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.reqs = append(m.reqs, req)
	r := m.responses[m.i]
	m.i++
	return r, nil
}

// echoTool is a trivial tool that echoes a fixed reply, used to advance a multi-turn
// loop (the first assistant turn calls it, the second concludes).
type echoTool struct{ name string }

func (e echoTool) Name() string                                 { return e.name }
func (e echoTool) Description() string                          { return "echo fake tool" }
func (e echoTool) Schema() string                               { return `{"type":"object"}` }
func (e echoTool) Call(context.Context, string) (string, error) { return "ok", nil }

// TestLoopCarriesOpaqueToHistory proves resp.Opaque flows response → assistant
// history turn → the NEXT request unchanged: the first completion returns tool calls
// plus an Opaque payload (e.g. signed thinking blocks); the loop must store it on the
// assistant turn so the model's second request replays it verbatim.
func TestLoopCarriesOpaqueToHistory(t *testing.T) {
	opaque := json.RawMessage(`[{"type":"thinking","thinking":"reason","signature":"sig-1"}]`)
	model := &scriptModel{responses: []providers.CompletionResponse{
		// Turn 1: a normal tool call, carrying provider-opaque replay content.
		{ToolCalls: []providers.ToolCall{{ID: "c1", Name: "what_changed", Args: "{}"}}, Opaque: opaque},
		// Turn 2: conclude.
		{ToolCalls: []providers.ToolCall{{ID: "c2", Name: submitFindingsName, Args: `{"confidence":0.9,"root_causes":[{"summary":"x"}]}`}}},
	}}
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{echoTool{name: "what_changed"}},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "t"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(model.reqs) != 2 {
		t.Fatalf("want 2 model requests, got %d", len(model.reqs))
	}
	// The second request's history must contain the assistant turn from turn 1 with the
	// Opaque payload carried through unchanged.
	var found bool
	for _, m := range model.reqs[1].Messages {
		if m.Role == "assistant" && string(m.Opaque) == string(opaque) {
			found = true
		}
	}
	if !found {
		t.Fatalf("assistant turn in the next request must carry resp.Opaque verbatim; messages=%+v", model.reqs[1].Messages)
	}
}

// blockingModel blocks every Complete on ctx, returning ctx.Err() when the
// per-investigation deadline fires — a hung model / slow tool stand-in.
type blockingModel struct{ calls int }

func (m *blockingModel) Complete(ctx context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	<-ctx.Done()
	return providers.CompletionResponse{}, ctx.Err()
}

func TestInvestigateDeadline(t *testing.T) {
	model := &blockingModel{}
	var got *providers.Investigation
	var delivered int
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   20, // the deadline, not MaxSteps, must end this
		Timeout:    20 * time.Millisecond,
		OnComplete: func(inv providers.Investigation) { got = &inv; delivered++ },
	}
	done := make(chan error, 1)
	go func() { done <- li.Investigate(context.Background(), Request{Title: "HungClone"}) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("a timed-out investigation must deliver a result, not return an error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Investigate did not return — the per-investigation deadline is not enforced")
	}
	if delivered != 1 || got == nil {
		t.Fatalf("expected exactly one timeout delivery, got %d (%+v)", delivered, got)
	}
	if len(got.Unresolved) == 0 || !strings.Contains(strings.ToLower(got.Unresolved[0]), "deadline") {
		t.Fatalf("timeout result must note the deadline in Unresolved, got %+v", got.Unresolved)
	}
	if model.calls > 1 {
		t.Fatalf("deadline should bound the investigation to a single blocked model call, got %d", model.calls)
	}
}

func TestInvestigateNoDeadlineWhenZero(t *testing.T) {
	// Timeout==0 ⇒ no WithTimeout ⇒ a normal scripted run is unaffected.
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"confidence":0.7,"root_causes":[{"summary":"ok"}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Timeout:    0, // disabled
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "x"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil || len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "ok" {
		t.Fatalf("Timeout==0 must not alter a normal run: %+v", got)
	}
}

// blockingTool's Call blocks until its context is cancelled (a stuck git clone /
// unresponsive metrics-logs endpoint stand-in). It records how many times Call ran
// and whether the most recent call observed its context expire.
type blockingTool struct {
	name      string
	calls     int
	ctxExpiry error
}

func (b *blockingTool) Name() string        { return b.name }
func (b *blockingTool) Description() string { return "" }
func (b *blockingTool) Schema() string      { return "{}" }
func (b *blockingTool) Call(ctx context.Context, _ string) (string, error) {
	b.calls++
	<-ctx.Done()
	b.ctxExpiry = ctx.Err()
	return "", ctx.Err()
}

// TestRunToolPerToolTimeout asserts a hung tool is bounded by ToolTimeout: runTool
// returns a clear, non-fatal "timed out" string PROMPTLY (without waiting on the
// much larger parent deadline) so the loop records it and continues.
func TestRunToolPerToolTimeout(t *testing.T) {
	tool := &blockingTool{name: "stuck_tool"}
	li := &LoopInvestigator{
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ToolTimeout: 20 * time.Millisecond,
	}
	byName := map[string]Tool{tool.Name(): tool}

	type res struct {
		out string
		dur time.Duration
	}
	ch := make(chan res, 1)
	go func() {
		start := time.Now()
		// Parent ctx carries a far larger deadline so the only thing that can unblock
		// the tool promptly is the per-tool timeout, not the investigation deadline.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		out := li.runTool(ctx, byName, providers.ToolCall{ID: "1", Name: tool.Name(), Args: "{}"})
		ch <- res{out: out, dur: time.Since(start)}
	}()

	select {
	case r := <-ch:
		if r.dur > 2*time.Second {
			t.Fatalf("runTool did not honour the per-tool timeout (took %v)", r.dur)
		}
		if !strings.Contains(strings.ToLower(r.out), "timed out") {
			t.Fatalf("expected a non-fatal timeout message, got %q", r.out)
		}
		if !strings.Contains(r.out, tool.Name()) {
			t.Fatalf("timeout message should name the tool, got %q", r.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runTool hung past the per-tool timeout")
	}
	if tool.calls != 1 {
		t.Fatalf("tool should have been called exactly once, got %d", tool.calls)
	}
}

// TestRunToolFastUnaffected asserts a fast tool returns its normal output when a
// per-tool timeout is configured (the timeout must not alter the happy path).
func TestRunToolFastUnaffected(t *testing.T) {
	tool := &fakeConfirmTool{name: "fast_tool", out: "all good"}
	li := &LoopInvestigator{
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ToolTimeout: time.Second, // generous; the fast tool returns well within it
	}
	byName := map[string]Tool{tool.Name(): tool}
	out := li.runTool(context.Background(), byName, providers.ToolCall{ID: "1", Name: tool.Name(), Args: `{"k":"v"}`})
	if out != "all good" {
		t.Fatalf("fast tool output altered by per-tool timeout: got %q", out)
	}
	if tool.gotArgs != `{"k":"v"}` {
		t.Fatalf("per-tool timeout wrapping must pass args through, got %q", tool.gotArgs)
	}
}

// TestRunToolParentDeadlineNotSwallowed asserts that when the PARENT investigation
// deadline fires (not the per-tool one), runTool does NOT report a per-tool timeout:
// the investigation-level deadline must surface as a normal error so the loop's own
// deadline handling (synthetic timeout result) takes over rather than being masked
// as a per-tool timeout-and-continue.
func TestRunToolParentDeadlineNotSwallowed(t *testing.T) {
	tool := &blockingTool{name: "stuck_tool"}
	li := &LoopInvestigator{
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ToolTimeout: 10 * time.Second, // far larger than the parent deadline below
	}
	byName := map[string]Tool{tool.Name(): tool}

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	ch := make(chan string, 1)
	go func() {
		ch <- li.runTool(ctx, byName, providers.ToolCall{ID: "1", Name: tool.Name(), Args: "{}"})
	}()
	select {
	case out := <-ch:
		// The parent investigation deadline fired, NOT the per-tool timeout. It must
		// not be reported as a per-tool "timed out" message (which the loop would treat
		// as a recorded tool result and continue) — it must surface as a normal error.
		if strings.Contains(strings.ToLower(out), "timed out") {
			t.Fatalf("parent deadline was masked as a per-tool timeout: %q", out)
		}
		if !strings.HasPrefix(out, "error:") {
			t.Fatalf("parent-deadline cancellation should surface as a normal error, got %q", out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runTool hung past the parent deadline")
	}
}

// TestRunToolTimeoutDisabled asserts ToolTimeout==0 leaves runTool's behaviour
// unchanged: a tool error is returned verbatim as before (no per-tool wrapping).
func TestRunToolTimeoutDisabled(t *testing.T) {
	tool := &fakeConfirmTool{name: "ok_tool", out: "fine"}
	li := &LoopInvestigator{
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ToolTimeout: 0, // disabled
	}
	byName := map[string]Tool{tool.Name(): tool}
	if out := li.runTool(context.Background(), byName, providers.ToolCall{ID: "1", Name: tool.Name(), Args: "{}"}); out != "fine" {
		t.Fatalf("ToolTimeout==0 must not alter a normal call, got %q", out)
	}
}

// TestRunToolTimeoutMetricLabel verifies that a per-tool timeout is recorded with
// result="timeout" (not "error") so it is distinguishable from a real backend
// failure in dashboards/alerts. The fix moved timeout classification BEFORE the
// metric block so the correct label reaches the counter.
func TestRunToolTimeoutMetricLabel(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	m := telemetry.NewMetrics()

	tool := &blockingTool{name: "hung_tool"}
	li := &LoopInvestigator{
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		ToolTimeout: 20 * time.Millisecond,
		Metrics:     m,
	}
	byName := map[string]Tool{tool.Name(): tool}

	// Parent ctx has a far longer deadline so only the per-tool timeout fires.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out := li.runTool(ctx, byName, providers.ToolCall{ID: "1", Name: tool.Name(), Args: "{}"})
	if !strings.Contains(strings.ToLower(out), "timed out") {
		t.Fatalf("expected a timed-out message, got %q", out)
	}

	// Scrape /metrics and confirm result="timeout" appears (not result="error").
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, `result="timeout"`) {
		t.Fatalf("per-tool timeout must record result=\"timeout\", not result=\"error\":\n%s", body)
	}
	if strings.Contains(body, `result="error"`) {
		t.Fatalf("per-tool timeout must NOT record result=\"error\" (it was mis-classified before the fix):\n%s", body)
	}
}

// TestInvestigatePerToolTimeoutNonFatal is a loop-level integration test proving
// the full path: a model requests a hung tool, the per-tool timeout fires (non-
// fatal), the loop records the "timed out" message in history and continues to a
// second model call, which submits findings — the investigation reaches a result
// rather than aborting.
func TestInvestigatePerToolTimeoutNonFatal(t *testing.T) {
	hung := &blockingTool{name: "hung_tool"}

	// The scriptModel returns two responses:
	//   1. request the blocking tool
	//   2. after seeing the timed-out result in history, submit findings
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: hung.Name(), Args: "{}"}}},
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitFindingsName,
			Args: `{"confidence":0.7,"root_causes":[{"summary":"found despite hung tool"}]}`}}},
	}}

	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:       model,
		Tools:       []Tool{hung},
		Log:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:    5,
		ToolTimeout: 20 * time.Millisecond, // short so the test is fast
		OnComplete:  func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "HungToolTest"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// The loop must have reached a result (non-fatal timeout → loop continued).
	if got == nil {
		t.Fatal("investigation must deliver a result when a tool times out (non-fatal)")
	}
	if len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "found despite hung tool" {
		t.Fatalf("unexpected finding: %+v", got)
	}
	// The model must have been called twice: once to get the tool call, once
	// after the timed-out message was recorded in history.
	if model.i != 2 {
		t.Fatalf("expected 2 model calls (tool request + findings after timeout), got %d", model.i)
	}
	// The blocking tool must have been called exactly once and its context must
	// have expired via the per-tool deadline (not the parent investigation).
	if hung.calls != 1 {
		t.Fatalf("hung_tool should have been called exactly once, got %d", hung.calls)
	}
	if !errors.Is(hung.ctxExpiry, context.DeadlineExceeded) {
		t.Fatalf("hung_tool context should have expired via deadline, got %v", hung.ctxExpiry)
	}
}

// errModel returns a plain (non-context) error on Complete — a backend rejecting
// the request (auth, 5xx, malformed response), distinct from the deadline path.
type errModel struct {
	err   error
	calls int
}

func (m *errModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	return providers.CompletionResponse{}, m.err
}

// TestInvestigateModelError asserts a non-deadline model error bubbles up wrapped
// as "model: …" (so the caller/queue sees a real failure), as opposed to the
// deadline path which delivers a synthetic timeout result and returns nil.
func TestInvestigateModelError(t *testing.T) {
	model := &errModel{err: errors.New("503 upstream unavailable")}
	var delivered int
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		OnComplete: func(providers.Investigation) { delivered++ },
	}
	err := li.Investigate(context.Background(), Request{Title: "BackendDown"})
	if err == nil {
		t.Fatal("a non-deadline model error must be returned, not swallowed")
	}
	if !strings.HasPrefix(err.Error(), "model: ") {
		t.Fatalf("error must be wrapped as model: …, got %q", err.Error())
	}
	if !errors.Is(err, model.err) {
		t.Fatalf("wrapped error must preserve the cause for errors.Is, got %v", err)
	}
	if delivered != 0 {
		t.Fatalf("a model error must not deliver a result, got %d deliveries", delivered)
	}
	if model.calls != 1 {
		t.Fatalf("a model error must end the loop after one call, got %d", model.calls)
	}
}

// TestLoopRecoversFromMalformedFindings asserts a submit_findings call whose args
// are malformed JSON does not abort the investigation: the parse error is fed back
// to the model as a tool result and the loop continues, delivering the next valid
// submission.
func TestLoopRecoversFromMalformedFindings(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		// turn 1: malformed submit_findings (truncated JSON)
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName, Args: `{"root_causes":[{"summary":`}}},
		// turn 2: a well-formed submission
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitFindingsName,
			Args: `{"confidence":0.7,"root_causes":[{"summary":"recovered"}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "MalformedThenOK"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("loop must recover from a malformed submission and deliver the next valid one")
	}
	if len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "recovered" {
		t.Fatalf("expected the recovered finding, got %+v", got)
	}
	if model.i != 2 {
		t.Fatalf("expected exactly 2 model calls (malformed, then valid), got %d", model.i)
	}
}

func TestLoopInvestigator(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		// turn 1: ask what changed
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "what_changed", Args: `{"namespace":"flux-system"}`}}},
		// turn 2: submit findings
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: "submit_findings", Args: `{"confidence":0.8,"root_causes":[{"summary":"chart bump broke db","confidence":0.8}]}`}}},
	}}
	gp := fakeGitOps{changes: []providers.Change{{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}, FromRev: "a", ToRev: "b",
	}}}

	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{WhatChangedTool{GitOps: gp}},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Source: SourceAlert, Title: "HarborProbeFailure", Workload: providers.Workload{Namespace: "flux-system"}}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("OnComplete was not called")
	}
	if got.Confidence != 0.8 || len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "chart bump broke db" {
		t.Fatalf("unexpected investigation: %+v", got)
	}
	// submit_findings carried no title, so it defaults to the triggering incident.
	if got.Title != "HarborProbeFailure" {
		t.Fatalf("title = %q, want it to default to the request title", got.Title)
	}
	if model.i != 2 {
		t.Fatalf("expected exactly 2 model calls, got %d", model.i)
	}
}

func TestPreferDiscoveredResource(t *testing.T) {
	origin := providers.Workload{Namespace: "apps", Name: "web"}
	cases := []struct {
		name       string
		discovered providers.Workload
		want       providers.Workload
	}{
		{"discovered wins", providers.Workload{Namespace: "apps", Name: "payment-api", Kind: "Deployment"}, providers.Workload{Namespace: "apps", Name: "payment-api", Kind: "Deployment"}},
		{"namespace defaulted from origin", providers.Workload{Name: "payment-api"}, providers.Workload{Namespace: "apps", Name: "payment-api"}},
		{"empty falls back to origin", providers.Workload{}, origin},
		{"namespace-only discovered kept", providers.Workload{Namespace: "ops"}, providers.Workload{Namespace: "ops"}},
	}
	for _, c := range cases {
		if got := preferDiscoveredResource(c.discovered, origin); got != c.want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, c.want)
		}
	}
}

// bigTool is a fake Tool that returns a string of length n filled with 'z'.
type bigTool struct{ size int }

func (b bigTool) Name() string        { return "big_tool" }
func (b bigTool) Description() string { return "returns large output" }
func (b bigTool) Schema() string      { return `{"type":"object","properties":{}}` }
func (b bigTool) Call(_ context.Context, _ string) (string, error) {
	return strings.Repeat("z", b.size), nil
}

// TestLoopToolOutputTruncatedMetric verifies that oversized tool outputs are
// truncated and that ToolOutputTruncatedBytes is incremented via OTel metrics.
func TestLoopToolOutputTruncatedMetric(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })

	m := telemetry.NewMetrics()

	model := &scriptModel{responses: []providers.CompletionResponse{
		// step 1: call big_tool
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "big_tool", Args: `{}`}}},
		// step 2: submit findings
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitFindingsName,
			Args: `{"confidence":0.7,"root_causes":[{"summary":"found it"}]}`}}},
	}}

	li := &LoopInvestigator{
		Model:              model,
		Tools:              []Tool{bigTool{size: 2000}},
		Log:                slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:           5,
		MaxToolOutputBytes: 200, // 2000-byte output must be truncated to ~200
		Metrics:            m,
		OnComplete:         func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "big output test"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	// Scrape /metrics and confirm ToolOutputTruncatedBytes appeared.
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "runlore_tool_output_truncated_bytes_total") {
		t.Fatalf("runlore_tool_output_truncated_bytes_total not in metrics:\n%s", body)
	}
}

// TestLoopTruncationNudgeAndMetric verifies that a truncated model response is not
// treated as a finished step: the loop nudges the model once to continue, records the
// truncation metric, and recovers (delivers a result) rather than accepting a half-answer.
func TestLoopTruncationNudgeAndMetric(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	m := telemetry.NewMetrics()

	model := &scriptModel{responses: []providers.CompletionResponse{
		// step 1: a truncated prose turn — the loop must nudge once and continue,
		// NOT treat this cut-off output as a finished (inconclusive) step.
		{Text: "the root cause is probably the chart bu", Truncated: true,
			Usage: providers.Usage{InputTokens: 500, OutputTokens: 4096}},
		// step 2: still truncated, but now carries submit_findings. The single-use
		// nudge has fired, so the loop falls through and processes the findings.
		{Truncated: true, ToolCalls: []providers.ToolCall{{ID: "2", Name: submitFindingsName,
			Args: `{"confidence":0.6,"root_causes":[{"summary":"chart bump"}]}`}}},
	}}

	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:         model,
		Log:           slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:      5,
		Metrics:       m,
		ModelProvider: "anthropic",
		OnComplete:    func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "truncation test"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// The loop recovered after the truncation nudge and delivered the findings.
	if got == nil || len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "chart bump" {
		t.Fatalf("truncation must nudge-and-recover, not drop the investigation: %+v", got)
	}
	// Exactly two model calls: the truncated turn + the recovery turn (nudge is single-use).
	if model.i != 2 {
		t.Fatalf("expected 2 model calls (truncated + recovery), got %d", model.i)
	}

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()
	if !strings.Contains(body, "runlore_model_responses_truncated_total") {
		t.Fatalf("runlore_model_responses_truncated_total not in metrics:\n%s", body)
	}
	if !strings.Contains(body, `provider="anthropic"`) {
		t.Fatalf("truncation metric must carry the provider label:\n%s", body)
	}
}

// runawayModel always returns a tool call (never submit_findings), simulating a
// model that keeps calling tools and never winds down regardless of nudges.
type runawayModel struct {
	calls int
}

func (m *runawayModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	// Return a benign no-op tool call so the loop has something to dispatch.
	return providers.CompletionResponse{
		ToolCalls: []providers.ToolCall{{ID: "r", Name: "noop_tool", Args: `{}`}},
	}, nil
}

// forceOnlyModel returns a benign non-terminal tool call on every free turn and only
// emits submit_findings when the request FORCES it via ToolChoice — the shape of a
// non-converging model (issue #234's claude-sonnet-5) that records a verdict solely
// when compelled. forcedAt records the 1-based request index at which forcing was
// first observed (0 = never).
type forceOnlyModel struct {
	calls    int
	forcedAt int
}

func (m *forceOnlyModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	if req.ToolChoice == submitFindingsName {
		if m.forcedAt == 0 {
			m.forcedAt = m.calls
		}
		return providers.CompletionResponse{ToolCalls: []providers.ToolCall{
			{ID: "f", Name: submitFindingsName,
				Args: `{"confidence":0.3,"root_causes":[{"summary":"best-effort hypothesis under forced conclusion","confidence":0.3}]}`}}}, nil
	}
	return providers.CompletionResponse{ToolCalls: []providers.ToolCall{
		{ID: "t", Name: "noop_tool", Args: `{}`}}}, nil
}

// scrapeMetrics renders the OTel /metrics exposition for assertions on a result label.
func scrapeMetrics(t *testing.T, h http.Handler) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Body.String()
}

// TestLoopForcesFinalSubmitAtMaxSteps proves the step-budget forced-conclusion path:
// a non-converging model that only submits when ToolChoice compels it is forced on
// the loop's FINAL step, its findings are delivered through the normal path, and the
// completion is labelled result="max_steps_degraded" (distinct from "resolved") so
// the metric can tell a forced degraded verdict from a genuine resolution.
func TestLoopForcesFinalSubmitAtMaxSteps(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	m := telemetry.NewMetrics()

	model := &forceOnlyModel{}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   4,
		Metrics:    m,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "NonConverging", Fingerprint: "fp-degraded"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// Forcing must happen exactly on the final step (request index == MaxSteps): the
	// three prior free turns were unforced, the model kept calling tools.
	if model.forcedAt != 4 {
		t.Fatalf("expected forcing on the final step (request 4), got forcedAt=%d (calls=%d)", model.forcedAt, model.calls)
	}
	// The forced submission is delivered through the normal path.
	if got == nil || len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "best-effort hypothesis under forced conclusion" {
		t.Fatalf("forced final submit_findings not delivered: %+v", got)
	}
	if got.Fingerprint != "fp-degraded" {
		t.Fatalf("forced degraded result must carry the fingerprint for attribution, got %q", got.Fingerprint)
	}
	// The completion counter must carry the distinct degraded label, not "resolved".
	body := scrapeMetrics(t, h)
	if !strings.Contains(body, `result="max_steps_degraded"`) {
		t.Fatalf("forced final submit must record result=\"max_steps_degraded\":\n%s", body)
	}
	if strings.Contains(body, `result="resolved"`) {
		t.Fatalf("a forced degraded verdict must NOT be labelled result=\"resolved\":\n%s", body)
	}
}

// TestLoopMaxStepsFallbackWhenForceIgnored proves the fallback: a model that ignores
// even the forced final step (never emits submit_findings) keeps the pre-existing
// silent max_steps behaviour — nothing is delivered and the completion is labelled
// result="max_steps".
func TestLoopMaxStepsFallbackWhenForceIgnored(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	m := telemetry.NewMetrics()

	model := &runawayModel{} // always returns noop_tool, ignoring the forced ToolChoice
	var delivered bool
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   3,
		Metrics:    m,
		OnComplete: func(providers.Investigation) { delivered = true },
	}
	if err := li.Investigate(context.Background(), Request{Title: "IgnoresForce"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if delivered {
		t.Fatal("a model that ignores the forced final step must deliver nothing (silent max_steps fallback)")
	}
	// The loop ran the full step budget, including the forced final step, then fell back.
	if model.calls != 3 {
		t.Fatalf("expected exactly 3 model calls (= MaxSteps), got %d", model.calls)
	}
	body := scrapeMetrics(t, h)
	if !strings.Contains(body, `result="max_steps"`) {
		t.Fatalf("ignored-force exhaustion must record result=\"max_steps\":\n%s", body)
	}
	if strings.Contains(body, `result="max_steps_degraded"`) {
		t.Fatalf("no degraded verdict was produced, so result=\"max_steps_degraded\" must be absent:\n%s", body)
	}
}

// TestLoopEarlyConclusionNotForced proves the normal early-conclusion path is
// unchanged by the final-step forcing: a model that submits on step 0 is never forced
// (ToolChoice stays empty) and the completion keeps result="resolved".
func TestLoopEarlyConclusionNotForced(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	h, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	t.Cleanup(func() { _ = shutdown(context.Background()) })
	m := telemetry.NewMetrics()

	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
			Args: `{"confidence":0.9,"root_causes":[{"summary":"chart bump broke db","confidence":0.9}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5, // plenty of headroom: step 0 concludes, final-step forcing never engages
		Metrics:    m,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "EarlyConclusion"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil || len(got.RootCauses) != 1 {
		t.Fatalf("early conclusion not delivered: %+v", got)
	}
	if len(model.reqs) != 1 {
		t.Fatalf("expected exactly 1 model call (early conclusion), got %d", len(model.reqs))
	}
	if model.reqs[0].ToolChoice != "" {
		t.Fatalf("an early-concluding step must not be forced, got ToolChoice=%q", model.reqs[0].ToolChoice)
	}
	body := scrapeMetrics(t, h)
	if !strings.Contains(body, `result="resolved"`) {
		t.Fatalf("a genuine early conclusion must record result=\"resolved\":\n%s", body)
	}
	if strings.Contains(body, `result="max_steps_degraded"`) {
		t.Fatalf("a genuine resolution must NOT be labelled degraded:\n%s", body)
	}
}

// TestLoopHardKillOnBudgetExhaustion verifies that, after the token-budget nudge
// has fired, the loop hard-kills on the next over-budget check and delivers an
// unresolved result rather than running to maxSteps.
func TestLoopHardKillOnBudgetExhaustion(t *testing.T) {
	model := &runawayModel{}
	var got *providers.Investigation
	var deliveries int
	// MaxTokensPerInvestigation = 1 forces the nudge to fire on step 0 (the real
	// system prompt alone exceeds 1 token), making the hard-kill trigger at step 1.
	li := &LoopInvestigator{
		Model:                     model,
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  50, // far higher than the expected kill point
		MaxTokensPerInvestigation: 1,  // triggers immediately; proves kill happens before maxSteps
		OnComplete: func(inv providers.Investigation) {
			deliveries++
			got = &inv
		},
	}
	if err := li.Investigate(context.Background(), Request{Title: "runaway", Fingerprint: "fp-kill"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// The loop must have terminated well before maxSteps.
	if model.calls >= 50 {
		t.Fatalf("hard-kill did not fire: model was called %d times (= maxSteps), loop ran to the end", model.calls)
	}
	// OnComplete must have been called exactly once.
	if deliveries != 1 {
		t.Fatalf("expected exactly 1 delivery, got %d", deliveries)
	}
	if got == nil {
		t.Fatal("OnComplete never called")
	}
	// The delivered result must carry an Unresolved entry that mentions the budget.
	if len(got.Unresolved) == 0 {
		t.Fatalf("expected at least one Unresolved entry on hard-kill, got none; result: %+v", got)
	}
	foundBudget := false
	for _, u := range got.Unresolved {
		if strings.Contains(u, "budget") {
			foundBudget = true
			break
		}
	}
	if !foundBudget {
		t.Fatalf("Unresolved entry must mention 'budget'; got: %v", got.Unresolved)
	}
	// Fingerprint must be propagated for outcome-ledger attribution.
	if got.Fingerprint != "fp-kill" {
		t.Fatalf("expected fingerprint %q, got %q", "fp-kill", got.Fingerprint)
	}
}

// TestBudgetNudgeForcesSubmitFindings verifies the budget nudge's ToolChoice
// contract: normal loop steps send an empty ToolChoice (the model chooses freely
// between investigation tools and prose), and once the token-budget nudge fires
// every subsequent request forces submit_findings so the model cannot ramble
// instead of concluding.
func TestBudgetNudgeForcesSubmitFindings(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		// step 0 (under budget): the model keeps investigating.
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "noop_tool", Args: `{}`}}},
		// step 1 (over budget, nudge fired, forced): it submits findings.
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitFindingsName, Args: `{"confidence":0.5,"root_causes":[{"summary":"x","confidence":0.5}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   10,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	// Budget = exactly the step-0 estimate (overBudget is a strict >), so step 0 is
	// unforced and step 1 — grown by the assistant turn and the tool result — trips
	// the nudge.
	req := Request{Title: "budget-forcing"}
	seed := []providers.Message{{Role: "user", Content: redact.Secrets(seedPrompt(req))}}
	li.MaxTokensPerInvestigation = estimateTokens(li.system(), seed, []providers.ToolSpec{submitFindingsSpec()})
	if err := li.Investigate(context.Background(), req); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil || len(got.RootCauses) != 1 {
		t.Fatalf("expected the forced submit_findings to be delivered, got %+v", got)
	}
	if len(model.reqs) != 2 {
		t.Fatalf("expected 2 model calls (normal + forced), got %d", len(model.reqs))
	}
	if model.reqs[0].ToolChoice != "" {
		t.Fatalf("normal loop step must not force a tool, got ToolChoice=%q", model.reqs[0].ToolChoice)
	}
	if model.reqs[1].ToolChoice != submitFindingsName {
		t.Fatalf("post-nudge step must force %q, got ToolChoice=%q", submitFindingsName, model.reqs[1].ToolChoice)
	}
	// The nudge message itself must also have been injected.
	last := model.reqs[1].Messages[len(model.reqs[1].Messages)-1]
	if !strings.Contains(last.Content, "token budget") {
		t.Fatalf("budget nudge message missing; last message: %+v", last)
	}
}

// TestLoopHardKillDisabledWhenNoBudget verifies that MaxTokensPerInvestigation=0
// (unlimited) suppresses the hard-kill entirely: a runaway model runs for the full
// maxSteps, exactly as before this change.
func TestLoopHardKillDisabledWhenNoBudget(t *testing.T) {
	model := &runawayModel{}
	li := &LoopInvestigator{
		Model:                     model,
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  5,
		MaxTokensPerInvestigation: 0, // disabled — no hard-kill
		OnComplete:                func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "unlimited"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// With no budget the loop must exhaust maxSteps normally (runaway model never
	// calls submit_findings, so the loop runs until maxSteps).
	if model.calls != 5 {
		t.Fatalf("expected exactly 5 model calls (= maxSteps), got %d — hard-kill must not fire when budget=0", model.calls)
	}
}

// reportingRunawayModel is a runaway model (never submits findings) whose
// completions REPORT provider usage far above the chars/4 heuristic — the
// real-world shape when tool schemas, JSON envelope and tokenizer density make
// the wire size much larger than the character count suggests.
type reportingRunawayModel struct {
	calls       int
	inputTokens int
}

func (m *reportingRunawayModel) Complete(context.Context, providers.CompletionRequest) (providers.CompletionResponse, error) {
	m.calls++
	return providers.CompletionResponse{
		ToolCalls: []providers.ToolCall{{ID: "u", Name: "noop_tool", Args: `{}`}},
		Usage:     providers.Usage{InputTokens: m.inputTokens, OutputTokens: 10},
	}, nil
}

// TestLoopBudgetAnchorsToReportedUsage verifies the budget guard is grounded in
// provider-reported usage, not just chars/4: the message history stays tiny (the
// heuristic alone would never cross the budget, so the pure-heuristic loop runs
// to maxSteps), but the provider reports 400k real input tokens per request — so
// after the first completion the anchored estimate must trip the nudge, and the
// next over-budget check must hard-kill.
func TestLoopBudgetAnchorsToReportedUsage(t *testing.T) {
	model := &reportingRunawayModel{inputTokens: 400_000}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:                     model,
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  50,
		MaxTokensPerInvestigation: 50_000, // far above the heuristic, far below the reported usage
		OnComplete: func(inv providers.Investigation) {
			got = &inv
		},
	}
	if err := li.Investigate(context.Background(), Request{Title: "usage-anchored", Fingerprint: "fp-usage"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// Step 0: uncalibrated, heuristic under budget → model call 1 reports usage.
	// Step 1: anchored estimate over budget → nudge → model call 2.
	// Step 2: still over budget → hard-kill, no further model calls.
	if model.calls != 2 {
		t.Fatalf("expected exactly 2 model calls (nudge then hard-kill), got %d — the guard did not anchor to reported usage", model.calls)
	}
	if got == nil {
		t.Fatal("OnComplete never called: hard-kill must deliver an unresolved result")
	}
	foundBudget := false
	for _, u := range got.Unresolved {
		if strings.Contains(u, "budget") {
			foundBudget = true
		}
	}
	if !foundBudget {
		t.Fatalf("Unresolved entry must mention 'budget'; got: %v", got.Unresolved)
	}
}

func TestInstantRecallUnconfirmedLowersConfidence(t *testing.T) {
	// A recall hit with NO confirm tools available → confidence is capped at
	// recallUnconfirmedCap before delivery.
	var got providers.Investigation
	li := &LoopInvestigator{
		Tools:      nil, // intentionally omit pod_status/kube_events so confirmRecall returns gathered=false
		Verify:     false,
		OnComplete: func(inv providers.Investigation) { got = inv },
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall: &Recall{MinScore: 2.0, Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{Entry: catalog.Entry{Title: "Known incident", Description: "chart bump", Path: "known.md", Resource: "apps/web"}, Score: 5.0}}}},
	}
	req := Request{Title: "web down", Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	if err := li.Investigate(context.Background(), req); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got.Confidence > recallUnconfirmedCap {
		t.Fatalf("unconfirmed recall must be capped at %.2f, got %.2f", recallUnconfirmedCap, got.Confidence)
	}
}

// contentRejectModel is a verify-pass model that makes its verdict content-dependent:
//   - if req.Messages[0].Content contains the sentinel "current state — pod_status"
//     the confirmatory evidence reached verify → reject (root causes emptied → test PASSES).
//   - if the sentinel is absent (confirmRecall wiring removed) → keep (root cause survives
//     → len(got.RootCauses)==1 → test FAILS), proving the test discriminates.
type contentRejectModel struct{}

func (contentRejectModel) Complete(_ context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	const sentinel = "current state — pod_status"
	verdict := "keep"
	if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content, sentinel) {
		verdict = "reject"
	}
	args := `{"verdicts":[{"index":0,"verdict":"` + verdict + `","confidence":0.1,"reason":"content-dependent verdict"}]}`
	return providers.CompletionResponse{
		ToolCalls: []providers.ToolCall{{ID: "1", Name: submitVerdictsName, Args: args}},
	}, nil
}

func TestInstantRecallConfirmedEvidenceReachesVerify(t *testing.T) {
	// A recall hit + a confirm tool whose output contradicts the entry + a verify
	// model that rejects the (now evidence-bearing) root cause → the delivered
	// finding is rejected (root causes emptied), proving the confirmatory evidence
	// reached verify rather than the tautological string.
	//
	// The contentRejectModel makes the verdict content-dependent: it rejects only
	// when "current state — pod_status" appears in the review content (i.e. when
	// confirmRecall wiring is intact). Without the wiring the sentinel is absent,
	// the model returns "keep", root causes survive, and len==0 assertion fails —
	// proving the test genuinely discriminates.
	var got providers.Investigation
	ps := &fakeConfirmTool{name: "pod_status", out: "web Running ready=1/1 (healthy — contradicts the recalled crash)"}
	li := &LoopInvestigator{
		Tools:      []Tool{ps},
		Verify:     true,
		Model:      contentRejectModel{},
		OnComplete: func(inv providers.Investigation) { got = inv },
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall: &Recall{MinScore: 2.0, Catalog: fakeScored{hits: []catalog.ScoredEntry{
			{Entry: catalog.Entry{Title: "Known incident", Description: "crash loop", Path: "known.md", Resource: "apps/web"}, Score: 5.0}}}},
	}
	req := Request{Title: "web down", Workload: providers.Workload{Namespace: "apps", Name: "web"}}
	if err := li.Investigate(context.Background(), req); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if len(got.RootCauses) != 0 {
		t.Fatalf("verify should have rejected the recalled cause using current-state evidence, got %+v", got.RootCauses)
	}
}

// TestCompactionLetsInvestigationFinish drives a model that calls a big-output tool many
// times under a low token budget; with compaction the loop reaches submit_findings
// instead of the budget hard-kill.
func TestCompactionLetsInvestigationFinish(t *testing.T) {
	var resp []providers.CompletionResponse
	for i := 1; i <= 6; i++ {
		resp = append(resp, providers.CompletionResponse{
			ToolCalls: []providers.ToolCall{{ID: fmtID(i), Name: "big_tool", Args: `{}`}},
		})
	}
	resp = append(resp, providers.CompletionResponse{ToolCalls: []providers.ToolCall{
		{ID: "f", Name: submitFindingsName, Args: `{"confidence":0.7,"root_causes":[{"summary":"found it"}]}`},
	}})

	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:                     &scriptModel{responses: resp},
		Tools:                     []Tool{bigTool{size: 4000}},
		Log:                       slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:                  10,
		MaxTokensPerInvestigation: 6000, // low: without compaction, 6 x ~1000-token outputs hard-kill
		OnComplete:                func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "compaction finish test"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil || len(got.RootCauses) == 0 {
		t.Fatalf("expected a resolved investigation via compaction, got %+v", got)
	}
}

func fmtID(i int) string { return string(rune('0' + i)) }

// TestSeedPrompt asserts the seed prompt threads Severity and Environment into
// the model context when set (so it can calibrate rigor for prod vs staging,
// critical vs warning), and omits each line — no empty label — when unset.
func TestSeedPrompt(t *testing.T) {
	t.Run("includes severity and environment when set", func(t *testing.T) {
		req := Request{
			Title:       "PodCrashLooping",
			Source:      SourceAlert,
			Workload:    providers.Workload{Namespace: "apps", Name: "web"},
			Reason:      "CrashLoopBackOff",
			Severity:    "critical",
			Environment: "prod",
		}
		got := seedPrompt(req)
		if !strings.Contains(got, "Severity: critical.") {
			t.Errorf("seed prompt missing severity, got %q", got)
		}
		if !strings.Contains(got, "Environment: prod.") {
			t.Errorf("seed prompt missing environment, got %q", got)
		}
	})
	t.Run("omits severity and environment when empty", func(t *testing.T) {
		req := Request{Title: "PodCrashLooping", Source: SourceAlert}
		got := seedPrompt(req)
		if strings.Contains(got, "Severity:") {
			t.Errorf("seed prompt should omit empty severity, got %q", got)
		}
		if strings.Contains(got, "Environment:") {
			t.Errorf("seed prompt should omit empty environment, got %q", got)
		}
	})
	t.Run("anchors the incident start time so the model can size tool windows", func(t *testing.T) {
		at := time.Now().Add(-42 * time.Minute)
		got := seedPrompt(Request{Title: "PodCrashLooping", Source: SourceAlert, At: at})
		if !strings.Contains(got, "Incident started: "+at.UTC().Format(time.RFC3339)) {
			t.Errorf("seed prompt missing incident start time, got %q", got)
		}
		if !strings.Contains(got, "42m before now") {
			t.Errorf("seed prompt missing relative age (42m), got %q", got)
		}
		if !strings.Contains(got, "since_minutes") {
			t.Errorf("seed prompt should tell the model how to aim tool windows at the start time, got %q", got)
		}
	})
	t.Run("omits the start line when At is zero", func(t *testing.T) {
		got := seedPrompt(Request{Title: "X", Source: SourceAlert})
		if strings.Contains(got, "Incident started:") {
			t.Errorf("seed prompt should omit a zero start time, got %q", got)
		}
	})
	t.Run("carries alert labels and annotations", func(t *testing.T) {
		req := Request{
			Title:  "KubePodCrashLooping",
			Source: SourceAlert,
			Labels: map[string]string{
				"alertname": "KubePodCrashLooping",
				"pod":       "web-abc123",
				"container": "app",
			},
			Annotations: map[string]string{
				"runbook_url": "https://runbooks.example/crashloop",
				"description": "already surfaced as the message",
			},
			Message: "already surfaced as the message",
		}
		got := seedPrompt(req)
		if !strings.Contains(got, `container="app"`) || !strings.Contains(got, `pod="web-abc123"`) {
			t.Errorf("seed prompt missing alert labels, got %q", got)
		}
		if !strings.Contains(got, `runbook_url="https://runbooks.example/crashloop"`) {
			t.Errorf("seed prompt missing runbook_url annotation, got %q", got)
		}
		// The annotation already surfaced as Message must not be duplicated.
		if strings.Count(got, "already surfaced as the message") != 1 {
			t.Errorf("description annotation duplicated in seed prompt, got %q", got)
		}
	})
	t.Run("clips oversized label values", func(t *testing.T) {
		long := strings.Repeat("x", 2000)
		got := seedPrompt(Request{Title: "X", Source: SourceAlert, Labels: map[string]string{"big": long}})
		if strings.Contains(got, long) {
			t.Errorf("seed prompt should clip a 2000-char label value")
		}
		if !strings.Contains(got, "big=") {
			t.Errorf("clipped label should still appear, got %q", got)
		}
	})
}
