package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestComplete(t *testing.T) {
	var gotReq chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "Bearer k" {
			t.Errorf("auth header = %q, want Bearer k", got)
		}
		if err := json.NewDecoder(r.Body).Decode(&gotReq); err != nil {
			t.Errorf("decode request: %v", err)
		}
		_ = json.NewEncoder(w).Encode(chatResponse{Choices: []chatChoice{{Message: chatMessage{
			Role: "assistant",
			ToolCalls: []chatToolCall{{ID: "tc1", Type: "function", Function: chatFunctionCall{
				Name: "what_changed", Arguments: `{"namespace":"apps"}`}}},
		}}}})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model", "k")
	resp, err := c.Complete(context.Background(), providers.CompletionRequest{
		System:   "sys",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
		Tools:    []providers.ToolSpec{{Name: "what_changed", Description: "d", Schema: `{"type":"object"}`}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// request mapping
	if gotReq.Model != "test-model" {
		t.Fatalf("model = %q", gotReq.Model)
	}
	if len(gotReq.Messages) != 2 || gotReq.Messages[0].Role != "system" || gotReq.Messages[1].Content != "hi" {
		t.Fatalf("messages mapped wrong: %+v", gotReq.Messages)
	}
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Function.Name != "what_changed" {
		t.Fatalf("tools mapped wrong: %+v", gotReq.Tools)
	}
	// response mapping
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "tc1" ||
		resp.ToolCalls[0].Name != "what_changed" || resp.ToolCalls[0].Args != `{"namespace":"apps"}` {
		t.Fatalf("response tool calls mapped wrong: %+v", resp.ToolCalls)
	}
}
