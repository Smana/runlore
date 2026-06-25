package anthropic

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

const maliciousBody = "\n\x1b[2Kfake=record secret=sk-LEAKED-0123456789 level=error msg=\"forged\""

// TestNon2xxErrorOmitsBody asserts a non-2xx response yields an error that
// excludes the upstream body but includes the status and request-id.
func TestNon2xxErrorOmitsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Request-Id", "req-abc-123")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(maliciousBody))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "claude-x", "k").Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want error for non-2xx response")
	}
	msg := err.Error()
	if strings.Contains(msg, "sk-LEAKED") || strings.Contains(msg, "fake=record") {
		t.Errorf("error leaked upstream body: %q", msg)
	}
	if strings.ContainsAny(msg, "\n\r") {
		t.Errorf("error contains a raw newline (log-injection risk): %q", msg)
	}
	if !strings.Contains(msg, "502") {
		t.Errorf("error should carry the status code: %q", msg)
	}
	if !strings.Contains(msg, "req-abc-123") {
		t.Errorf("error should carry the request-id: %q", msg)
	}
}

func TestComplete(t *testing.T) {
	var gotReq msgRequest
	var gotVersion, gotKey string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotVersion, gotKey = r.Header.Get("anthropic-version"), r.Header.Get("x-api-key")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_, _ = w.Write([]byte(`{"content":[
		  {"type":"text","text":"investigating"},
		  {"type":"tool_use","id":"tu1","name":"what_changed","input":{"namespace":"apps"}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "claude-x", "k")
	resp, err := c.Complete(context.Background(), providers.CompletionRequest{
		System:   "sys",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
		Tools:    []providers.ToolSpec{{Name: "what_changed", Description: "d", Schema: `{"type":"object"}`}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// headers + request mapping
	if gotVersion != apiVersion || gotKey != "k" {
		t.Fatalf("version=%q key=%q", gotVersion, gotKey)
	}
	if gotReq.Model != "claude-x" || gotReq.MaxTokens == 0 {
		t.Fatalf("request: %+v", gotReq)
	}
	// system is sent as a content-block array carrying a prompt-cache breakpoint
	if len(gotReq.System) != 1 || gotReq.System[0].Text != "sys" ||
		gotReq.System[0].CacheControl == nil || gotReq.System[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("system (want one cached 'sys' block): %+v", gotReq.System)
	}
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Name != "what_changed" || string(gotReq.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("tools: %+v", gotReq.Tools)
	}
	// the last tool is the cache breakpoint for the (static) tool schemas
	if gotReq.Tools[0].CacheControl == nil || gotReq.Tools[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("last tool should be a cache breakpoint: %+v", gotReq.Tools[0])
	}
	if len(gotReq.Messages) != 1 || gotReq.Messages[0].Role != "user" || gotReq.Messages[0].Content[0].Text != "hi" {
		t.Fatalf("messages: %+v", gotReq.Messages)
	}
	// response mapping: text + tool_use
	if resp.Text != "investigating" || len(resp.ToolCalls) != 1 ||
		resp.ToolCalls[0].ID != "tu1" || resp.ToolCalls[0].Name != "what_changed" || resp.ToolCalls[0].Args != `{"namespace":"apps"}` {
		t.Fatalf("response: %+v", resp)
	}
}

// TestUsageAndStopReason verifies the Anthropic usage block and stop_reason are
// parsed onto CompletionResponse: token counts surface on Usage, and a "max_tokens"
// stop_reason flags Truncated. A response omitting usage parses to the zero value.
func TestUsageAndStopReason(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantIn        int
		wantOut       int
		wantTruncated bool
	}{
		{
			name:          "usage + end_turn (not truncated)",
			body:          `{"stop_reason":"end_turn","usage":{"input_tokens":120,"output_tokens":45},"content":[{"type":"text","text":"done"}]}`,
			wantIn:        120,
			wantOut:       45,
			wantTruncated: false,
		},
		{
			name:          "max_tokens stop_reason flags truncation",
			body:          `{"stop_reason":"max_tokens","usage":{"input_tokens":200,"output_tokens":4096},"content":[{"type":"text","text":"cut off"}]}`,
			wantIn:        200,
			wantOut:       4096,
			wantTruncated: true,
		},
		{
			name:          "usage omitted parses to zero value",
			body:          `{"stop_reason":"end_turn","content":[{"type":"text","text":"done"}]}`,
			wantIn:        0,
			wantOut:       0,
			wantTruncated: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			resp, err := New(srv.URL, "claude-x", "k").Complete(context.Background(), providers.CompletionRequest{
				Messages: []providers.Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Usage.InputTokens != tt.wantIn || resp.Usage.OutputTokens != tt.wantOut {
				t.Fatalf("usage = %+v, want in=%d out=%d", resp.Usage, tt.wantIn, tt.wantOut)
			}
			if resp.Truncated != tt.wantTruncated {
				t.Fatalf("Truncated = %v, want %v", resp.Truncated, tt.wantTruncated)
			}
		})
	}
}

// TestMessageCoalescing verifies the OpenAI-shaped exchange (assistant tool_calls +
// separate tool messages) maps to Anthropic's tool_use / coalesced tool_result form.
func TestMessageCoalescing(t *testing.T) {
	var gotReq msgRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"done"}]}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "claude-x", "k").Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: "user", Content: "investigate"},
			{Role: "assistant", ToolCalls: []providers.ToolCall{
				{ID: "a", Name: "what_changed", Args: `{"namespace":"apps"}`},
				{ID: "b", Name: "kb_search", Args: `{"query":"x"}`},
			}},
			{Role: "tool", ToolCallID: "a", Content: "changed: chart bump"},
			{Role: "tool", ToolCallID: "b", Content: "runbook: rollback"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(gotReq.Messages) != 3 {
		t.Fatalf("want 3 messages (user, assistant, coalesced user), got %d: %+v", len(gotReq.Messages), gotReq.Messages)
	}
	asst := gotReq.Messages[1]
	if asst.Role != "assistant" || len(asst.Content) != 2 || asst.Content[0].Type != "tool_use" || asst.Content[0].ID != "a" {
		t.Fatalf("assistant turn: %+v", asst)
	}
	results := gotReq.Messages[2]
	if results.Role != "user" || len(results.Content) != 2 ||
		results.Content[0].Type != "tool_result" || results.Content[0].ToolUseID != "a" || results.Content[0].Content != "changed: chart bump" ||
		results.Content[1].ToolUseID != "b" {
		t.Fatalf("coalesced tool results: %+v", results)
	}
}

// TestPromptCacheToolsOnly verifies that with no system prompt, the tools array
// still gets exactly one cache breakpoint — on the LAST tool, not every tool.
func TestPromptCacheToolsOnly(t *testing.T) {
	var gotReq msgRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_, _ = w.Write([]byte(`{"content":[{"type":"text","text":"ok"}]}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "claude-x", "k").Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
		Tools: []providers.ToolSpec{
			{Name: "a", Description: "d", Schema: `{"type":"object"}`},
			{Name: "b", Description: "d", Schema: `{"type":"object"}`},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(gotReq.System) != 0 {
		t.Fatalf("want no system block, got %+v", gotReq.System)
	}
	if gotReq.Tools[0].CacheControl != nil {
		t.Fatalf("only the last tool should be the breakpoint; first tool was marked: %+v", gotReq.Tools[0])
	}
	if gotReq.Tools[1].CacheControl == nil || gotReq.Tools[1].CacheControl.Type != "ephemeral" {
		t.Fatalf("last tool should be the cache breakpoint: %+v", gotReq.Tools[1])
	}
}
