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

// maliciousBody is an upstream-controlled error body carrying a secret and a
// forged log record (newline + ANSI). A non-2xx error must not echo any of it.
const maliciousBody = "\n\x1b[2Kfake=record secret=sk-LEAKED-0123456789 level=error msg=\"forged\""

// TestNon2xxErrorOmitsBody asserts a non-2xx response yields an error that
// excludes the upstream body (no secret, no newline) but includes the status and
// the upstream request-id for correlation.
func TestNon2xxErrorOmitsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req-abc-123")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(maliciousBody))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "test-model", "k").Complete(context.Background(), providers.CompletionRequest{
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

// TestUsageAndFinishReason verifies the OpenAI usage block and choice finish_reason
// are parsed onto CompletionResponse: prompt/completion tokens surface on Usage, and a
// "length" finish_reason flags Truncated. A response omitting usage parses to the zero value.
func TestUsageAndFinishReason(t *testing.T) {
	tests := []struct {
		name          string
		body          string
		wantIn        int
		wantOut       int
		wantTruncated bool
	}{
		{
			name:          "usage + stop (not truncated)",
			body:          `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"done"}}],"usage":{"prompt_tokens":130,"completion_tokens":42}}`,
			wantIn:        130,
			wantOut:       42,
			wantTruncated: false,
		},
		{
			name:          "length finish_reason flags truncation",
			body:          `{"choices":[{"finish_reason":"length","message":{"role":"assistant","content":"cut off"}}],"usage":{"prompt_tokens":210,"completion_tokens":4096}}`,
			wantIn:        210,
			wantOut:       4096,
			wantTruncated: true,
		},
		{
			name:          "usage omitted parses to zero value",
			body:          `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"done"}}]}`,
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
			resp, err := New(srv.URL, "test-model", "").Complete(context.Background(), providers.CompletionRequest{
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
