package gemini

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

const maliciousBody = "\n\x1b[2Kfake=record secret=sk-LEAKED-0123456789 level=error msg=\"forged\""

// sseServer returns a test server that writes the given SSE event lines, flushing
// after each so the client sees a real incremental stream. Gemini's
// :streamGenerateContent?alt=sse stream is one JSON GenerateContentResponse per
// data: event, ending at EOF (no [DONE] sentinel).
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

// TestGeminiErrorDetail asserts the error-body parser surfaces the structured
// Gemini error status/message (so 4xx causes like a bad model name or an invalid
// argument are diagnosable) while never echoing a non-JSON body and never
// emitting control characters (log-injection safe).
func TestGeminiErrorDetail(t *testing.T) {
	cases := []struct {
		name, body, want string
	}{
		{
			name: "structured 400 is surfaced",
			body: `{"error":{"code":400,"message":"Invalid JSON payload received.","status":"INVALID_ARGUMENT"}}`,
			want: ": INVALID_ARGUMENT: Invalid JSON payload received.",
		},
		{name: "non-JSON body is omitted", body: maliciousBody, want: ""},
		{name: "json without error fields is omitted", body: `{"foo":"bar"}`, want: ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := geminiErrorDetail([]byte(tc.body)); got != tc.want {
				t.Errorf("geminiErrorDetail(%q) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}

	// A message with control characters must be sanitized (no log injection).
	inj := geminiErrorDetail([]byte(`{"error":{"status":"X","message":"line1\nline2\u001b[2Kforged"}}`))
	if strings.ContainsAny(inj, "\n\r\x1b") {
		t.Errorf("detail leaked control chars: %q", inj)
	}
	// An over-long message is truncated with an ellipsis.
	long := geminiErrorDetail([]byte(`{"error":{"status":"S","message":"` + strings.Repeat("a", 500) + `"}}`))
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
		{"404 is permanent", http.StatusNotFound, true},
		{"429 is transient", http.StatusTooManyRequests, false},
		{"502 is transient", http.StatusBadGateway, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
			}))
			defer srv.Close()
			_, err := New(srv.URL, "gemini-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
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

// TestComplete drives a full Gemini SSE stream — text parts spread across chunks, a
// functionCall part, and a final chunk carrying finishReason "STOP" + usageMetadata —
// and asserts the accumulated text, the reassembled tool call, usage, the stop reason,
// and the request mapping (the request hit :streamGenerateContent with
// generationConfig.maxOutputTokens set, plus the api-key header).
func TestComplete(t *testing.T) {
	var gotReq genRequest
	var gotKey, gotPath, gotRawQuery string
	srv := sseServer(t, func(r *http.Request) {
		gotKey = r.Header.Get("x-goog-api-key")
		gotPath = r.URL.Path
		gotRawQuery = r.URL.RawQuery
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
	}, []string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"investi"}]}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"gating"}]}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"name":"what_changed","args":{"namespace":"apps"}}}]}}]}` + "\n\n",
		`data: {"candidates":[{"finishReason":"STOP","content":{"role":"model","parts":[]}}],"usageMetadata":{"promptTokenCount":140,"candidatesTokenCount":48}}` + "\n\n",
	})
	defer srv.Close()

	c := New(srv.URL, "gemini-x", "k", 16384)
	resp, err := c.Complete(context.Background(), providers.CompletionRequest{
		System:   "sys",
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
		Tools:    []providers.ToolSpec{{Name: "what_changed", Description: "d", Schema: `{"type":"object"}`}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	// auth header + streaming endpoint
	if gotKey != "k" {
		t.Fatalf("x-goog-api-key = %q", gotKey)
	}
	if !strings.HasSuffix(gotPath, "/v1beta/models/gemini-x:streamGenerateContent") {
		t.Fatalf("path = %q, want a :streamGenerateContent endpoint", gotPath)
	}
	if !strings.Contains(gotRawQuery, "alt=sse") {
		t.Fatalf("query = %q, want alt=sse", gotRawQuery)
	}
	// request mapping: generationConfig.maxOutputTokens, system_instruction, tools, contents
	if gotReq.GenerationConfig == nil || gotReq.GenerationConfig.MaxOutputTokens != 16384 {
		t.Fatalf("generationConfig (want maxOutputTokens:16384): %+v", gotReq.GenerationConfig)
	}
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
	// response accumulation: text fragments concatenate, functionCall → ToolCall,
	// usage and stop reason surface.
	if resp.Text != "investigating" {
		t.Fatalf("accumulated text = %q, want %q", resp.Text, "investigating")
	}
	if len(resp.ToolCalls) != 1 || resp.ToolCalls[0].Name != "what_changed" || resp.ToolCalls[0].Args != `{"namespace":"apps"}` {
		t.Fatalf("tool call: %+v", resp.ToolCalls)
	}
	if resp.Usage.InputTokens != 140 || resp.Usage.OutputTokens != 48 {
		t.Fatalf("usage = %+v, want in=140 out=48", resp.Usage)
	}
	if resp.StopReason != "STOP" {
		t.Fatalf("StopReason = %q, want STOP", resp.StopReason)
	}
	if resp.Truncated {
		t.Fatalf("STOP must not flag Truncated")
	}
}

// TestTruncation verifies a finishReason of "MAX_TOKENS" flags Truncated and that the
// last usageMetadata wins across chunks.
func TestTruncation(t *testing.T) {
	srv := sseServer(t, nil, []string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"cut "}]}}],"usageMetadata":{"promptTokenCount":220,"candidatesTokenCount":1}}` + "\n\n",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"off"}]}}]}` + "\n\n",
		`data: {"candidates":[{"finishReason":"MAX_TOKENS","content":{"role":"model","parts":[]}}],"usageMetadata":{"promptTokenCount":220,"candidatesTokenCount":4096}}` + "\n\n",
	})
	defer srv.Close()
	resp, err := New(srv.URL, "gemini-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "cut off" {
		t.Fatalf("text = %q, want %q", resp.Text, "cut off")
	}
	if !resp.Truncated {
		t.Fatalf("MAX_TOKENS must flag Truncated")
	}
	if resp.StopReason != "MAX_TOKENS" {
		t.Fatalf("StopReason = %q, want MAX_TOKENS", resp.StopReason)
	}
	// last usageMetadata wins
	if resp.Usage.InputTokens != 220 || resp.Usage.OutputTokens != 4096 {
		t.Fatalf("usage = %+v, want in=220 out=4096 (last wins)", resp.Usage)
	}
}

// TestRefusal verifies a safety finishReason ("SAFETY") with no content surfaces as a
// CompletionResponse that reports Refused()==true (the raw reason is carried into
// StopReason, which providers.Refused recognizes).
func TestRefusal(t *testing.T) {
	srv := sseServer(t, nil, []string{
		`data: {"candidates":[{"finishReason":"SAFETY","content":{"role":"model","parts":[]}}],"usageMetadata":{"promptTokenCount":50,"candidatesTokenCount":0}}` + "\n\n",
	})
	defer srv.Close()
	resp, err := New(srv.URL, "gemini-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if !resp.Refused() {
		t.Fatalf("want Refused()==true for finishReason SAFETY, got StopReason=%q", resp.StopReason)
	}
	if resp.Text != "" || len(resp.ToolCalls) != 0 {
		t.Fatalf("refusal must carry no content: text=%q calls=%+v", resp.Text, resp.ToolCalls)
	}
}

// TestMidStreamDrop verifies a connection dropped mid-stream (before any finishReason
// is seen) surfaces as an error rather than a silently-truncated success.
func TestMidStreamDrop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// Start emitting content but never send a finishReason; then drop the connection.
		_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"par`)
		fl.Flush()
		// Abruptly close mid-event by hijacking and closing the TCP connection.
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}))
	defer srv.Close()

	_, err := New(srv.URL, "gemini-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want an error when the stream is dropped before a finishReason")
	}
}

