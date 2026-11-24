// SPDX-License-Identifier: Apache-2.0

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

	_, err := New(srv.URL, "gemini-x", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
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
			_, err := New(srv.URL, "gemini-x", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
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

	c := New(srv.URL, "gemini-x", "k", 16384, "")
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

// TestToolChoice asserts CompletionRequest.ToolChoice maps to Gemini's forced
// function calling — toolConfig.functionCallingConfig with mode ANY and the tool
// name in allowedFunctionNames — and that an empty ToolChoice omits toolConfig
// entirely (provider default: AUTO).
func TestToolChoice(t *testing.T) {
	events := []string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}` + "\n\n",
	}
	cases := []struct {
		name   string
		choice string
	}{
		{"forced tool", "submit_verdicts"},
		{"empty omits toolConfig", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			srv := sseServer(t, func(r *http.Request) { body, _ = io.ReadAll(r.Body) }, events)
			defer srv.Close()

			_, err := New(srv.URL, "gemini-x", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
				Messages:   []providers.Message{{Role: "user", Content: "hi"}},
				Tools:      []providers.ToolSpec{{Name: "submit_verdicts", Description: "d", Schema: `{"type":"object"}`}},
				ToolChoice: tc.choice,
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			var got struct {
				ToolConfig *struct {
					FunctionCallingConfig struct {
						Mode                 string   `json:"mode"`
						AllowedFunctionNames []string `json:"allowedFunctionNames"`
					} `json:"functionCallingConfig"`
				} `json:"toolConfig"`
			}
			if err := json.Unmarshal(body, &got); err != nil {
				t.Fatalf("decode request body: %v", err)
			}
			if tc.choice == "" {
				if got.ToolConfig != nil {
					t.Fatalf("toolConfig must be omitted when unset, got %+v", got.ToolConfig)
				}
				if strings.Contains(string(body), "toolConfig") {
					t.Fatalf("request body must not carry a toolConfig key when unset: %s", body)
				}
				return
			}
			if got.ToolConfig == nil || got.ToolConfig.FunctionCallingConfig.Mode != "ANY" ||
				!reflect.DeepEqual(got.ToolConfig.FunctionCallingConfig.AllowedFunctionNames, []string{tc.choice}) {
				t.Fatalf(`toolConfig = %+v, want functionCallingConfig{mode:"ANY", allowedFunctionNames:[%q]}`, got.ToolConfig, tc.choice)
			}
		})
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
	resp, err := New(srv.URL, "gemini-x", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
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
	resp, err := New(srv.URL, "gemini-x", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
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

	_, err := New(srv.URL, "gemini-x", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err == nil {
		t.Fatal("want an error when the stream is dropped before a finishReason")
	}
}

// dropAfter serves a 200 stream, writes prefix, then hijacks and closes the TCP
// connection — a provider dying mid-flight, after it already reported usage.
func dropAfter(t *testing.T, prefix string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fl, ok := w.(http.Flusher)
		if !ok {
			t.Errorf("server needs http.Flusher")
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, prefix)
		fl.Flush()
		if hj, ok := w.(http.Hijacker); ok {
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
		}
	}))
}

// TestErrorPreservesObservedUsage pins that a FAILED exchange still reports the
// tokens the provider already told us it produced. Gemini resends the running
// usageMetadata totals on its chunks, so once any chunk carrying one has been
// folded the cost of the exchange is known even if the stream then dies, errors,
// or ends without a finishReason. Returning a zero CompletionResponse there
// charges a generation the provider really billed as free — internal/thread's
// per-hour chat budget and the per-investigation cost total both read Usage.
func TestErrorPreservesObservedUsage(t *testing.T) {
	const chunk = `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"partial"}]}}],"usageMetadata":{"promptTokenCount":1500,"candidatesTokenCount":800,"cachedContentTokenCount":300}}` + "\n\n"

	cases := []struct {
		name string
		srv  func(t *testing.T) *httptest.Server
	}{
		{
			name: "stream ended before a finishReason",
			srv:  func(t *testing.T) *httptest.Server { return sseServer(t, nil, []string{chunk}) },
		},
		{
			name: "connection dropped mid-stream",
			srv:  func(t *testing.T) *httptest.Server { return dropAfter(t, chunk+`data: {"candid`) },
		},
		{
			name: "provider error chunk mid-stream",
			srv: func(t *testing.T) *httptest.Server {
				return sseServer(t, nil, []string{chunk, `data: {"error":{"code":503,"status":"UNAVAILABLE","message":"overloaded"}}` + "\n\n"})
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := tc.srv(t)
			defer srv.Close()
			resp, err := New(srv.URL, "gemini-x", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
				Messages: []providers.Message{{Role: "user", Content: "hi"}},
			})
			if err == nil {
				t.Fatal("want an error for a failed exchange")
			}
			if resp.Usage.InputTokens != 1500 || resp.Usage.OutputTokens != 800 || resp.Usage.CachedInputTokens != 300 {
				t.Fatalf("usage on the error path = %+v, want in=1500 out=800 cached=300 — the provider reported these before it failed, and billed them", resp.Usage)
			}
		})
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

	_, err := New(srv.URL, "gemini-x", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
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

	_, err := New(srv.URL, "gemini-x", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
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
	resp, err := New(srv.URL, "gemini-x", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
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
	c := New(srv.URL, "gemini-x", "k", 0, "")
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
			resp, err := New(srv.URL, "gemini-x", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
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

// TestEffortSendsThinkingLevel verifies effort maps uppercase to
// generationConfig.thinkingConfig.thinkingLevel and that empty effort omits
// thinkingConfig entirely.
func TestEffortSendsThinkingLevel(t *testing.T) {
	events := []string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}` + "\n\n",
	}
	cases := []struct {
		name      string
		effort    string
		wantLevel string
	}{
		{"high maps uppercase", "high", "HIGH"},
		{"xhigh maps uppercase", "xhigh", "XHIGH"},
		{"empty omits thinkingConfig", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			var gotReq genRequest
			srv := sseServer(t, func(r *http.Request) {
				body, _ = io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &gotReq)
			}, events)
			defer srv.Close()

			_, err := New(srv.URL, "gemini-4-neon", "k", 0, tc.effort).Complete(context.Background(), providers.CompletionRequest{
				Messages: []providers.Message{{Role: "user", Content: "hi"}},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if tc.wantLevel == "" {
				if gotReq.GenerationConfig == nil || gotReq.GenerationConfig.ThinkingConfig != nil {
					t.Fatalf("thinkingConfig must be nil when effort is empty, got %+v", gotReq.GenerationConfig)
				}
				if strings.Contains(string(body), "thinkingConfig") {
					t.Fatalf("request body must omit thinkingConfig when effort is empty: %s", body)
				}
				return
			}
			if gotReq.GenerationConfig == nil || gotReq.GenerationConfig.ThinkingConfig == nil ||
				gotReq.GenerationConfig.ThinkingConfig.ThinkingLevel != tc.wantLevel {
				t.Fatalf("thinkingConfig = %+v, want thinkingLevel %q", gotReq.GenerationConfig, tc.wantLevel)
			}
		})
	}
}

// TestUsageIncludesThoughtsTokenCount asserts thoughtsTokenCount is folded into
// Usage.OutputTokens alongside candidatesTokenCount.
func TestUsageIncludesThoughtsTokenCount(t *testing.T) {
	srv := sseServer(t, nil, []string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":200,"candidatesTokenCount":50,"thoughtsTokenCount":350,"cachedContentTokenCount":120}}` + "\n\n",
	})
	defer srv.Close()

	resp, err := New(srv.URL, "gemini-4-neon", "k", 0, "high").Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	want := providers.Usage{InputTokens: 200, OutputTokens: 400, CachedInputTokens: 120}
	if resp.Usage != want {
		t.Fatalf("usage = %+v, want %+v", resp.Usage, want)
	}
}

// TestThoughtSignatureMultiTurnReplay verifies a thoughtSignature on a turn-1
// functionCall is captured into resp.Opaque and replayed verbatim as a sibling of
// functionCall on turn 2, including when ToolChoice forces a function call.
func TestThoughtSignatureMultiTurnReplay(t *testing.T) {
	var turn int
	var turn2Body []byte
	var turn2Req genRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		b, _ := io.ReadAll(r.Body)
		if turn == 2 {
			turn2Body = b
			_ = json.Unmarshal(b, &turn2Req)
		}
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		if turn == 1 {
			_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_1","name":"what_changed","args":{"ns":"apps"}},"thoughtSignature":"sig-turn-1"}]},"finishReason":"STOP"}]}`+"\n\n")
		} else {
			_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"call_2","name":"submit_findings","args":{"summary":"ok"}}}]},"finishReason":"STOP"}]}`+"\n\n")
		}
		fl.Flush()
	}))
	defer srv.Close()

	c := New(srv.URL, "gemini-4-neon", "k", 0, "high")
	tools := []providers.ToolSpec{
		{Name: "what_changed", Description: "d", Schema: `{"type":"object"}`},
		{Name: "submit_findings", Description: "d", Schema: `{"type":"object"}`},
	}

	resp1, err := c.Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "investigate"}},
		Tools:    tools,
	})
	if err != nil {
		t.Fatalf("turn 1 Complete: %v", err)
	}
	if len(resp1.Opaque) == 0 {
		t.Fatal("want non-empty Opaque when response carries a thoughtSignature")
	}
	var env struct {
		V        int    `json:"v"`
		Provider string `json:"provider"`
		Parts    []part `json:"parts"`
	}
	if err := json.Unmarshal(resp1.Opaque, &env); err != nil {
		t.Fatalf("unmarshal Opaque: %v", err)
	}
	if env.V != 1 || env.Provider != "gemini" || len(env.Parts) != 1 || env.Parts[0].ThoughtSignature != "sig-turn-1" {
		t.Fatalf("unexpected Opaque envelope: %+v", env)
	}

	_, err = c.Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: "user", Content: "investigate"},
			{Role: "assistant", Content: resp1.Text, ToolCalls: resp1.ToolCalls, Opaque: resp1.Opaque},
			{Role: "tool", ToolCallID: resp1.ToolCalls[0].ID, Content: "rollout changed"},
		},
		Tools:      tools,
		ToolChoice: "submit_findings",
	})
	if err != nil {
		t.Fatalf("turn 2 Complete: %v", err)
	}
	if len(turn2Req.Contents) != 3 {
		t.Fatalf("turn 2 contents len = %d, want 3: %s", len(turn2Req.Contents), turn2Body)
	}
	modelTurn := turn2Req.Contents[1]
	if modelTurn.Role != "model" || len(modelTurn.Parts) != 1 ||
		modelTurn.Parts[0].FunctionCall == nil ||
		modelTurn.Parts[0].FunctionCall.Name != "what_changed" ||
		modelTurn.Parts[0].ThoughtSignature != "sig-turn-1" {
		t.Fatalf("turn 2 model turn did not replay thoughtSignature verbatim: %+v (body: %s)", modelTurn, turn2Body)
	}
	if turn2Req.ToolConfig == nil || turn2Req.ToolConfig.FunctionCallingConfig.Mode != "ANY" {
		t.Fatalf("turn 2 toolConfig = %+v, want ANY", turn2Req.ToolConfig)
	}
	if turn2Req.GenerationConfig == nil || turn2Req.GenerationConfig.ThinkingConfig == nil ||
		turn2Req.GenerationConfig.ThinkingConfig.ThinkingLevel != "HIGH" {
		t.Fatalf("turn 2 thinkingConfig = %+v, want HIGH", turn2Req.GenerationConfig)
	}
}

// TestParallelFunctionCallsPreserveFirstSignatureOnly verifies that when only the
// first functionCall in a parallel turn carries a thoughtSignature, the signature
// stays on parts[0] only (never duplicated onto sibling calls) and both
// functionResponses correlate properly.
func TestParallelFunctionCallsPreserveFirstSignatureOnly(t *testing.T) {
	var turn int
	var turn2Req genRequest

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		turn++
		if turn == 2 {
			_ = json.NewDecoder(r.Body).Decode(&turn2Req)
		}
		fl := w.(http.Flusher)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		fl.Flush()
		if turn == 1 {
			_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"functionCall":{"id":"c1","name":"get_status","args":{"ns":"a"}},"thoughtSignature":"sig-par-1"},{"functionCall":{"id":"c2","name":"get_status","args":{"ns":"b"}}}]},"finishReason":"STOP"}]}`+"\n\n")
		} else {
			_, _ = io.WriteString(w, `data: {"candidates":[{"content":{"role":"model","parts":[{"text":"done"}]},"finishReason":"STOP"}]}`+"\n\n")
		}
		fl.Flush()
	}))
	defer srv.Close()

	c := New(srv.URL, "gemini-4-neon", "k", 0, "")
	resp1, err := c.Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "check both"}},
	})
	if err != nil {
		t.Fatalf("turn 1 Complete: %v", err)
	}
	if len(resp1.ToolCalls) != 2 {
		t.Fatalf("want 2 tool calls, got %+v", resp1.ToolCalls)
	}

	_, err = c.Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{
			{Role: "user", Content: "check both"},
			{Role: "assistant", Content: resp1.Text, ToolCalls: resp1.ToolCalls, Opaque: resp1.Opaque},
			{Role: "tool", ToolCallID: "c1", Content: "a ok"},
			{Role: "tool", ToolCallID: "c2", Content: "b failing"},
		},
	})
	if err != nil {
		t.Fatalf("turn 2 Complete: %v", err)
	}
	modelParts := turn2Req.Contents[1].Parts
	if len(modelParts) != 2 {
		t.Fatalf("want 2 model parts, got %+v", modelParts)
	}
	if modelParts[0].ThoughtSignature != "sig-par-1" {
		t.Fatalf("parts[0].ThoughtSignature = %q, want sig-par-1", modelParts[0].ThoughtSignature)
	}
	if modelParts[1].ThoughtSignature != "" {
		t.Fatalf("parts[1].ThoughtSignature = %q, want empty (must not duplicate signature)", modelParts[1].ThoughtSignature)
	}
	resParts := turn2Req.Contents[2].Parts
	if len(resParts) != 2 ||
		resParts[0].FunctionResponse == nil || resParts[0].FunctionResponse.ID != "c1" || resParts[0].FunctionResponse.Name != "get_status" ||
		resParts[1].FunctionResponse == nil || resParts[1].FunctionResponse.ID != "c2" || resParts[1].FunctionResponse.Name != "get_status" {
		t.Fatalf("functionResponse correlation wrong: %+v", resParts)
	}
}

