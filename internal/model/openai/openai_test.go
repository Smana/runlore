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
// typed []chatMessage here for assertions. The streaming knobs (stream,
// max_tokens, stream_options) are decoded too so a test can assert the request.
type decodedRequest struct {
	Model         string         `json:"model"`
	Messages      []chatMessage  `json:"messages"`
	Tools         []chatTool     `json:"tools"`
	MaxTokens     int            `json:"max_tokens"`
	Stream        bool           `json:"stream"`
	StreamOptions *streamOptions `json:"stream_options"`
}

// maliciousBody is an upstream-controlled error body carrying a secret and a
// forged log record (newline + ANSI). A non-2xx error must not echo any of it.
const maliciousBody = "\n\x1b[2Kfake=record secret=sk-LEAKED-0123456789 level=error msg=\"forged\""

// sseServer returns a test server that writes the given SSE event lines, flushing
// after each so the client sees a real incremental stream.
func sseServer(t *testing.T, capture func(*http.Request), events []string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if capture != nil {
			capture(r)
		}
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("server needs http.Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		for _, e := range events {
			_, _ = io.WriteString(w, e)
			fl.Flush()
		}
	}))
}

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

	_, err := New(srv.URL, "test-model", "k", 0).Complete(context.Background(), providers.CompletionRequest{
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

// TestOpenAIErrorDetail asserts the error-body parser surfaces the structured
// OpenAI-compatible error type/message (so 4xx causes like a bad model name are
// diagnosable) while never echoing a non-JSON body and never emitting control
// characters (log-injection safe).
func TestOpenAIErrorDetail(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{
			name: "structured 404 is surfaced",
			body: `{"error":{"message":"The model 'gpt-oops' does not exist","type":"invalid_request_error","code":"model_not_found"}}`,
			want: ": invalid_request_error: The model 'gpt-oops' does not exist",
		},
		{name: "non-JSON body is omitted", body: maliciousBody, want: ""},
		{name: "json without error fields is omitted", body: `{"foo":"bar"}`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := openaiErrorDetail([]byte(tc.body)); got != tc.want {
				t.Errorf("openaiErrorDetail(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}

	// A message with control characters must be sanitized (no log injection).
	inj := openaiErrorDetail([]byte(`{"error":{"type":"x","message":"line1\nline2\u001b[2Kforged"}}`))
	if strings.ContainsAny(inj, "\n\r\x1b") {
		t.Errorf("detail leaked control chars: %q", inj)
	}
	// An over-long message is truncated with an ellipsis.
	long := openaiErrorDetail([]byte(`{"error":{"type":"t","message":"` + strings.Repeat("a", 500) + `"}}`))
	if !strings.HasSuffix(long, "…") {
		t.Errorf("long message should be truncated with an ellipsis: %q", long)
	}
}

// TestNon2xxPermanence asserts a 4xx other than 429 is classified permanent (so the
// investigation workqueue drops it), while 429 and 5xx stay transient (retryable).
func TestNon2xxPermanence(t *testing.T) {
	for _, tc := range []struct {
		name string
		code int
		want bool
	}{
		{"400 is permanent", http.StatusBadRequest, true},
		{"401 is permanent", http.StatusUnauthorized, true},
		{"404 is permanent", http.StatusNotFound, true},
		{"429 is transient", http.StatusTooManyRequests, false},
		{"502 is transient", http.StatusBadGateway, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()
			_, err := New(srv.URL, "test-model", "k", 0).Complete(context.Background(), providers.CompletionRequest{
				Messages: []providers.Message{{Role: "user", Content: "hi"}},
			})
			if err == nil {
				t.Fatal("want error for non-2xx response")
			}
			if got := providers.IsPermanent(err); got != tc.want {
				t.Errorf("IsPermanent(status %d) = %v, want %v", tc.code, got, tc.want)
			}
		})
	}
}

// TestComplete drives a full OpenAI chat/completions SSE stream — content deltas, a
// tool_call assembled by index (first chunk carries id+name, later chunks carry
// arguments fragments), a finish_reason chunk, a usage-only chunk, and the [DONE]
// terminator — and asserts the accumulated text, the reassembled tool call, usage,
// stop reason, and the request mapping (stream:true + max_tokens + stream_options).
func TestComplete(t *testing.T) {
	var gotReq decodedRequest
	var gotAuth string
	srv := sseServer(t, func(r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
	}, []string{
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"investi"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"content":"gating"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"tc1","type":"function","function":{"name":"what_changed","arguments":""}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"namespace\":"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"apps\"}"}}]}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":120,"completion_tokens":45}}` + "\n\n",
		"data: [DONE]\n\n",
	})
	defer srv.Close()

	c := New(srv.URL, "test-model", "k", 16384)
	resp, err := c.Complete(context.Background(), providers.CompletionRequest{
		System:   "sys",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
		Tools:    []providers.ToolSpec{{Name: "what_changed", Description: "d", Schema: `{"type":"object"}`}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}

	// request mapping
	if gotAuth != "Bearer k" {
		t.Fatalf("auth header = %q, want Bearer k", gotAuth)
	}
	if gotReq.Model != "test-model" {
		t.Fatalf("model = %q", gotReq.Model)
	}
	if !gotReq.Stream || gotReq.MaxTokens != 16384 {
		t.Fatalf("request (want stream:true, max_tokens:16384): %+v", gotReq)
	}
	if gotReq.StreamOptions == nil || !gotReq.StreamOptions.IncludeUsage {
		t.Fatalf("request must set stream_options.include_usage: %+v", gotReq.StreamOptions)
	}
	if len(gotReq.Messages) != 2 || gotReq.Messages[0].Role != "system" || gotReq.Messages[1].Content != "hi" {
		t.Fatalf("messages mapped wrong: %+v", gotReq.Messages)
	}
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Function.Name != "what_changed" {
		t.Fatalf("tools mapped wrong: %+v", gotReq.Tools)
	}

	// response accumulation: text + reassembled tool call args + usage + stop reason
	if resp.Text != "investigating" {
		t.Fatalf("accumulated text = %q, want %q", resp.Text, "investigating")
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "tc1" ||
		resp.ToolCalls[0].Name != "what_changed" || resp.ToolCalls[0].Args != `{"namespace":"apps"}` {
		t.Fatalf("tool call: %+v", resp.ToolCalls)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 45 {
		t.Fatalf("usage = %+v, want in=120 out=45", resp.Usage)
	}
	if resp.StopReason != "tool_calls" {
		t.Fatalf("StopReason = %q, want tool_calls", resp.StopReason)
	}
}

// TestTruncation verifies a finish_reason of "length" flags Truncated while the
// content accumulated so far is preserved.
func TestTruncation(t *testing.T) {
	srv := sseServer(t, nil, []string{
		`data: {"choices":[{"index":0,"delta":{"content":"cut off"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":210,"completion_tokens":4096}}` + "\n\n",
		"data: [DONE]\n\n",
	})
	defer srv.Close()
	resp, err := New(srv.URL, "test-model", "", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "cut off" {
		t.Fatalf("text = %q, want %q", resp.Text, "cut off")
	}
	if !resp.Truncated {
		t.Fatalf("Truncated = false, want true for finish_reason length")
	}
	if resp.StopReason != "length" {
		t.Fatalf("StopReason = %q, want length", resp.StopReason)
	}
	if resp.Usage.InputTokens != 210 || resp.Usage.OutputTokens != 4096 {
		t.Fatalf("usage = %+v, want in=210 out=4096", resp.Usage)
	}
}

// TestRefusal verifies a finish_reason of "content_filter" with no content surfaces
// as a CompletionResponse that reports Refused()==true.
func TestRefusal(t *testing.T) {
	srv := sseServer(t, nil, []string{
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}` + "\n\n",
		`data: {"choices":[],"usage":{"prompt_tokens":50,"completion_tokens":0}}` + "\n\n",
		"data: [DONE]\n\n",
	})
	defer srv.Close()
	resp, err := New(srv.URL, "test-model", "", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !resp.Refused() {
		t.Fatalf("want Refused()==true for finish_reason content_filter, got StopReason=%q", resp.StopReason)
	}
	if resp.Text != "" || len(resp.ToolCalls) != 0 {
		t.Fatalf("refusal must carry no content: text=%q calls=%+v", resp.Text, resp.ToolCalls)
	}
}

// TestMidStreamDrop verifies a connection dropped mid-stream (before any
// finish_reason and before [DONE]) surfaces as an error rather than a
// silently-truncated success.
func TestMidStreamDrop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Start emitting content but never send a finish_reason or [DONE].
		_, _ = io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"par`)
		fl.Flush()
		// Abruptly close mid-event by hijacking and closing the TCP connection.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	_, err := New(srv.URL, "test-model", "", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want an error when the stream is dropped before a finish_reason / [DONE]")
	}
}

// TestUsageCachedTokens asserts prompt_tokens_details.cached_tokens maps to
// CachedInputTokens (OpenAI prompt_tokens already includes the cached subset).
func TestUsageCachedTokens(t *testing.T) {
	srv := sseServer(t, nil, []string{
		"data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n",
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n",
		"data: {\"choices\":[],\"usage\":{\"prompt_tokens\":200,\"completion_tokens\":9,\"prompt_tokens_details\":{\"cached_tokens\":160}}}\n\n",
		"data: [DONE]\n\n",
	})
	defer srv.Close()
	resp, err := New(srv.URL, "m", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.InputTokens != 200 || resp.Usage.CachedInputTokens != 160 {
		t.Fatalf("usage = %+v, want in=200 cached=160", resp.Usage)
	}
}

// TestEmptyToolResultKeepsContent guards the OpenAI-compat requirement that a
// tool-role message always carries a "content" field, even when the tool produced
// no output. With json:"content,omitempty" an empty result elides the field and
// strict servers (OpenAI/vLLM/Ollama) reject the request with 400. We assert on the
// raw request body the server receives, since the typed struct hides the omission.
func TestEmptyToolResultKeepsContent(t *testing.T) {
	var rawBody string
	srv := sseServer(t, func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		rawBody = string(b)
	}, []string{
		`data: {"choices":[{"index":0,"delta":{"content":"ok"}}]}` + "\n\n",
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n",
		"data: [DONE]\n\n",
	})
	defer srv.Close()

	c := New(srv.URL, "test-model", "", 0)
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
