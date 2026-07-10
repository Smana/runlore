// SPDX-License-Identifier: Apache-2.0

// Package gemini implements providers.ModelProvider against the Gemini API
// (streamGenerateContent with native function calling). It maps RunLore's engine-
// agnostic exchange (OpenAI-shaped: assistant tool_calls + separate tool messages)
// onto Gemini's contents/parts form: assistant turns become role "model" with
// functionCall parts, and tool results coalesce into one role "user" turn of
// functionResponse parts. Each call/response carries the originating call id, so
// parallel calls to the same function name correlate by id (Gemini's rule for
// parallel calls); when the model returns no id, correlation falls back to name.
//
// Streaming is internal: callers see the same one-shot Complete interface, but the
// request uses :streamGenerateContent?alt=sse and the SSE chunks are accumulated
// into a single CompletionResponse. Consuming the stream avoids a flat request
// deadline truncating a long completion.
package gemini

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/Smana/runlore/internal/model/clientcore"
	"github.com/Smana/runlore/internal/providers"
)

// DefaultBaseURL is the public Gemini API.
const DefaultBaseURL = "https://generativelanguage.googleapis.com"

// Caching: RunLore relies on Gemini's automatic IMPLICIT prefix caching (enabled on
// Gemini 2.5+). No explicit CachedContent lifecycle is used. This depends on the request
// prefix (system_instruction + tools + earlier contents) being byte-stable and append-only
// across the loop's steps; TestRequestPrefixStable guards that invariant.

// Client is a Gemini (streamGenerateContent) model provider.
type Client struct {
	clientcore.Base
}

// New builds a client. baseURL may be empty (defaults to DefaultBaseURL).
// maxTokens caps output tokens per request; <= 0 falls back to
// clientcore.DefaultMaxTokens.
func New(baseURL, model, apiKey string, maxTokens int) *Client {
	return &Client{Base: clientcore.NewBase(baseURL, DefaultBaseURL, model, apiKey, maxTokens)}
}

var _ providers.ModelProvider = (*Client)(nil)

type genRequest struct {
	SystemInstruction *content          `json:"system_instruction,omitempty"`
	Contents          []content         `json:"contents"`
	Tools             []tool            `json:"tools,omitempty"`
	ToolConfig        *toolConfig       `json:"toolConfig,omitempty"`
	GenerationConfig  *generationConfig `json:"generationConfig,omitempty"`
}

// toolConfig carries Gemini's function-calling controls. RunLore only emits it to
// FORCE a tool: mode ANY restricted to a single allowed function name. Omitted
// (nil) = AUTO: the model chooses freely between prose and any declared function.
// It is only ever set on single-shot structured-output requests, so the loop's
// append-only prefix stability (implicit caching) is unaffected.
type toolConfig struct {
	FunctionCallingConfig functionCallingConfig `json:"functionCallingConfig"`
}

type functionCallingConfig struct {
	Mode                 string   `json:"mode"` // "ANY" = the model MUST call a function
	AllowedFunctionNames []string `json:"allowedFunctionNames,omitempty"`
}

// generationConfig carries per-request decoding controls. MaxOutputTokens caps the
// output tokens; without it Gemini applies the model's (often small) default and a
// long completion is silently cut off (a MAX_TOKENS truncation).
type generationConfig struct {
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
}

type content struct {
	Role  string `json:"role,omitempty"`
	Parts []part `json:"parts"`
}

type part struct {
	Text             string            `json:"text,omitempty"`
	FunctionCall     *functionCall     `json:"functionCall,omitempty"`
	FunctionResponse *functionResponse `json:"functionResponse,omitempty"`
}

type functionCall struct {
	ID   string          `json:"id,omitempty"` // call id; correlates parallel same-name calls
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type functionResponse struct {
	ID       string          `json:"id,omitempty"` // echoes the originating functionCall id
	Name     string          `json:"name"`
	Response json.RawMessage `json:"response"`
}

type tool struct {
	FunctionDeclarations []functionDeclaration `json:"functionDeclarations"`
}

type functionDeclaration struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
}