// TestThoughtPartsFilteredAndSignedRetained verifies thought:true parts never leak
// into resp.Text, unsigned thought:true parts are dropped from Opaque, signed
// thought:true parts are retained verbatim, adjacent unsigned text deltas coalesce,
// and trailing empty-text parts carrying a thoughtSignature are retained.
func TestThoughtPartsFilteredAndSignedRetained(t *testing.T) {
	srv := sseServer(t, nil, []string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"internal scratchpad"}]}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"thought":true,"text":"signed scratch","thoughtSignature":"sig-t"}]}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"Hello "}]}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"world"}]}}]}` + "\n\n",
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"","thoughtSignature":"sig-end"}]},"finishReason":"STOP"}]}` + "\n\n",
	})
	defer srv.Close()

	resp, err := New(srv.URL, "gemini-4-neon", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Text != "Hello world" {
		t.Fatalf("resp.Text = %q, want %q", resp.Text, "Hello world")
	}
	parts, ok := decodeOpaqueParts(resp.Opaque)
	if !ok {
		t.Fatalf("expected valid Gemini Opaque envelope, got %s", resp.Opaque)
	}
	want := []part{
		{Text: "signed scratch", Thought: true, ThoughtSignature: "sig-t"},
		{Text: "Hello world"},
		{Text: "", ThoughtSignature: "sig-end"},
	}
	if !reflect.DeepEqual(parts, want) {
		t.Fatalf("decoded Opaque parts =\n  %+v\nwant\n  %+v", parts, want)
	}
}