// TestToolResultCoalescing verifies the OpenAI-shaped exchange (assistant tool_calls
// + separate tool messages) maps to Gemini's model functionCall / coalesced user
// functionResponse form, named by the originating call.
func TestToolResultCoalescing(t *testing.T) {
	var gotReq genRequest
	srv := sseServer(t, func(r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
	}, []string{
		`data: {"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"done"}]}}]}` + "\n\n",
	})
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
	srv := sseServer(t, func(r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotReq)
	}, []string{
		`data: {"candidates":[{"finishReason":"STOP","content":{"parts":[{"text":"done"}]}}]}` + "\n\n",
	})
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

// TestUsageCachedContent asserts cachedContentTokenCount maps to CachedInputTokens
// (Gemini promptTokenCount already includes the cached subset).
func TestUsageCachedContent(t *testing.T) {
	srv := sseServer(t, nil, []string{
		"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":220,\"candidatesTokenCount\":8,\"cachedContentTokenCount\":180}}\n\n",
	})
	defer srv.Close()
	resp, err := New(srv.URL, "gemini-x", "k", 0).Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Usage.InputTokens != 220 || resp.Usage.CachedInputTokens != 180 || resp.Usage.OutputTokens != 8 {
		t.Fatalf("usage = %+v, want in=220 cached=180 out=8", resp.Usage)
	}
}

