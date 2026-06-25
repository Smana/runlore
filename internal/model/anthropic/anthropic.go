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
		http:      httpx.SecureClient(2 * time.Minute),
	}
}

var _ providers.ModelProvider = (*Client)(nil)

// cacheControl marks a content block as a prompt-cache breakpoint: Anthropic
// caches the request prefix up to and including the marked block. The default
// 5-minute ephemeral cache is GA on anthropic-version 2023-06-01 (no beta header).
type cacheControl struct {
	Type string `json:"type"` // "ephemeral"
}

// ephemeral is the shared 5-minute cache marker (read-only; never mutated).
var ephemeral = &cacheControl{Type: "ephemeral"}

// systemBlock is one block of an array-form system prompt. The array form (vs a
// bare string) is what lets the system prompt carry a cache breakpoint.
type systemBlock struct {
	Type         string        `json:"type"` // "text"
	Text         string        `json:"text"`
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type msgRequest struct {
	Model     string        `json:"model"`
	MaxTokens int           `json:"max_tokens"`
	System    []systemBlock `json:"system,omitempty"`
	Messages  []message     `json:"messages"`
	Tools     []tool        `json:"tools,omitempty"`
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
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

type msgResponse struct {
	Content []block `json:"content"`
	// StopReason is the turn-termination reason; "max_tokens" marks an output cut off
	// at the token ceiling (a truncated, not complete, answer).
	StopReason string `json:"stop_reason"`
	// Usage carries the per-request token counts; a pointer so an absent block parses
	// to nil (unknown) rather than a misleading {0,0}.
	Usage *struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// Complete sends a Messages request with tools and maps the result back.
func (c *Client) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	areq := msgRequest{Model: c.model, MaxTokens: c.maxTokens, Messages: toMessages(req.Messages)}
	if req.System != "" {
		areq.System = []systemBlock{{Type: "text", Text: req.System, CacheControl: ephemeral}}
	}
	for _, t := range req.Tools {
		areq.Tools = append(areq.Tools, tool{Name: t.Name, Description: t.Description, InputSchema: json.RawMessage(t.Schema)})
	}
	// Prompt caching: the system prompt + tool schemas are identical across every
	// step of an investigation's ReAct loop (up to ~20 calls), so mark them as cache
	// breakpoints — Anthropic re-reads that static prefix at ~0.1x input cost instead
	// of re-billing it each step. The system block (set above) caches tools+system
	// (cache prefix order is tools → system); marking the last tool also caches the
	// tool array when there's no system prompt. Savings surface as a drop in
	// usage.input_tokens (cached input moves to cache_read_input_tokens).
	if n := len(areq.Tools); n > 0 {
		areq.Tools[n-1].CacheControl = ephemeral
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
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("read response: %w", err)
	}
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
	out := providers.CompletionResponse{Truncated: mr.StopReason == "max_tokens"}
	if mr.Usage != nil {
		out.Usage = providers.Usage{InputTokens: mr.Usage.InputTokens, OutputTokens: mr.Usage.OutputTokens}
	}
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