// genResponse is one GenerateContentResponse — the shape of both a full response and
// each SSE stream chunk (?alt=sse emits one per data: event). Only the fields RunLore
// accumulates are decoded.
type genResponse struct {
	Candidates []struct {
		Content content `json:"content"`
		// FinishReason is the candidate-termination reason; "MAX_TOKENS" marks an output
		// cut off at the token ceiling (a truncated, not complete, answer). Safety stops
		// (SAFETY, PROHIBITED_CONTENT, BLOCKLIST, SPII) surface raw via StopReason, which
		// providers.Refused recognizes. It is empty on intermediate stream chunks.
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	// UsageMetadata carries the per-request token counts; a pointer so an absent block
	// parses to nil (unknown) rather than a misleading {0,0}.
	UsageMetadata *struct {
		PromptTokenCount        int `json:"promptTokenCount"`
		CandidatesTokenCount    int `json:"candidatesTokenCount"`
		CachedContentTokenCount int `json:"cachedContentTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends a streaming streamGenerateContent request with tools and accumulates
// the full SSE response into a single CompletionResponse. Streaming is internal —
// callers see the same one-shot interface; consuming the stream avoids the flat
// request deadline truncating a long completion.
func (c *Client) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	greq := genRequest{
		Contents:         toContents(req.Messages),
		GenerationConfig: &generationConfig{MaxOutputTokens: c.MaxTokens},
	}
	if req.System != "" {
		greq.SystemInstruction = &content{Parts: []part{{Text: req.System}}}
	}
	if len(req.Tools) > 0 {
		decls := make([]functionDeclaration, 0, len(req.Tools))
		for _, t := range req.Tools {
			decls = append(decls, functionDeclaration{Name: t.Name, Description: t.Description, Parameters: json.RawMessage(t.Schema)})
		}
		greq.Tools = []tool{{FunctionDeclarations: decls}}
	}
	if req.ToolChoice != "" {
		greq.ToolConfig = &toolConfig{FunctionCallingConfig: functionCallingConfig{
			Mode: "ANY", AllowedFunctionNames: []string{req.ToolChoice},
		}}
	}
	return c.Stream(ctx, clientcore.Request{
		Op: "streamGenerateContent",
		// ?alt=sse switches streamGenerateContent from a JSON array to a Server-Sent
		// Events stream: one GenerateContentResponse per data: event, ending at EOF.
		URL:  fmt.Sprintf("%s/v1beta/models/%s:streamGenerateContent?alt=sse", c.BaseURL, c.Model),
		Body: greq,
		SetHeaders: func(r *http.Request) {
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("x-goog-api-key", c.APIKey)
		},
		ErrorDetail: geminiErrorDetail,
	}, accumulate)
}

// geminiErrorDetail extracts a sanitized ": status: message" suffix from a Gemini
// error response body ({"error":{"code","message","status"}}), or "" if the body
// can't be parsed as one. Only the structured error.status / error.message fields
// are surfaced (never the raw bytes); sanitization and truncation live in
// clientcore.Detail.
func geminiErrorDetail(body []byte) string {
	var e struct {
		Error struct {
			Status  string `json:"status"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || (e.Error.Status == "" && e.Error.Message == "") {
		return ""
	}
	return clientcore.Detail(e.Error.Status, e.Error.Message)
}

// accumulate consumes a Gemini SSE stream and folds it into one CompletionResponse:
// text parts concatenate into Text, functionCall parts become ToolCalls (id-correlated,
// name fallback), usageMetadata is taken last-one-wins, and a candidate finishReason
// maps to StopReason (and Truncated when "MAX_TOKENS"; safety reasons like "SAFETY"
// surface raw so providers.Refused recognizes them). A read error, or a stream that
// ends before any finishReason (a mid-stream drop), is an error.
func accumulate(r io.Reader) (providers.CompletionResponse, error) {
	var out providers.CompletionResponse
	sawFinish := false

	for gr, err := range clientcore.SSEEvents[genResponse](r) {
		if err != nil {
			return providers.CompletionResponse{}, fmt.Errorf("read stream: %w", err)
		}
		if gr.Error != nil {
			return providers.CompletionResponse{}, fmt.Errorf("gemini error: %s", gr.Error.Message)
		}
		// usageMetadata accumulates across chunks (last non-nil block wins; Gemini
		// resends the running totals, so the final chunk carries the full count).
		if gr.UsageMetadata != nil {
			out.Usage = providers.Usage{
				InputTokens:       gr.UsageMetadata.PromptTokenCount,
				OutputTokens:      gr.UsageMetadata.CandidatesTokenCount,
				CachedInputTokens: gr.UsageMetadata.CachedContentTokenCount,
			}
		}
		if len(gr.Candidates) == 0 {
			continue
		}
		cand := gr.Candidates[0]
		if cand.FinishReason != "" {
			sawFinish = true
			out.StopReason = cand.FinishReason
			out.Truncated = cand.FinishReason == "MAX_TOKENS"
		}
		for _, p := range cand.Content.Parts {
			switch {
			case p.FunctionCall != nil:
				// Newer Gemini responses carry a call id that correlates parallel calls
				// to the same function; fall back to the name when the id is absent.
				id := p.FunctionCall.ID
				if id == "" {
					id = p.FunctionCall.Name
				}
				out.ToolCalls = append(out.ToolCalls, providers.ToolCall{
					ID:   id,
					Name: p.FunctionCall.Name,
					Args: argString(p.FunctionCall.Args),
				})
			case p.Text != "":
				out.Text += p.Text
			}
		}
	}
	// A stream that ends before any finishReason is a mid-stream drop, not a clean
	// completion — surface it as an error rather than a silently-truncated success.
	if !sawFinish {
		return providers.CompletionResponse{}, fmt.Errorf("stream ended before a finishReason (truncated upstream)")
	}
	return out, nil
}

// toContents maps engine-agnostic messages to Gemini contents. Assistant turns
// become role "model" (text + functionCall parts); consecutive tool results are
// coalesced into a single role "user" turn of functionResponse parts. Each call/
// response carries the originating call id so parallel calls to the SAME function
// name correlate correctly; the name resolves from the id and is kept as a label.
//
// A call id equal to its function name is a fallback (older Gemini API responses
// without a real id): that id is not emitted on the wire, preserving name-only
// behavior for single calls and matching what the model itself sent.
func toContents(in []providers.Message) []content {
	idToName := map[string]string{}
	for _, m := range in {
		for _, tc := range m.ToolCalls {
			idToName[tc.ID] = tc.Name
		}
	}
	var out []content
	for i := 0; i < len(in); i++ {
		m := in[i]
		switch m.Role {
		case "assistant":
			var parts []part
			if m.Content != "" {
				parts = append(parts, part{Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				parts = append(parts, part{FunctionCall: &functionCall{
					ID:   wireID(tc.ID, tc.Name),
					Name: tc.Name,
					Args: clientcore.RawObject(tc.Args),
				}})
			}
			out = append(out, content{Role: "model", Parts: parts})
		case "tool":
			var parts []part
			for i < len(in) && in[i].Role == "tool" {
				id := in[i].ToolCallID
				name := idToName[id]
				if name == "" {
					name = id
				}
				parts = append(parts, part{FunctionResponse: &functionResponse{
					ID:       wireID(id, name),
					Name:     name,
					Response: resultObject(in[i].Content),
				}})
				i++
			}
			i-- // outer loop will increment
			out = append(out, content{Role: "user", Parts: parts})
		default: // user (and any other) → a text part
			out = append(out, content{Role: "user", Parts: []part{{Text: m.Content}}})
		}
	}
	return out
}

// wireID returns the call id to send on the wire, or "" when the id is just the
// function name (our fallback for older API responses with no real id) — in which
// case Gemini correlates by name as before.
func wireID(id, name string) string {
	if id == name {
		return ""
	}
	return id
}

// argString renders a response functionCall's args (a JSON object) as the string
// RunLore's ToolCall carries; empty/null becomes "{}".
func argString(raw json.RawMessage) string {
	s := strings.TrimSpace(string(raw))
	if s == "" || s == "null" {
		return "{}"
	}
	return s
}

// resultObject wraps a tool's string output as Gemini's functionResponse.response
// object (Gemini requires an object, not a bare string).
func resultObject(out string) json.RawMessage {
	b, _ := json.Marshal(map[string]string{"result": out})
	return b
}
