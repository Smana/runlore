// Package anthropic implements providers.ModelProvider against the Anthropic
// Messages API (native tool use). It maps RunLore's engine-agnostic exchange
// (OpenAI-shaped: assistant tool_calls + separate tool messages) onto Anthropic's
// content-block form (tool_use blocks; tool_result blocks coalesced into one user
// turn).
package anthropic

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

// DefaultBaseURL is the public Anthropic API.
const DefaultBaseURL = "https://api.anthropic.com"

const (
	defaultMaxTokens = 4096
	apiVersion       = "2023-06-01"
)

// Client is an Anthropic Messages API model provider.
type Client struct {
	baseURL   string
	model     string
	apiKey    string
	maxTokens int
	http      *http.Client
}

// New builds a client. baseURL may be empty (defaults to DefaultBaseURL).
func New(baseURL, model, apiKey string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		model:     model,
		apiKey:    apiKey,
		maxTokens: defaultMaxTokens,
		http:      &http.Client{Timeout: 2 * time.Minute},
	}
}

var _ providers.ModelProvider = (*Client)(nil)

type msgRequest struct {
	Model     string    `json:"model"`
	MaxTokens int       `json:"max_tokens"`
	System    string    `json:"system,omitempty"`
	Messages  []message `json:"messages"`
	Tools     []tool    `json:"tools,omitempty"`
}

type message struct {
	Role    string  `json:"role"`
	Content []block `json:"content"`
}

type block struct {
	Type string `json:"type"`
	// text
	Text string `json:"text,omitempty"`
	// tool_use
	ID    string          `json:"id,omitempty"`
	Name  string          `json:"name,omitempty"`
	Input json.RawMessage `json:"input,omitempty"`
	// tool_result
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   string `json:"content,omitempty"`
}

type tool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

type msgResponse struct {
	Content []block `json:"content"`
	Error   *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends a Messages request with tools and maps the result back.
func (c *Client) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	areq := msgRequest{Model: c.model, MaxTokens: c.maxTokens, System: req.System, Messages: toMessages(req.Messages)}
	for _, t := range req.Tools {
		areq.Tools = append(areq.Tools, tool{Name: t.Name, Description: t.Description, InputSchema: json.RawMessage(t.Schema)})
	}
	body, err := json.Marshal(areq)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("content-type", "application/json")
		r.Header.Set("anthropic-version", apiVersion)
		r.Header.Set("x-api-key", c.apiKey)
		return r, nil
	}
	resp, err := httpx.DoWithRetry(ctx, c.http, 3, newReq)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("messages request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		// Don't echo the upstream body into an Error-level log (info disclosure +
		// log injection); surface status + sanitized request-id for correlation.
		return providers.CompletionResponse{}, fmt.Errorf("messages status %d (request-id %q)", resp.StatusCode, httpx.RequestID(resp.Header))
	}
	var mr msgResponse
	if err := json.Unmarshal(data, &mr); err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("parse response: %w", err)
	}
	if mr.Error != nil {
		return providers.CompletionResponse{}, fmt.Errorf("anthropic error: %s", mr.Error.Message)
	}
	out := providers.CompletionResponse{}
	for _, b := range mr.Content {
		switch b.Type {
		case "text":
			out.Text += b.Text
		case "tool_use":
			out.ToolCalls = append(out.ToolCalls, providers.ToolCall{ID: b.ID, Name: b.Name, Args: string(b.Input)})
		}
	}
	return out, nil
}

// toMessages maps engine-agnostic messages to Anthropic messages. Consecutive
// "tool" results are coalesced into a single user message (Anthropic requires all
// tool_result blocks answering an assistant turn to share one user message).
func toMessages(in []providers.Message) []message {
	var out []message
	for i := 0; i < len(in); i++ {
		m := in[i]
		switch m.Role {
		case "assistant":
			var blocks []block
			if m.Content != "" {
				blocks = append(blocks, block{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, block{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: rawObject(tc.Args)})
			}
			out = append(out, message{Role: "assistant", Content: blocks})
		case "tool":
			var results []block
			for i < len(in) && in[i].Role == "tool" {
				results = append(results, block{Type: "tool_result", ToolUseID: in[i].ToolCallID, Content: in[i].Content})
				i++
			}
			i-- // outer loop will increment
			out = append(out, message{Role: "user", Content: results})
		default: // user (and any other) → a text block
			out = append(out, message{Role: "user", Content: []block{{Type: "text", Text: m.Content}}})
		}
	}
	return out
}

// rawObject ensures tool_use input is a JSON object ("" → {}).
func rawObject(args string) json.RawMessage {
	if strings.TrimSpace(args) == "" {
		return json.RawMessage("{}")
	}
	return json.RawMessage(args)
}
