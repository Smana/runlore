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

// defaultMaxTokens is the output-token ceiling used when the caller passes <= 0.
const defaultMaxTokens = 8192

const (
	// responseHeaderTimeout caps the wait for response headers (time-to-first-byte);
	// the streamed body then has no flat deadline (a long completion is legitimate).
	responseHeaderTimeout = 2 * time.Minute
	// idleTimeout aborts a stream that stalls (no bytes) for this long — the streaming
	// counterpart of an overall deadline, without killing an actively-sending stream.
	idleTimeout = 2 * time.Minute
)

// Client is an OpenAI-compatible model provider.
type Client struct {
	baseURL   string
	model     string
	apiKey    string
	maxTokens int
	http      *http.Client
}

// New builds a client. apiKey may be empty (keyless vLLM/Ollama). maxTokens caps
// output tokens per request; <= 0 falls back to defaultMaxTokens.
func New(baseURL, model, apiKey string, maxTokens int) *Client {
	if maxTokens <= 0 {
		maxTokens = defaultMaxTokens
	}
	return &Client{
		baseURL:   strings.TrimRight(baseURL, "/"),
		model:     model,
		apiKey:    apiKey,
		maxTokens: maxTokens,
		http:      httpx.SecureStreamingClient(responseHeaderTimeout),
	}
}

var _ providers.ModelProvider = (*Client)(nil)

type chatRequest struct {
	Model     string     `json:"model"`
	Messages  []any      `json:"messages"` // chatMessage | toolMessage
	Tools     []chatTool `json:"tools,omitempty"`
	MaxTokens int        `json:"max_tokens,omitempty"`
	Stream    bool       `json:"stream"`
	// StreamOptions asks the server to emit a trailing usage-only chunk on a streamed
	// response (omitted otherwise, since a non-streaming request rejects it).
	StreamOptions *streamOptions `json:"stream_options,omitempty"`
}

// streamOptions toggles streaming extras; include_usage adds a final chunk whose
// choices array is empty and whose usage block carries the per-request token counts.
type streamOptions struct {
	IncludeUsage bool `json:"include_usage"`
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

// chatChunk is one parsed chat/completions SSE event (the `data:` JSON payload). The
// stream interleaves: content/tool_call delta chunks (choices[0].delta), a final
// finish_reason chunk (choices[0].finish_reason), and — with stream_options.
// include_usage — a trailing usage-only chunk (empty choices, populated usage),
// followed by a literal `[DONE]` sentinel. Only the accumulated fields are decoded.
type chatChunk struct {
	Choices []chunkChoice `json:"choices"`
	// Usage is the per-request token count, present only on the trailing usage chunk;
	// a pointer so an absent block parses to nil (unknown) rather than a misleading {0,0}.
	Usage *struct {
		PromptTokens        int `json:"prompt_tokens"`
		CompletionTokens    int `json:"completion_tokens"`
		PromptTokensDetails *struct {
			CachedTokens int `json:"cached_tokens"`
		} `json:"prompt_tokens_details"`
	} `json:"usage"`
}

type chunkChoice struct {
	Delta chunkDelta `json:"delta"`
	// FinishReason is the choice-termination reason, non-empty only on the final
	// content chunk; "length" marks an output cut off at the token ceiling.
	FinishReason string `json:"finish_reason"`
}

type chunkDelta struct {
	Content   string          `json:"content"`
	ToolCalls []chunkToolCall `json:"tool_calls"`
}

// chunkToolCall is one tool-call fragment. Fragments are keyed by Index: the first
// fragment for an index carries ID and Function.Name, later fragments carry
// Function.Arguments string fragments that concatenate into the full JSON args.
type chunkToolCall struct {
	Index    int               `json:"index"`
	ID       string            `json:"id"`
	Type     string            `json:"type"`
	Function chunkFunctionCall `json:"function"`
}

type chunkFunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// Complete sends a streaming chat completion with tools and accumulates the full SSE
// response into a single CompletionResponse. Streaming is internal — callers see the
// same one-shot interface; consuming the stream avoids the flat request deadline
// truncating a long completion and lets per-index argument fragments reassemble each
// tool call's JSON args incrementally.
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

	body, err := json.Marshal(chatRequest{
		Model:         c.model,
		Messages:      msgs,
		Tools:         tools,
		MaxTokens:     c.maxTokens,
		Stream:        true,
		StreamOptions: &streamOptions{IncludeUsage: true},
	})
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("marshal request: %w", err)
	}
	// A child context lets the idle-timeout reader abort a stalled stream by cancelling
	// the in-flight HTTP read; cancel always runs on return to release resources.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(streamCtx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			r.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		return r, nil
	}
	// DoWithRetry retries only connection establishment / 429 / 5xx (before the stream
	// begins); once a 200 stream is flowing it is never retried mid-stream.
	resp, err := httpx.DoWithRetry(streamCtx, c.http, 3, newReq)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("chat request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Drain a bounded prefix so the connection can be reused, but never echo the
		// upstream body into an Error-level log: base_url is operator-configurable, so a
		// misbehaving/compromised proxy could inject arbitrary content (info disclosure +
		// log injection). Surface the status and the upstream request-id (sanitized).
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return providers.CompletionResponse{}, fmt.Errorf("chat status %d (request-id %q)", resp.StatusCode, httpx.RequestID(resp.Header))
	}
	stream := httpx.NewIdleTimeoutReader(resp.Body, idleTimeout, cancel)
	return accumulate(stream)
}