// TestNoSignatureLeavesOpaqueNil verifies Gemini 2.5 responses (plain text +
// unsigned functionCall) keep resp.Opaque == nil.
func TestNoSignatureLeavesOpaqueNil(t *testing.T) {
	srv := sseServer(t, nil, []string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"checking"},{"functionCall":{"name":"what_changed","args":{"ns":"apps"}}}]},"finishReason":"STOP"}]}` + "\n\n",
	})
	defer srv.Close()

	resp, err := New(srv.URL, "gemini-2.5-flash-lite", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
		Messages: []providers.Message{{Role: "user", Content: "hi"}},
	})
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if resp.Opaque != nil {
		t.Fatalf("want nil Opaque when no thoughtSignature is present, got %s", resp.Opaque)
	}
}

// TestForeignOrCorruptOpaqueFallsBackCleanly verifies foreign (e.g. Anthropic) or
// malformed Opaque payloads fall back cleanly to synthesizing parts from Content +
// ToolCalls, with no skip_thought_signature_validator sentinel emitted.
func TestForeignOrCorruptOpaqueFallsBackCleanly(t *testing.T) {
	cases := []struct {
		name   string
		opaque json.RawMessage
	}{
		{"anthropic thinking array", json.RawMessage(`[{"type":"thinking","thinking":"x","signature":"s"}]`)},
		{"wrong version", json.RawMessage(`{"v":2,"provider":"gemini","parts":[{"text":"x"}]}`)},
		{"wrong provider", json.RawMessage(`{"v":1,"provider":"openai","parts":[{"text":"x"}]}`)},
		{"corrupt json", json.RawMessage(`{corrupt`)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var body []byte
			var gotReq genRequest
			srv := sseServer(t, func(r *http.Request) {
				body, _ = io.ReadAll(r.Body)
				_ = json.Unmarshal(body, &gotReq)
			}, []string{
				`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}` + "\n\n",
			})
			defer srv.Close()

			_, err := New(srv.URL, "gemini-4-neon", "k", 0, "").Complete(context.Background(), providers.CompletionRequest{
				Messages: []providers.Message{
					{Role: "user", Content: "hi"},
					{
						Role:      "assistant",
						Content:   "checking",
						ToolCalls: []providers.ToolCall{{ID: "c1", Name: "what_changed", Args: `{"ns":"apps"}`}},
						Opaque:    tc.opaque,
					},
					{Role: "tool", ToolCallID: "c1", Content: "done"},
				},
			})
			if err != nil {
				t.Fatalf("Complete: %v", err)
			}
			if strings.Contains(string(body), "skip_thought_signature_validator") {
				t.Fatalf("fallback must never emit skip_thought_signature_validator: %s", body)
			}
			modelParts := gotReq.Contents[1].Parts
			if len(modelParts) != 2 || modelParts[0].Text != "checking" ||
				modelParts[1].FunctionCall == nil || modelParts[1].FunctionCall.Name != "what_changed" {
				t.Fatalf("unexpected fallback parts: %+v", modelParts)
			}
		})
	}
}

// TestRequestPrefixStableWithThoughtSignature guards Gemini's implicit prefix cache
// across multi-turn conversations carrying signed Opaque assistant turns.
func TestRequestPrefixStableWithThoughtSignature(t *testing.T) {
	var bodies [][]byte
	capture := func(r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		bodies = append(bodies, b)
	}
	events := []string{
		`data: {"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}` + "\n\n",
	}
	srv := sseServer(t, capture, events)
	defer srv.Close()

	c := New(srv.URL, "gemini-4-neon", "k", 0, "high")
	tools := []providers.ToolSpec{{Name: "a", Description: "d", Schema: `{"type":"object"}`}}
	opaque1 := json.RawMessage(`{"v":1,"provider":"gemini","parts":[{"functionCall":{"id":"t1","name":"a","args":{"k":"v"}},"thoughtSignature":"sig-1"}]}`)
	opaque2 := json.RawMessage(`{"v":1,"provider":"gemini","parts":[{"functionCall":{"id":"t2","name":"a","args":{"k":"v2"}},"thoughtSignature":"sig-2"}]}`)

	turn2Msgs := []providers.Message{
		{Role: "user", Content: "incident"},
		{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "t1", Name: "a", Args: `{"k":"v"}`}}, Opaque: opaque1},
		{Role: "tool", ToolCallID: "t1", Content: "res1"},
	}
	turn3Msgs := append(append([]providers.Message(nil), turn2Msgs...),
		providers.Message{Role: "assistant", ToolCalls: []providers.ToolCall{{ID: "t2", Name: "a", Args: `{"k":"v2"}`}}, Opaque: opaque2},
		providers.Message{Role: "tool", ToolCallID: "t2", Content: "res2"},
	)

	if _, err := c.Complete(context.Background(), providers.CompletionRequest{
		System: "sys", Tools: tools, Messages: turn2Msgs,
	}); err != nil {
		t.Fatalf("turn 2 Complete: %v", err)
	}
	if _, err := c.Complete(context.Background(), providers.CompletionRequest{
		System: "sys", Tools: tools, Messages: turn3Msgs,
	}); err != nil {
		t.Fatalf("turn 3 Complete: %v", err)
	}

	var r2, r3 genRequest
	if err := json.Unmarshal(bodies[0], &r2); err != nil {
		t.Fatalf("unmarshal r2: %v", err)
	}
	if err := json.Unmarshal(bodies[1], &r3); err != nil {
		t.Fatalf("unmarshal r3: %v", err)
	}
	b2Prefix, _ := json.Marshal(r2.Contents)
	b3Prefix, _ := json.Marshal(r3.Contents[:len(r2.Contents)])
	if string(b2Prefix) != string(b3Prefix) {
		t.Fatalf("signed Opaque prefix drifted across turns:\n  turn2: %s\n  turn3: %s", b2Prefix, b3Prefix)
	}
}
