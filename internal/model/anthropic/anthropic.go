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
	"iter"
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
	// responseHeaderTimeout caps the wait for response headers (time-to-first-byte);
	// the streamed body then has no flat deadline (a long completion is legitimate).
	responseHeaderTimeout = 2 * time.Minute
	// idleTimeout aborts a stream that stalls (no bytes) for this long — the streaming
	// counterpart of an overall deadline, without killing an actively-sending stream.
	idleTimeout = 2 * time.Minute
)

// Client is an Anthropic Messages API model provider.
type Client struct {
	baseURL   string
	model     string
	apiKey    string
	maxTokens int
	effort    string
	http      *http.Client
}

// New builds a client. baseURL may be empty (defaults to DefaultBaseURL).
// maxTokens caps output tokens per request; <= 0 falls back to defaultMaxTokens.
// effort opts into deeper reasoning (output_config.effort: low|medium|high|max,
// validated in config); empty omits the field entirely.
func New(baseURL, model, apiKey string, maxTokens int, effort string) *Client {
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
		effort:    effort,
		http:      httpx.SecureStreamingClient(responseHeaderTimeout),
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
	Model        string        `json:"model"`
	MaxTokens    int           `json:"max_tokens"`
	Stream       bool          `json:"stream"`
	System       []systemBlock `json:"system,omitempty"`
	Messages     []message     `json:"messages"`
	Tools        []tool        `json:"tools,omitempty"`
	ToolChoice   *toolChoice   `json:"tool_choice,omitempty"`
	OutputConfig *outputConfig `json:"output_config,omitempty"`
}

// outputConfig carries Anthropic's output-level controls; RunLore only sets the
// effort level. The thinking param is deliberately NOT enabled alongside it:
// adaptive thinking in a multi-turn tool loop requires replaying signed thinking
// blocks verbatim on every subsequent request, which the provider-agnostic message
// history (providers.Message) cannot carry — explicitly out of scope here. effort
// is constant per client, so the prompt-cache prefix stays stable across steps.
type outputConfig struct {
	Effort string `json:"effort"`
}

// toolChoice forces the model to call one named tool this turn
// ({"type":"tool","name":...}). Omitted (nil) = auto: the model chooses freely.
// tool_choice sits below the tools/system cache tiers, so varying it per request
// does not invalidate the tools+system prompt-cache prefix.
type toolChoice struct {
	Type string `json:"type"` // "tool"
	Name string `json:"name"`
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
	// cache breakpoint (set on the last block of the last message — the rolling history breakpoint)
	CacheControl *cacheControl `json:"cache_control,omitempty"`
}

type tool struct {
	Name         string          `json:"name"`
	Description  string          `json:"description"`
	InputSchema  json.RawMessage `json:"input_schema"`
	CacheControl *cacheControl   `json:"cache_control,omitempty"`
}

// streamEvent is one parsed Anthropic SSE event (the `data:` JSON payload). The
// Messages stream interleaves: message_start (input usage) → content_block_start /
// content_block_delta (text_delta text; input_json_delta tool args) /
// content_block_stop, per block → message_delta (stop_reason + output usage) →
// message_stop. Only the fields RunLore accumulates are decoded.
type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message *struct {
		Usage *usageDelta `json:"usage"`
	} `json:"message"`
	ContentBlock *struct {
		Type string `json:"type"` // "text" | "tool_use"
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"` // "text_delta" | "input_json_delta"
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage *usageDelta `json:"usage"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error"`
}

// usageDelta carries token counts; input arrives on message_start, output on
// message_delta (so the two are accumulated from different events).
type usageDelta struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
}

// Complete sends a streaming Messages request with tools and accumulates the full
// SSE response into a single CompletionResponse. Streaming is internal — callers see
// the same one-shot interface; consuming the stream avoids the flat request deadline
// truncating a long completion and lets a per-block input_json_delta reassemble tool
// arguments incrementally.
func (c *Client) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	msgs := toMessages(req.Messages)
	// Rolling cache breakpoint: mark the last content block of the last message, so the
	// growing conversation prefix is a cache READ on the next step. The loop only ever
	// APPENDS to history, so the prefix is byte-identical step to step — a guaranteed
	// rolling hit. Total breakpoints stay <= 4 (system + last tool + this one). Below
	// Anthropic's minimum cacheable size the marker is ignored, so early steps are fine.
	if n := len(msgs); n > 0 {
		if blocks := msgs[n-1].Content; len(blocks) > 0 {
			blocks[len(blocks)-1].CacheControl = ephemeral
		}
	}
	areq := msgRequest{Model: c.model, MaxTokens: c.maxTokens, Stream: true, Messages: msgs}
	if req.ToolChoice != "" {
		areq.ToolChoice = &toolChoice{Type: "tool", Name: req.ToolChoice}
	}
	if c.effort != "" {
		areq.OutputConfig = &outputConfig{Effort: c.effort}
	}
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
	// A child context lets the idle-timeout reader abort a stalled stream by cancelling
	// the in-flight HTTP read; cancel always runs on return to release resources.
	streamCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(streamCtx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("content-type", "application/json")
		r.Header.Set("anthropic-version", apiVersion)
		r.Header.Set("x-api-key", c.apiKey)
		return r, nil
	}
	// DoWithRetry retries only connection establishment / 429 / 5xx (before the stream
	// begins); once a 200 stream is flowing it is never retried mid-stream.
	resp, err := httpx.DoWithRetry(streamCtx, c.http, 3, newReq)
	if err != nil {
		return providers.CompletionResponse{}, fmt.Errorf("messages request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// Read a bounded prefix of the error body. Anthropic returns a JSON error
		// object; surface only its structured type/message (never the raw bytes),
		// sanitized, so 4xx causes (e.g. 400 invalid_request_error "prompt is too
		// long", or a bad tool_use/tool_result pairing) are diagnosable from the
		// error alone — without info disclosure or log injection.
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
		err := fmt.Errorf("messages status %d (request-id %q)%s", resp.StatusCode, httpx.RequestID(resp.Header), anthropicErrorDetail(body))
		// A 4xx other than 429 is permanent: the request itself is bad (e.g. 400
		// invalid_request_error, 401/403 auth, 404), so retrying can't help. Mark it
		// so the investigation workqueue drops it instead of requeuing forever. 429
		// and 5xx are already retried by DoWithRetry and stay transient here.
		if resp.StatusCode >= 400 && resp.StatusCode < 500 && resp.StatusCode != http.StatusTooManyRequests {
			return providers.CompletionResponse{}, providers.Permanent(err)
		}
		return providers.CompletionResponse{}, err
	}
	stream := httpx.NewIdleTimeoutReader(resp.Body, idleTimeout, cancel)
	return accumulate(stream)
}