// accumulate consumes an OpenAI chat/completions SSE stream and folds it into one
// CompletionResponse: content deltas concatenate into Text, tool-call fragments per
// choice index reassemble each tool call's JSON args (first fragment carries id/name,
// later carry arguments fragments), the choice finish_reason maps to StopReason (and
// Truncated when "length"), and the trailing usage-only chunk fills Usage. A read
// error, or a stream that ends with no finish_reason and no [DONE] (a mid-stream
// drop), is an error rather than a silently-truncated success.
func accumulate(r io.Reader) (providers.CompletionResponse, error) {
	var out providers.CompletionResponse
	type toolAcc struct {
		id, name string
		args     strings.Builder
	}
	toolByIndex := map[int]*toolAcc{}
	var order []int // tool-call indices, in first-seen order
	done := false   // saw the finish_reason and/or the [DONE] terminator

	for payload, err := range httpx.SSEData(r) {
		if err != nil {
			return providers.CompletionResponse{}, fmt.Errorf("read stream: %w", err)
		}
		if string(bytes.TrimSpace(payload)) == "[DONE]" {
			done = true
			break
		}
		var ck chatChunk
		if err := json.Unmarshal(payload, &ck); err != nil {
			return providers.CompletionResponse{}, fmt.Errorf("decode sse event: %w", err)
		}
		if ck.Usage != nil {
			u := providers.Usage{InputTokens: ck.Usage.PromptTokens, OutputTokens: ck.Usage.CompletionTokens}
			if ck.Usage.PromptTokensDetails != nil {
				u.CachedInputTokens = ck.Usage.PromptTokensDetails.CachedTokens
			}
			out.Usage = u
		}
		if len(ck.Choices) == 0 {
			continue // usage-only (or empty) chunk: no delta to fold
		}
		choice := ck.Choices[0]
		out.Text += choice.Delta.Content
		for _, tc := range choice.Delta.ToolCalls {
			acc := toolByIndex[tc.Index]
			if acc == nil {
				acc = &toolAcc{}
				toolByIndex[tc.Index] = acc
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				acc.id = tc.ID
			}
			if tc.Function.Name != "" {
				acc.name = tc.Function.Name
			}
			acc.args.WriteString(tc.Function.Arguments)
		}
		if choice.FinishReason != "" {
			out.StopReason = choice.FinishReason
			out.Truncated = choice.FinishReason == "length"
			done = true
		}
	}
	if !done {
		return providers.CompletionResponse{}, fmt.Errorf("stream ended before finish_reason or [DONE] (truncated upstream)")
	}
	for _, idx := range order {
		acc := toolByIndex[idx]
		out.ToolCalls = append(out.ToolCalls, providers.ToolCall{ID: acc.id, Name: acc.name, Args: acc.args.String()})
	}
	return out, nil
}
