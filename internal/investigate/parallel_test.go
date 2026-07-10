// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// slowTool is a fake investigation tool that sleeps for delay (respecting ctx) and
// echoes its args, recording concurrency: how many calls are in flight at once and
// which args it ran. Safe for concurrent use.
type slowTool struct {
	name  string
	delay time.Duration

	mu        sync.Mutex
	inflight  int
	maxInFly  int
	calledFor []string
}

func (s *slowTool) Name() string        { return s.name }
func (s *slowTool) Description() string { return "slow fake tool" }
func (s *slowTool) Schema() string      { return `{"type":"object"}` }

func (s *slowTool) Call(ctx context.Context, args string) (string, error) {
	s.mu.Lock()
	s.inflight++
	if s.inflight > s.maxInFly {
		s.maxInFly = s.inflight
	}
	s.calledFor = append(s.calledFor, args)
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		s.inflight--
		s.mu.Unlock()
	}()
	select {
	case <-time.After(s.delay):
	case <-ctx.Done():
		return "", ctx.Err()
	}
	return "slow:" + args, nil
}

func (s *slowTool) maxInflight() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.maxInFly
}

func (s *slowTool) calls() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.calledFor...)
}

// toolResultsInOrder extracts the tool-result messages of req in history order as
// (ToolCallID, Content) pairs.
func toolResultsInOrder(req providers.CompletionRequest) (ids, contents []string) {
	for _, m := range req.Messages {
		if m.Role == "tool" {
			ids = append(ids, m.ToolCallID)
			contents = append(contents, m.Content)
		}
	}
	return ids, contents
}

