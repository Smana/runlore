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
	if gotReq.Model != "claude-x" || gotReq.System != "sys" || gotReq.MaxTokens == 0 {
		t.Fatalf("request: %+v", gotReq)
	}
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Name != "what_changed" || string(gotReq.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("tools: %+v", gotReq.Tools)
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
