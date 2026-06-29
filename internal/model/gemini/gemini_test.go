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

const maliciousBody = "\n\x1b[2Kfake=record secret=sk-LEAKED-0123456789 level=error msg=\"forged\""

// TestNon2xxErrorOmitsBody asserts a non-2xx response yields an error that
// excludes the upstream body but includes the status and request-id.
func TestNon2xxErrorOmitsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Goog-Request-Id", "req-abc-123")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(maliciousBody))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "gemini-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
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

	c := New(srv.URL, "gemini-x", "k", 0)
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

// TestUsageAndFinishReason verifies the Gemini usageMetadata and candidate finishReason
// are parsed onto CompletionResponse: prompt/candidates token counts surface on Usage, and a
// "MAX_TOKENS" finishReason flags Truncated. A response omitting usageMetadata parses to zero.
func TestUsageAndFinishReason(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantIn        int
		wantOut       int
		wantTruncated bool
	}{
		{
			name:          "usageMetadata + STOP (not truncated)",
			body:          `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"done"}]}}],"usageMetadata":{"promptTokenCount":140,"candidatesTokenCount":48}}`,
			wantIn:        140,
			wantOut:       48,
			wantTruncated: false,
		},
		{
			name:          "MAX_TOKENS finishReason flags truncation",
			body:          `{"candidates":[{"finishReason":"MAX_TOKENS","content":{"role":"model","parts":[{"text":"cut off"}]}}],"usageMetadata":{"promptTokenCount":220,"candidatesTokenCount":4096}}`,
			wantIn:        220,
			wantOut:       4096,
			wantTruncated: true,
		},
		{
			name:          "usageMetadata omitted parses to zero value",
			body:          `{"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[{"text":"done"}]}}]}`,
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
			resp, err := New(srv.URL, "gemini-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
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

	_, err := New(srv.URL, "gemini-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
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

// TestParallelSameFunctionCorrelatesByID verifies that two parallel calls to the
// SAME function name correlate by id: each request functionResponse carries the
// originating call's id (not just the shared name), so Gemini maps them correctly.
func TestParallelSameFunctionCorrelatesByID(t *testing.T) {
	var gotReq genRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
		_, _ = w.Write([]byte(`{"candidates":[{"content":{"parts":[{"text":"done"}]}}]}`))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "gemini-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: "user", Content: "investigate"},
			{Role: "assistant", ToolCalls: []providers.ToolCall{
				{ID: "call_1", Name: "get_status", Args: `{"ns":"a"}`},
				{ID: "call_2", Name: "get_status", Args: `{"ns":"b"}`}, // same function, different id
			}},
			{Role: "tool", ToolCallID: "call_1", Content: "status: a ok"},
			{Role: "tool", ToolCallID: "call_2", Content: "status: b failing"},
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// model turn: both functionCall parts carry their id.
	model := gotReq.Contents[1]
	if len(model.Parts) != 2 ||
		model.Parts[0].FunctionCall == nil || model.Parts[0].FunctionCall.ID != "call_1" ||
		model.Parts[1].FunctionCall == nil || model.Parts[1].FunctionCall.ID != "call_2" {
		t.Fatalf("model functionCall ids wrong: %+v", model.Parts)
	}
	// coalesced tool results: each functionResponse carries the originating id, and
	// the response payload matches the right call.
	res := gotReq.Contents[2]
	if len(res.Parts) != 2 {
		t.Fatalf("want 2 functionResponse parts, got %d: %+v", len(res.Parts), res.Parts)
	}
	byID := map[string]string{}
	for _, p := range res.Parts {
		if p.FunctionResponse == nil {
			t.Fatalf("nil functionResponse: %+v", p)
		}
		if p.FunctionResponse.Name != "get_status" {
			t.Fatalf("functionResponse name = %q, want get_status", p.FunctionResponse.Name)
		}
		byID[p.FunctionResponse.ID] = string(p.FunctionResponse.Response)
	}
	if !strings.Contains(byID["call_1"], "a ok") {
		t.Fatalf("call_1 response mismatched: %q", byID["call_1"])
	}
	if !strings.Contains(byID["call_2"], "b failing") {
		t.Fatalf("call_2 response mismatched: %q", byID["call_2"])
	}
}

// TestResponseFunctionCallID verifies the model's functionCall id (newer API) is
// carried into ToolCall.ID, and that absent id falls back to the function name.
func TestResponseFunctionCallID(t *testing.T) {
	tests := []struct {
		name     string
		body     string
		wantID   string
		wantName string
	}{
		{
			name:     "id present",
			body:     `{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_xyz","name":"what_changed","args":{}}}]}}]}`,
			wantID:   "call_xyz",
			wantName: "what_changed",
		},
		{
			name:     "id absent falls back to name",
			body:     `{"candidates":[{"content":{"parts":[{"functionCall":{"name":"what_changed","args":{}}}]}}]}`,
			wantID:   "what_changed",
			wantName: "what_changed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write([]byte(tt.body))
			}))
			defer srv.Close()
			resp, err := New(srv.URL, "gemini-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
				Messages: []providers.Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != tt.wantID || resp.ToolCalls[0].Name != tt.wantName {
				t.Fatalf("ToolCall = %+v, want id=%q name=%q", resp.ToolCalls, tt.wantID, tt.wantName)
			}
		})
	}
}