// anthropicErrorDetail extracts a sanitized ": type: message" suffix from an
// Anthropic error response body, or "" if the body can't be parsed as one. Only
// the structured error.type / error.message fields are surfaced (never the raw
// bytes), control characters are collapsed to spaces to prevent log injection,
// and the message is truncated — so a 4xx cause is diagnosable without echoing
// arbitrary upstream content into an Error-level log.
func anthropicErrorDetail(body []byte) string {
	var e struct {
		Error struct {
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil || (e.Error.Type == "" && e.Error.Message == "") {
		return ""
	}
	msg := sanitizeLine(e.Error.Message)
	const maxLen = 300
	if len(msg) > maxLen {
		msg = msg[:maxLen] + "…"
	}
	return fmt.Sprintf(": %s: %s", sanitizeLine(e.Error.Type), msg)
}

// sanitizeLine collapses control characters (including newlines) to spaces so an
// upstream-controlled string can't inject additional log lines.
func sanitizeLine(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s))
}

// sseEvents parses the Anthropic Messages SSE stream into decoded streamEvents. It
// wraps the generic httpx.SSEData framing primitive and json-unmarshals each event's
// data payload; a framing/read error or a JSON decode error is surfaced via the
// second yield value.
func sseEvents(r io.Reader) iter.Seq2[streamEvent, error] {
	return func(yield func(streamEvent, error) bool) {
		for payload, err := range httpx.SSEData(r) {
			if err != nil {
				yield(streamEvent{}, err)
				return
			}
			var ev streamEvent
			if err := json.Unmarshal(payload, &ev); err != nil {
				if !yield(streamEvent{}, fmt.Errorf("decode sse event: %w", err)) {
					return
				}
				continue
			}
			if !yield(ev, nil) {
				return
			}
		}
	}
}

// accumulate consumes an Anthropic Messages SSE stream and folds it into one
// CompletionResponse: text_delta fragments concatenate into Text, input_json_delta
// fragments per content-block index reassemble each tool_use's JSON args, usage is
// summed across message_start (input) and message_delta (output), and the
// message_delta stop_reason maps to StopReason (and Truncated when "max_tokens").
// A stream that ends before message_stop (a mid-stream drop) is an error.
func accumulate(r io.Reader) (providers.CompletionResponse, error) {
	var out providers.CompletionResponse
	toolArgs := map[int]*strings.Builder{} // content-block index → reassembled JSON
	toolMeta := map[int]*providers.ToolCall{}
	var order []int // tool block indices, in first-seen order
	sawStop := false

	for ev, err := range sseEvents(r) {
		if err != nil {
			return providers.CompletionResponse{}, fmt.Errorf("read stream: %w", err)
		}
		if ev.Error != nil {
			return providers.CompletionResponse{}, fmt.Errorf("anthropic error: %s", ev.Error.Message)
		}
		switch ev.Type {
		case "message_start":
			if ev.Message != nil && ev.Message.Usage != nil {
				u := ev.Message.Usage
				// Anthropic reports input_tokens as the NON-cached remainder; total input is
				// the sum of input + cache_read + cache_creation (per Anthropic docs).
				out.Usage.InputTokens = u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
				out.Usage.CachedInputTokens = u.CacheReadInputTokens
				out.Usage.CacheWriteTokens = u.CacheCreationInputTokens
			}
		case "content_block_start":
			if ev.ContentBlock != nil && ev.ContentBlock.Type == "tool_use" {
				toolArgs[ev.Index] = &strings.Builder{}
				toolMeta[ev.Index] = &providers.ToolCall{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
				order = append(order, ev.Index)
			}
		case "content_block_delta":
			if ev.Delta == nil {
				continue
			}
			switch ev.Delta.Type {
			case "text_delta":
				out.Text += ev.Delta.Text
			case "input_json_delta":
				if b := toolArgs[ev.Index]; b != nil {
					b.WriteString(ev.Delta.PartialJSON)
				}
			}
		case "message_delta":
			if ev.Delta != nil && ev.Delta.StopReason != "" {
				out.StopReason = ev.Delta.StopReason
				out.Truncated = ev.Delta.StopReason == "max_tokens"
			}
			if ev.Usage != nil && ev.Usage.OutputTokens != 0 {
				out.Usage.OutputTokens = ev.Usage.OutputTokens
			}
		case "message_stop":
			sawStop = true
		}
	}
	if !sawStop {
		return providers.CompletionResponse{}, fmt.Errorf("stream ended before message_stop (truncated upstream)")
	}
	for _, idx := range order {
		tc := *toolMeta[idx]
		tc.Args = toolArgs[idx].String()
		out.ToolCalls = append(out.ToolCalls, tc)
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
