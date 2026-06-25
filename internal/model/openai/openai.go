// Package openai implements providers.ModelProvider against an OpenAI-compatible
// /chat/completions endpoint (OpenAI, in-cluster vLLM, Ollama, OpenRouter).
package openai

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

// Client is an OpenAI-compatible model provider.
type Client struct {
	baseURL string
	model   string
	apiKey  string
	http    *http.Client
}

// New builds a client. apiKey may be empty (keyless vLLM/Ollama).
func New(baseURL, model, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  apiKey,
		http:    &http.Client{Timeout: 2 * time.Minute},
	}
}

var _ providers.ModelProvider = (*Client)(nil)

type chatRequest struct {
	Model    string     `json:"model"`
	Messages []any      `json:"messages"` // chatMessage | toolMessage
	Tools    []chatTool `json:"tools,omitempty"`
}

type chatMessage struct {
	Role       string         `json:"role"`
	Content    string         `json:"content,omitempty"`
	ToolCalls  []chatToolCall `json:"tool_calls,omitempty"`
	ToolCallID string         `json:"tool_call_id,omitempty"`
}

// toolMessage is the tool-role result. Its content has no omitempty: the OpenAI
// chat-completions schema (and strict OpenAI-compatible servers — vLLM, Ollama)
// require a "content" field on a tool message, so an empty tool result must still
// serialize "content":"" rather than be elided.
type toolMessage struct {
	Role       string `json:"role"`
	Content    string `json:"content"`
	ToolCallID string `json:"tool_call_id,omitempty"`
}

type chatToolCall struct {
	ID       string           `json:"id"`
	Type     string           `json:"type"`
	Function chatFunctionCall `json:"function"`
}

type chatFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type chatTool struct {
	Type     string       `json:"type"`
	Function chatFunction `json:"function"`
}

type chatFunction struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
}

type chatChoice struct {
	Message chatMessage `json:"message"`
	// FinishReason is the choice-termination reason; "length" marks an output cut off
	// at the token ceiling (a truncated, not complete, answer).
	FinishReason string `json:"finish_reason"`
}

type chatResponse struct {
	Choices []chatChoice `json:"choices"`
	// Usage carries the per-request token counts; a pointer so an absent block parses
	// to nil (unknown) rather than a misleading {0,0}.
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Complete sends a chat completion with tools and maps the result back.
func (c *Client) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	msgs := make([]any, 0, len(req.Messages)+1)
	if req.System != "" {
		msgs = append(msgs, chatMessage{Role: "system", Content: req.System})
	}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			// Tool results use a shape that always emits "content" (see toolMessage).
			msgs = append(msgs, toolMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID})
			continue
		}
		cm := chatMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, tc := range m.ToolCalls {
			cm.ToolCalls = append(cm.ToolCalls, chatToolCall{
				ID: tc.ID, Type: "function",
				Function: chatFunctionCall{Name: tc.Name, Arguments: tc.Args},
			})
		}
		msgs = append(msgs, cm)
	}
	tools := make([]chatTool, 0, len(req.Tools))
	for _, t := range req.Tools {
		tools = append(tools, chatTool{Type: "function", Function: chatFunction{
			Name: t.Name, Description: t.Description, Parameters: json.RawMessage(t.Schema),
		}})
	}

	body, err := json.Marshal(chatRequest{Model: c.model, Messages: msgs, Tools: tools})
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			r.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		return r, nil
	}
	resp, err := httpx.DoWithRetry(ctx, c.http, 3, newReq)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("chat request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return providers.CompletionResponse{}, fmt.Errorf("chat status %d: %s", resp.StatusCode, string(data[:min(len(data), 512)]))
	}
	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("parse response: %w", err)
	}
	if len(cr.Choices) == 0 {
		return providers.CompletionResponse{}, fmt.Errorf("no choices in response")
	}
	choice := cr.Choices[0]
	msg := choice.Message
	out := providers.CompletionResponse{Text: msg.Content, Truncated: choice.FinishReason == "length"}
	if cr.Usage != nil {
		out.Usage = providers.Usage{InputTokens: cr.Usage.PromptTokens, OutputTokens: cr.Usage.CompletionTokens}
	}
	for _, tc := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, providers.ToolCall{ID: tc.ID, Name: tc.Function.Name, Args: tc.Function.Arguments})
	}
	return out, nil
}
