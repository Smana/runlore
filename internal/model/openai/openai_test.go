package openai

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// decodedRequest mirrors chatRequest for test decoding: chatRequest.Messages is
// []any (mixed chatMessage/toolMessage) on the wire, which round-trips through a
// typed []chatMessage here for assertions.
type decodedRequest struct {
	Model    string        `json:"model"`
	Messages []chatMessage `json:"messages"`
	Tools    []chatTool    `json:"tools"`
}

func TestComplete(t *testing.T) {
	var gotReq decodedRequest
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

// TestEmptyToolResultKeepsContent guards the OpenAI-compat requirement that a
// tool-role message always carries a "content" field, even when the tool produced
// no output. With json:"content,omitempty" an empty result elides the field and
// strict servers (OpenAI/vLLM/Ollama) reject the request with 400. We assert on the
// raw request body the server receives, since the typed struct hides the omission.
func TestEmptyToolResultKeepsContent(t *testing.T) {
	var rawBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rawBody = string(b)
		_ = json.NewEncoder(w).Encode(chatResponse{Choices: []chatChoice{{Message: chatMessage{Role: "assistant", Content: "ok"}}}})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-model", "")
	_, err := c.Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: "user", Content: "investigate"},
			{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "tc1", Name: "noop", Args: "{}"}}},
			{Role: "tool", ToolCallID: "tc1", Content: ""}, // empty tool result
		},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// The tool message must serialize an explicit (empty) content field.
	if !strings.Contains(rawBody, `"tool_call_id":"tc1"`) {
		t.Fatalf("tool message missing from request body: %s", rawBody)
	}
	if !strings.Contains(rawBody, `"content":""`) {
		t.Fatalf("empty tool result must keep \"content\":\"\" in request body, got: %s", rawBody)
	}
}