// TestRequestPrefixStable guards implicit caching: across two successive Complete calls
// for an append-only conversation, the system instruction, tools, and earlier contents
// must be byte-identical (only new turns appended), so Gemini's implicit cache hits.
func TestRequestPrefixStable(t *testing.T) {
	var bodies [][]byte
	capture := func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
	}
	events := []string{"data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"ok\"}]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":1,\"candidatesTokenCount\":1}}\n\n"}
	srv := sseServer(t, capture, events)
	defer srv.Close()
	c := New(srv.URL, "gemini-x", "k", 0)
	tools := []providers.ToolSpec{{Name: "a", Description: "d", Schema: `{"type":"object"}`}}

	// Step N
	if _, err := c.Complete(context.Background(), providers.CompletionRequest{
		System: "sys", Tools: tools,
		Messages: []providers.Message{{Role: "user", Content: "incident"}},
	}); err != nil {
		t.Fatalf("Complete N: %v", err)
	}
	// Step N+1: same prefix, one appended assistant turn + tool result
	if _, err := c.Complete(context.Background(), providers.CompletionRequest{
		System: "sys", Tools: tools,
		Messages: []providers.Message{
			{Role: "user", Content: "incident"},
			{Role: "assistant", Content: "", ToolCalls: []providers.ToolCall{{ID: "t1", Name: "a", Args: "{}"}}},
			{Role: "tool", ToolCallID: "t1", Content: "result"},
		},
	}); err != nil {
		t.Fatalf("Complete N+1: %v", err)
	}

	if len(bodies) != 2 {
		t.Fatalf("expected 2 captured request bodies, got %d", len(bodies))
	}
	var r0, r1 genRequest
	if err := json.Unmarshal(bodies[0], &r0); err != nil {
		t.Fatalf("unmarshal r0: %v", err)
	}
	if err := json.Unmarshal(bodies[1], &r1); err != nil {
		t.Fatalf("unmarshal r1: %v", err)
	}
	if !reflect.DeepEqual(r0.SystemInstruction, r1.SystemInstruction) {
		t.Fatal("system instruction must be byte-stable across steps (implicit-cache prefix)")
	}
	if !reflect.DeepEqual(r0.Tools, r1.Tools) {
		t.Fatal("tools must be byte-stable across steps (implicit-cache prefix)")
	}
	if len(r1.Contents) <= len(r0.Contents) {
		t.Fatalf("step N+1 must append contents: len0=%d len1=%d", len(r0.Contents), len(r1.Contents))
	}
	if !reflect.DeepEqual(r0.Contents, r1.Contents[:len(r0.Contents)]) {
		t.Fatal("earlier contents must be unchanged (append-only) so the prefix stays cacheable")
	}
}

// TestResponseFunctionCallID verifies the model's functionCall id (newer API) is
// carried into ToolCall.ID across the stream, and that an absent id falls back to the
// function name.
func TestResponseFunctionCallID(t *testing.T) {
	tests := []struct {
		name     string
		events   []string
		wantID   string
		wantName string
	}{
		{
			name: "id present",
			events: []string{
				`data: {"candidates":[{"content":{"parts":[{"functionCall":{"id":"call_xyz","name":"what_changed","args":{}}}]}}]}` + "\n\n",
				`data: {"candidates":[{"finishReason":"STOP","content":{"parts":[]}}]}` + "\n\n",
			},
			wantID:   "call_xyz",
			wantName: "what_changed",
		},
		{
			name: "id absent falls back to name",
			events: []string{
				`data: {"candidates":[{"content":{"parts":[{"functionCall":{"name":"what_changed","args":{}}}]}}]}` + "\n\n",
				`data: {"candidates":[{"finishReason":"STOP","content":{"parts":[]}}]}` + "\n\n",
			},
			wantID:   "what_changed",
			wantName: "what_changed",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := sseServer(t, nil, tt.events)
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
