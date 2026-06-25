// Package gemini implements providers.ModelProvider against the Gemini API
// (generateContent with native function calling). It maps RunLore's engine-
// agnostic exchange (OpenAI-shaped: assistant tool_calls + separate tool messages)
// onto Gemini's contents/parts form: assistant turns become role "model" with
// functionCall parts, and tool results coalesce into one role "user" turn of
// functionResponse parts (Gemini matches a response to its call by function name).
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

// Client is a Gemini (generateContent) model provider.
type Client struct {
	baseURL string
	model   string
	apiKey  string
	http    *http.Client
}

// New builds a client. baseURL may be empty (defaults to DefaultBaseURL).
func New(baseURL, model, apiKey string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 2 * time.Minute},
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
	Name string          `json:"name"`
	Args json.RawMessage `json:"args,omitempty"`
}

type functionResponse struct {
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
	} `json:"candidates"`
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
	data, _ := io.ReadAll(resp.Body)
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
	if len(gr.Candidates) == 0 {
		return out, nil
	}
	for _, p := range gr.Candidates[0].Content.Parts {
		switch {
		case p.FunctionCall != nil:
			out.ToolCalls = append(out.ToolCalls, providers.ToolCall{
				ID:   p.FunctionCall.Name, // Gemini matches a function response to its call by name
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
// coalesced into a single role "user" turn of functionResponse parts, named by the
// function they answer (resolved from the originating tool call's id).
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
				parts = append(parts, part{FunctionCall: &functionCall{Name: tc.Name, Args: rawObject(tc.Args)}})
			}
			out = append(out, content{Role: "model", Parts: parts})
		case "tool":
			var parts []part
			for i < len(in) && in[i].Role == "tool" {
				name := idToName[in[i].ToolCallID]
				if name == "" {
					name = in[i].ToolCallID
				}
				parts = append(parts, part{FunctionResponse: &functionResponse{Name: name, Response: resultObject(in[i].Content)}})
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