// TestToolCallsRunConcurrentlyAndKeepOrder proves the two core properties of the
// parallel dispatch: (a) one turn's tool calls overlap in time — the turn's wall
// clock is well under the serial sum of the per-call delays — and (b) the tool
// results are appended to history in the ORIGINAL call order even when completion
// order differs (the first call is the slowest).
func TestToolCallsRunConcurrentlyAndKeepOrder(t *testing.T) {
	const perCall = 100 * time.Millisecond
	tool := &slowTool{name: "slow_tool", delay: perCall}
	model := &scriptModel{responses: []providers.CompletionResponse{
		// One turn, four calls. Serial execution would take >= 4*perCall.
		{ToolCalls: []providers.ToolCall{
			{ID: "c1", Name: "slow_tool", Args: `{"n":1}`},
			{ID: "c2", Name: "slow_tool", Args: `{"n":2}`},
			{ID: "c3", Name: "slow_tool", Args: `{"n":3}`},
			{ID: "c4", Name: "slow_tool", Args: `{"n":4}`},
		}},
		{ToolCalls: []providers.ToolCall{{ID: "f", Name: submitFindingsName,
			Args: `{"confidence":0.7,"root_causes":[{"summary":"done"}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{tool},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	start := time.Now()
	if err := li.Investigate(context.Background(), Request{Title: "parallel"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	elapsed := time.Since(start)
	if got == nil || len(got.RootCauses) != 1 {
		t.Fatalf("investigation did not finish: %+v", got)
	}
	// Concurrency: 4 x 100ms serially is >= 400ms; concurrently ~100ms. 300ms is a
	// generous margin for slow CI machines while still proving overlap.
	if serial := 4 * perCall; elapsed >= serial-perCall {
		t.Fatalf("turn took %v; want well under the %v serial sum (calls did not overlap)", elapsed, serial)
	}
	if tool.maxInflight() < 2 {
		t.Fatalf("max in-flight calls = %d; want >= 2 (calls did not overlap)", tool.maxInflight())
	}
	// Ordering: the second model request carries the turn's tool results, which must
	// be in the ORIGINAL call order regardless of completion order.
	if len(model.reqs) != 2 {
		t.Fatalf("expected 2 model calls, got %d", len(model.reqs))
	}
	ids, contents := toolResultsInOrder(model.reqs[1])
	wantIDs := []string{"c1", "c2", "c3", "c4"}
	if len(ids) != len(wantIDs) {
		t.Fatalf("expected %d tool results, got %d (%v)", len(wantIDs), len(ids), ids)
	}
	for i, want := range wantIDs {
		if ids[i] != want {
			t.Fatalf("tool result order = %v, want %v", ids, wantIDs)
		}
		if wantContent := fmt.Sprintf(`slow:{"n":%d}`, i+1); contents[i] != wantContent {
			t.Fatalf("tool result %s content = %q, want %q (result matched to wrong call)", ids[i], contents[i], wantContent)
		}
	}
}

// TestToolCallConcurrencyCap proves the worker cap: with more calls than
// maxConcurrentToolCalls in one turn, at most maxConcurrentToolCalls run at once.
func TestToolCallConcurrencyCap(t *testing.T) {
	tool := &slowTool{name: "slow_tool", delay: 50 * time.Millisecond}
	calls := make([]providers.ToolCall, 0, maxConcurrentToolCalls+3)
	for i := 0; i < maxConcurrentToolCalls+3; i++ {
		calls = append(calls, providers.ToolCall{ID: fmt.Sprintf("c%d", i), Name: "slow_tool", Args: fmt.Sprintf(`{"n":%d}`, i)})
	}
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: calls},
		{ToolCalls: []providers.ToolCall{{ID: "f", Name: submitFindingsName,
			Args: `{"confidence":0.7,"root_causes":[{"summary":"done"}]}`}}},
	}}
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{tool},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		OnComplete: func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), Request{Title: "cap"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got := tool.maxInflight(); got > maxConcurrentToolCalls {
		t.Fatalf("max in-flight calls = %d; the cap is %d", got, maxConcurrentToolCalls)
	}
	if len(tool.calls()) != maxConcurrentToolCalls+3 {
		t.Fatalf("all %d calls must run, got %d", maxConcurrentToolCalls+3, len(tool.calls()))
	}
}

// TestSubmitFindingsMixedTurn locks the sequential turn rule under parallel
// dispatch: when a turn mixes submit_findings with other calls, every call BEFORE
// the first parseable submit_findings runs (and its result is recorded), the
// submit_findings finalizes the investigation, and every call AFTER it never runs.
func TestSubmitFindingsMixedTurn(t *testing.T) {
	before := &slowTool{name: "before_tool"}
	after := &slowTool{name: "after_tool"}
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{
			{ID: "b", Name: "before_tool", Args: `{"which":"before"}`},
			{ID: "f", Name: submitFindingsName, Args: `{"confidence":0.9,"root_causes":[{"summary":"mixed turn"}]}`},
			{ID: "a", Name: "after_tool", Args: `{"which":"after"}`},
		}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{before, after},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "mixed"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil || len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "mixed turn" {
		t.Fatalf("submit_findings in a mixed turn must finalize the investigation, got %+v", got)
	}
	if model.i != 1 {
		t.Fatalf("a parseable submit_findings must end the loop after one model call, got %d", model.i)
	}
	if calls := before.calls(); len(calls) != 1 {
		t.Fatalf("the call BEFORE submit_findings must run exactly once, ran %d times", len(calls))
	}
	if calls := after.calls(); len(calls) != 0 {
		t.Fatalf("the call AFTER submit_findings must NEVER run (sequential semantics), ran %d times: %v", len(calls), calls)
	}
}

// TestMalformedSubmitFindingsAmongCalls locks the other half of the turn rule: a
// submit_findings whose args do NOT parse does not end the turn — it is answered
// with a parse-error tool result in its slot, the turn's other calls still run,
// and the loop continues to the next step.
func TestMalformedSubmitFindingsAmongCalls(t *testing.T) {
	tool := &slowTool{name: "next_tool"}
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{
			{ID: "m", Name: submitFindingsName, Args: `{"root_causes":[{"summary":`}, // malformed JSON
			{ID: "n", Name: "next_tool", Args: `{"go":"on"}`},
		}},
		{ToolCalls: []providers.ToolCall{{ID: "f", Name: submitFindingsName,
			Args: `{"confidence":0.7,"root_causes":[{"summary":"recovered"}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{tool},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		MaxSteps:   5,
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), Request{Title: "malformed mixed"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil || len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "recovered" {
		t.Fatalf("loop must continue past a malformed submit_findings and deliver the valid one, got %+v", got)
	}
	if model.i != 2 {
		t.Fatalf("expected 2 model calls (malformed turn + recovery), got %d", model.i)
	}
	if calls := tool.calls(); len(calls) != 1 {
		t.Fatalf("the other call in the malformed turn must still run, ran %d times", len(calls))
	}
	// The malformed call's slot must carry the parse error, in original order, ahead
	// of the other call's result.
	ids, contents := toolResultsInOrder(model.reqs[1])
	if len(ids) != 2 || ids[0] != "m" || ids[1] != "n" {
		t.Fatalf("tool results must stay in call order [m n], got %v", ids)
	}
	if !strings.HasPrefix(contents[0], "error: ") {
		t.Fatalf("malformed submit_findings must be answered with a parse-error result, got %q", contents[0])
	}
	if contents[1] != `slow:{"go":"on"}` {
		t.Fatalf("other call's result = %q, want its own output", contents[1])
	}
}
