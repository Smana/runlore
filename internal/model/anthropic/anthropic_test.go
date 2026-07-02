package anthropic

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
// excludes the upstream body but includes the status and request-id.
func TestNon2xxErrorOmitsBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Request-Id", "req-abc-123")
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte(maliciousBody))
	}))
	defer srv.Close()

	_, err := New(srv.URL, "claude-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
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

// TestAnthropicErrorDetail asserts the error-body parser surfaces the structured
// Anthropic error type/message (so 4xx causes like "prompt is too long" are
// diagnosable) while never echoing a non-JSON body and never emitting control
// characters (log-injection safe).
func TestAnthropicErrorDetail(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{
			name: "structured 400 is surfaced",
			body: `{"type":"error","error":{"type":"invalid_request_error","message":"prompt is too long: 210000 tokens > 200000 maximum"}}`,
			want: ": invalid_request_error: prompt is too long: 210000 tokens > 200000 maximum",
		},
		{name: "non-JSON body is omitted", body: maliciousBody, want: ""},
		{name: "json without error fields is omitted", body: `{"foo":"bar"}`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := anthropicErrorDetail([]byte(tc.body)); got != tc.want {
				t.Errorf("anthropicErrorDetail(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}

	// A message with control characters must be sanitized (no log injection).
	inj := anthropicErrorDetail([]byte(`{"error":{"type":"x","message":"line1\nline2\u001b[2Kforged"}}`))
	if strings.ContainsAny(inj, "\n\r\x1b") {
		t.Errorf("detail leaked control chars: %q", inj)
	}
	// An over-long message is truncated with an ellipsis.
	long := anthropicErrorDetail([]byte(`{"error":{"type":"t","message":"` + strings.Repeat("a", 500) + `"}}`))
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
		{"403 is permanent", http.StatusForbidden, true},
		{"429 is transient", http.StatusTooManyRequests, false},
		{"502 is transient", http.StatusBadGateway, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()
			_, err := New(srv.URL, "claude-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
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

// TestComplete drives a full Anthropic SSE stream — message_start (input usage),
// a text content block, a tool_use block assembled from input_json_delta fragments,
// content_block_stop, message_delta (stop_reason + output usage), message_stop — and
// asserts the accumulated text, the reassembled tool call, and the request mapping
// (stream:true + max_tokens + headers + prompt-cache breakpoints).
func TestComplete(t *testing.T) {
	var gotReq msgRequest
	var gotVersion, gotKey string
	srv := sseServer(t, func(r *http.Request) {
		gotVersion, gotKey = r.Header.Get("anthropic-version"), r.Header.Get("x-api-key")
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
	}, []string{
		`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":120,"output_tokens":1}}}

`,
		`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"investi"}}

`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"gating"}}

`,
		`event: content_block_stop
data: {"type":"content_block_stop","index":0}

`,
		`event: content_block_start
data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"tu1","name":"what_changed","input":{}}}

`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"namespace\":"}}

`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"\"apps\"}"}}

`,
		`event: content_block_stop
data: {"type":"content_block_stop","index":1}

`,
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":45}}

`,
		`event: message_stop
data: {"type":"message_stop"}

`,
	})
	defer srv.Close()

	c := New(srv.URL, "claude-x", "k", 16384)
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
	if gotReq.Model != "claude-x" || !gotReq.Stream || gotReq.MaxTokens != 16384 {
		t.Fatalf("request (want stream:true, max_tokens:16384): %+v", gotReq)
	}
	// system is sent as a content-block array carrying a prompt-cache breakpoint
	if len(gotReq.System) != 1 || gotReq.System[0].Text != "sys" ||
		gotReq.System[0].CacheControl == nil || gotReq.System[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("system (want one cached 'sys' block): %+v", gotReq.System)
	}
	if len(gotReq.Tools) != 1 || gotReq.Tools[0].Name != "what_changed" || string(gotReq.Tools[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("tools: %+v", gotReq.Tools)
	}
	if gotReq.Tools[0].CacheControl == nil || gotReq.Tools[0].CacheControl.Type != "ephemeral" {
		t.Fatalf("last tool should be a cache breakpoint: %+v", gotReq.Tools[0])
	}
	if len(gotReq.Messages) != 1 || gotReq.Messages[0].Role != "user" || gotReq.Messages[0].Content[0].Text != "hi" {
		t.Fatalf("messages: %+v", gotReq.Messages)
	}
	// response accumulation: text + reassembled tool_use args + usage + stop reason
	if resp.Text != "investigating" {
		t.Fatalf("accumulated text = %q, want %q", resp.Text, "investigating")
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].ID != "tu1" || resp.ToolCalls[0].Name != "what_changed" ||
		resp.ToolCalls[0].Args != `{"namespace":"apps"}` {
		t.Fatalf("tool call: %+v", resp.ToolCalls)
	}
	if resp.Usage.InputTokens != 120 || resp.Usage.OutputTokens != 45 {
		t.Fatalf("usage = %+v, want in=120 out=45", resp.Usage)
	}
	if resp.StopReason != "tool_use" {
		t.Fatalf("StopReason = %q, want tool_use", resp.StopReason)
	}
}

// TestUsageAndStopReason verifies usage accumulation and stop_reason mapping over a
// minimal SSE stream: a "max_tokens" stop_reason flags Truncated; "end_turn" does not.
func TestUsageAndStopReason(t *testing.T) {
	tests := []struct {
		name          string
		stopReason    string
		wantTruncated bool
	}{
		{"end_turn (not truncated)", "end_turn", false},
		{"max_tokens flags truncation", "max_tokens", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := sseServer(t, nil, []string{
				`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":200}}}

`,
				`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`,
				`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}

`,
				`event: content_block_stop
data: {"type":"content_block_stop","index":0}

`,
				`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"` + tt.stopReason + `"},"usage":{"output_tokens":4096}}

`,
				`event: message_stop
data: {"type":"message_stop"}

`,
			})
			defer srv.Close()
			resp, err := New(srv.URL, "claude-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
				Messages: []providers.Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if resp.Usage.InputTokens != 200 || resp.Usage.OutputTokens != 4096 {
				t.Fatalf("usage = %+v, want in=200 out=4096", resp.Usage)
			}
			if resp.Text != "done" {
				t.Fatalf("text = %q, want done", resp.Text)
			}
			if resp.Truncated != tt.wantTruncated {
				t.Fatalf("Truncated = %v, want %v", resp.Truncated, tt.wantTruncated)
			}
			if resp.StopReason != tt.stopReason {
				t.Fatalf("StopReason = %q, want %q", resp.StopReason, tt.stopReason)
			}
		})
	}
}

// TestRefusal verifies a stop_reason of "refusal" with no content surfaces as a
// CompletionResponse that reports Refused()==true.
func TestRefusal(t *testing.T) {
	srv := sseServer(t, nil, []string{
		`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":50}}}

`,
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"refusal"},"usage":{"output_tokens":0}}

`,
		`event: message_stop
data: {"type":"message_stop"}

`,
	})
	defer srv.Close()
	resp, err := New(srv.URL, "claude-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !resp.Refused() {
		t.Fatalf("want Refused()==true for stop_reason refusal, got StopReason=%q", resp.StopReason)
	}
	if resp.Text != "" || len(resp.ToolCalls) != 0 {
		t.Fatalf("refusal must carry no content: text=%q calls=%+v", resp.Text, resp.ToolCalls)
	}
}

// TestMidStreamDrop verifies a connection dropped mid-stream (before message_stop)
// surfaces as an error rather than a silently-truncated success.
func TestMidStreamDrop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Start a message but never finish it; then drop the connection.
		_, _ = io.WriteString(w, `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"par`)
		fl.Flush()
		// Abruptly close mid-event by hijacking and closing the TCP connection.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	_, err := New(srv.URL, "claude-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want an error when the stream is dropped before message_stop")
	}
}

// TestMessageCoalescing verifies the OpenAI-shaped exchange (assistant tool_calls +
// separate tool messages) maps to Anthropic's tool_use / coalesced tool_result form.
func TestMessageCoalescing(t *testing.T) {
	var gotReq msgRequest
	srv := sseServer(t, func(r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
	}, []string{
		`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}

`,
		`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"done"}}

`,
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

`,
		`event: message_stop
data: {"type":"message_stop"}

`,
	})
	defer srv.Close()

	_, err := New(srv.URL, "claude-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
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

// TestPromptCacheHistoryBreakpoint asserts the rolling breakpoint: the last content
// block of the last message carries cache_control, alongside system + last tool.
func TestPromptCacheHistoryBreakpoint(t *testing.T) {
	var gotReq msgRequest
	srv := sseServer(t, func(r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
	}, []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":1}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	})
	defer srv.Close()

	_, err := New(srv.URL, "claude-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		System: "sys",
		Messages: []providers.Message{
			{Role: "user", Content: "incident"},
			{Role: "assistant", Content: "thinking", ToolCalls: []providers.ToolCall{{ID: "t1", Name: "a", Args: "{}"}}},
			{Role: "tool", ToolCallID: "t1", Content: "result"},
		},
		Tools: []providers.ToolSpec{{Name: "a", Description: "d", Schema: `{"type":"object"}`}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// system marked
	if len(gotReq.System) == 0 || gotReq.System[0].CacheControl == nil {
		t.Fatalf("system block should be a cache breakpoint: %+v", gotReq.System)
	}
	// last tool marked
	lt := gotReq.Tools[len(gotReq.Tools)-1]
	if lt.CacheControl == nil || lt.CacheControl.Type != "ephemeral" {
		t.Fatalf("last tool should be a cache breakpoint: %+v", lt)
	}
	// last block of the last message marked (the rolling breakpoint)
	last := gotReq.Messages[len(gotReq.Messages)-1]
	lb := last.Content[len(last.Content)-1]
	if lb.CacheControl == nil || lb.CacheControl.Type != "ephemeral" {
		t.Fatalf("last message's last block should be the rolling cache breakpoint: %+v", last)
	}
	// an earlier message block must NOT be marked
	if gotReq.Messages[0].Content[0].CacheControl != nil {
		t.Fatalf("earlier message blocks must not be marked: %+v", gotReq.Messages[0])
	}
	// exactly one message-level breakpoint (≤4 total is enforced; message portion must be 1)
	msgBreakpoints := 0
	for _, m := range gotReq.Messages {
		for _, b := range m.Content {
			if b.CacheControl != nil {
				msgBreakpoints++
			}
		}
	}
	if msgBreakpoints != 1 {
		t.Fatalf("exactly one message-level cache breakpoint expected, got %d", msgBreakpoints)
	}
}

// TestUsageCacheFields asserts the Anthropic usage normalization: InputTokens is the
// sum of input + cache_read + cache_creation; the read/creation subsets are reported.
func TestUsageCacheFields(t *testing.T) {
	srv := sseServer(t, nil, []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":30,\"cache_read_input_tokens\":100,\"cache_creation_input_tokens\":20}}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":7}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	})
	defer srv.Close()
	resp, err := New(srv.URL, "claude-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.InputTokens != 150 || resp.Usage.CachedInputTokens != 100 || resp.Usage.CacheWriteTokens != 20 {
		t.Fatalf("usage = %+v, want in=150 cached=100 write=20", resp.Usage)
	}
}

// TestToolChoice asserts CompletionRequest.ToolChoice maps to Anthropic's forced
// tool_choice — {"type":"tool","name":"<name>"} — and that an empty ToolChoice
// omits the field entirely (provider default: auto).
func TestToolChoice(t *testing.T) {
	events := []string{
		"event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":1}}}\n\n",
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":1}}\n\n",
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n",
	}
	cases := []struct {
		name   string
		choice string
	}{
		{"forced tool", "submit_verdicts"},
		{"empty omits the field", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			srv := sseServer(t, func(r *http.Request) { body, _ = io.ReadAll(r.Body) }, events)
			defer srv.Close()

			_, err := New(srv.URL, "claude-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
				Messages:   []providers.Message{{Role: "user", Content: "hi"}},
				Tools:      []providers.ToolSpec{{Name: "submit_verdicts", Description: "d", Schema: `{"type":"object"}`}},
				ToolChoice: tc.choice,
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			var got struct {
				ToolChoice *struct {
					Type string `json:"type"`
					Name string `json:"name"`
				} `json:"tool_choice"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if tc.choice == "" {
				if got.ToolChoice != nil {
					t.Fatalf("tool_choice must be omitted when unset, got %+v", got.ToolChoice)
				}
				if strings.Contains(string(body), "tool_choice") {
					t.Fatalf("request body must not carry a tool_choice key when unset: %s", body)
				}
				return
			}
			if got.ToolChoice == nil || got.ToolChoice.Type != "tool" || got.ToolChoice.Name != tc.choice {
				t.Fatalf(`tool_choice = %+v, want {"type":"tool","name":%q}`, got.ToolChoice, tc.choice)
			}
		})
	}
}

// TestPromptCacheToolsOnly verifies that with no system prompt, the tools array
// still gets exactly one cache breakpoint — on the LAST tool, not every tool.
func TestPromptCacheToolsOnly(t *testing.T) {
	var gotReq msgRequest
	srv := sseServer(t, func(r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
	}, []string{
		`event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":1}}}

`,
		`event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

`,
		`event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"ok"}}

`,
		`event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":1}}

`,
		`event: message_stop
data: {"type":"message_stop"}

`,
	})
	defer srv.Close()

	_, err := New(srv.URL, "claude-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
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
