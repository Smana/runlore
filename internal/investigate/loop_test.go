package investigate

import (
	"context"
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
	if err := li.Investigate(context.Background(), Request{Title: "HarborProbeFailure", Fingerprint: "fp-recall", Workload: providers.Workload{Namespace: "tooling", Name: "harbor"}}); err != nil {
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
	if err := li.Investigate(context.Background(), Request{Title: "t", Fingerprint: "fp-loop"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// Only the reversible action is surfaced (suggest mode, reversible_only); none executed.
	if got == nil || len(got.Actions) != 1 || got.Actions[0].Description != "flux rollback hr/harbor" {
		t.Fatalf("expected only the reversible action surfaced, got %+v", got)
	}
	if got.Fingerprint != "fp-loop" {
		t.Fatalf("full-loop path must carry the alert fingerprint for outcome attribution, got %q", got.Fingerprint)
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

// scriptModel returns a fixed sequence of responses, ignoring its input.
type scriptModel struct {
	responses []providers.CompletionResponse
	i         int
}

func (m *scriptModel) Complete(context.Context, providers.CompletionRequest) (providers.CompletionResponse, error) {
	r := m.responses[m.i]
	m.i++
	return r, nil
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
