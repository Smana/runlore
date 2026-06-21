package gemini

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestComplete(t *testing.T) {
	var gotReq genRequest
	var gotKey, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotKey = r.Header.Get("x-goog-api-key")
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"role":"model","parts":[
		  {"text":"investigating"},
		  {"functionCall":{"name":"what_changed","args":{"namespace":"apps"}}}]}}]}`))
	}))
	defer srv.Close()

	c := New(srv.URL, "gemini-x", "k")
	resp, err := c.Complete(context.Background(), providers.CompletionRequest{
		System:   "sys",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
		Tools:    []providers.ToolSpec{{Name: "what_changed", Description: "d", Schema: `{"type":"object"}`}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// auth header + URL
	if gotKey != "k" {
		t.Fatalf("x-goog-api-key = %q", gotKey)
	}
	if !strings.HasSuffix(gotPath, "/v1beta/models/gemini-x:generateContent") {
		t.Fatalf("path = %q", gotPath)
	}
	// request mapping: system_instruction, tools (functionDeclarations), contents
	if gotReq.SystemInstruction == nil || gotReq.SystemInstruction.Parts[0].Text != "sys" {
		t.Fatalf("system: %+v", gotReq.SystemInstruction)
	}
	if len(gotReq.Tools) != 1 || len(gotReq.Tools[0].FunctionDeclarations) != 1 ||
		gotReq.Tools[0].FunctionDeclarations[0].Name != "what_changed" ||
		string(gotReq.Tools[0].FunctionDeclarations[0].Parameters) != `{"type":"object"}` {
		t.Fatalf("tools: %+v", gotReq.Tools)
	}
	if len(gotReq.Contents) != 1 || gotReq.Contents[0].Role != "user" || gotReq.Contents[0].Parts[0].Text != "hi" {
		t.Fatalf("contents: %+v", gotReq.Contents)
	}
	// response mapping: text + functionCall → ToolCall (args object → string)
	if resp.Text != "investigating" || len(resp.ToolCalls) != 1 ||
		resp.ToolCalls[0].Name != "what_changed" || resp.ToolCalls[0].Args != `{"namespace":"apps"}` {
		t.Fatalf("response: %+v", resp)
	}
}

// TestToolResultCoalescing verifies the OpenAI-shaped exchange (assistant tool_calls
// + separate tool messages) maps to Gemini's model functionCall / coalesced user
// functionResponse form, named by the originating call.
func TestToolResultCoalescing(t *testing.T) {
	var gotReq genRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"done"}]}}]}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "gemini-x", "k").Complete(context.Background(), providers.CompletionRequest{
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
	if len(gotReq.Contents) != 3 {
		t.Fatalf("want 3 contents (user, model, coalesced user), got %d: %+v", len(gotReq.Contents), gotReq.Contents)
	}
	model := gotReq.Contents[1]
	if model.Role != "model" || len(model.Parts) != 2 ||
		model.Parts[0].FunctionCall == nil || model.Parts[0].FunctionCall.Name != "what_changed" {
		t.Fatalf("model turn: %+v", model)
	}
	results := gotReq.Contents[2]
	if results.Role != "user" || len(results.Parts) != 2 ||
		results.Parts[0].FunctionResponse == nil || results.Parts[0].FunctionResponse.Name != "what_changed" ||
		results.Parts[1].FunctionResponse == nil || results.Parts[1].FunctionResponse.Name != "kb_search" {
		t.Fatalf("coalesced tool results: %+v", results)
	}
	if !strings.Contains(string(results.Parts[0].FunctionResponse.Response), "chart bump") {
		t.Fatalf("functionResponse.response should wrap the tool output: %s", results.Parts[0].FunctionResponse.Response)
	}
}
