// Package gemini implements providers.ModelProvider against the Gemini API
// (generateContent with native function calling). It maps RunLore's engine-
// agnostic exchange (OpenAI-shaped: assistant tool_calls + separate tool messages)
// onto Gemini's contents/parts form: assistant turns become role "model" with
// functionCall parts, and tool results coalesce into one role "user" turn of
// functionResponse parts. Each call/response carries the originating call id, so
// parallel calls to the same function name correlate by id (Gemini's rule for
// parallel calls); when the model returns no id, correlation falls back to name.
package gemini

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
)

// DefaultBaseURL is the public Gemini API.
const DefaultBaseURL = "https://generativelanguage.googleapis.com"

// defaultMaxTokens is the output-token ceiling used when the caller passes <= 0.
const defaultMaxTokens = 8192

// Client is a Gemini (generateContent) model provider.
type Client struct {
	baseURL   string
	model     string
	apiKey    string
	maxTokens int
	http      *http.Client
}

// New builds a client. baseURL may be empty (defaults to DefaultBaseURL).
// maxTokens caps output tokens per request; <= 0 falls back to defaultMaxTokens.
func New(baseURL, model, apiKey string, maxTokens int) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		model:     model,
		apiKey:    apiKey,
		maxTokens: maxTokens,
		http:      httpx.SecureClient(2 * time.Minute),
	}
}

var _ providers.ModelProvider = (*Client)(nil)

type genRequest struct {
	SystemInstruction *content  `json:"system_instruction,omitempty"`
	Contents          []content `json:"contents"`
	Tools             []tool    `json:"tools,omitempty"`
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

type genResponse struct {
	Candidates []struct {
		Content content `json:"content"`
		// FinishReason is the candidate-termination reason; "MAX_TOKENS" marks an output
		// cut off at the token ceiling (a truncated, not complete, answer).
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	// UsageMetadata carries the per-request token counts; a pointer so an absent block
	// parses to nil (unknown) rather than a misleading {0,0}.
	UsageMetadata *struct {
		PromptTokenCount     int `json:"promptTokenCount"`
		CandidatesTokenCount int `json:"candidatesTokenCount"`
	} `json:"usageMetadata"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends a generateContent request with tools and maps the result back.
func (c *Client) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	greq := genRequest{Contents: toContents(req.Messages)}
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
	body, err := json.Marshal(greq)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	url := fmt.Sprintf("%s/v1beta/models/%s:generateContent", c.baseURL, c.model)
	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		r.Header.Set("x-goog-api-key", c.apiKey)
		return r, nil
	}
	resp, err := httpx.DoWithRetry(ctx, c.http, 3, newReq)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("generateContent request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// Don't echo the upstream body into an Error-level log (info disclosure +
		// log injection); surface status + sanitized request-id for correlation.
		return providers.CompletionResponse{}, fmt.Errorf("generateContent status %d (request-id %q)", resp.StatusCode, httpx.RequestID(resp.Header))
	}
	var gr genResponse
	if err := json.Unmarshal(data, &gr); err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("parse response: %w", err)
	}
	if gr.Error != nil {
		return providers.CompletionResponse{}, fmt.Errorf("gemini error: %s", gr.Error.Message)
	}
	out := providers.CompletionResponse{}
	if gr.UsageMetadata != nil {
		out.Usage = providers.Usage{InputTokens: gr.UsageMetadata.PromptTokenCount, OutputTokens: gr.UsageMetadata.CandidatesTokenCount}
	}
	if len(gr.Candidates) == 0 {
		return out, nil
	}
	out.Truncated = gr.Candidates[0].FinishReason == "MAX_TOKENS"
	for _, p := range gr.Candidates[0].Content.Parts {
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
					Args: rawObject(tc.Args),
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

// rawObject ensures functionCall args is a JSON object ("" → {}).
func rawObject(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(args)
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
