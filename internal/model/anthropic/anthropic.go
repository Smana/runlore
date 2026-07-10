// SPDX-License-Identifier: Apache-2.0

// Package anthropic implements providers.ModelProvider against the Anthropic
// Messages API (native tool use). It maps RunLore's engine-agnostic exchange
// (OpenAI-shaped: assistant tool_calls + separate tool messages) onto Anthropic's
// content-block form (tool_use blocks; tool_result blocks coalesced into one user
// turn).
package anthropic

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

// DefaultBaseURL is the public Anthropic API.
const DefaultBaseURL = "https://api.anthropic.com"

const apiVersion = "2023-06-01"

// Client is an Anthropic Messages API model provider.
type Client struct {
	clientcore.Base
	effort   string
	thinking string
}

// New builds a client. baseURL may be empty (defaults to DefaultBaseURL).
// maxTokens caps output tokens per request; <= 0 falls back to
// clientcore.DefaultMaxTokens. effort opts into deeper reasoning
// (output_config.effort: low|medium|high|max, validated in config); empty
// omits the field entirely. thinking opts into adaptive extended thinking
// ("adaptive", validated in config); empty keeps today's byte-for-byte behaviour.
func New(baseURL, model, apiKey string, maxTokens int, effort, thinking string) *Client {
	return &Client{
		Base:     clientcore.NewBase(baseURL, DefaultBaseURL, model, apiKey, maxTokens),
		effort:   effort,
		thinking: thinking,
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
	Model        string          `json:"model"`
	MaxTokens    int             `json:"max_tokens"`
	Stream       bool            `json:"stream"`
	System       []systemBlock   `json:"system,omitempty"`
	Messages     []message       `json:"messages"`
	Tools        []tool          `json:"tools,omitempty"`
	ToolChoice   *toolChoice     `json:"tool_choice,omitempty"`
	OutputConfig *outputConfig   `json:"output_config,omitempty"`
	Thinking     *thinkingConfig `json:"thinking,omitempty"`
}

// outputConfig carries Anthropic's output-level controls; RunLore only sets the
// effort level. effort is constant per client, so the prompt-cache prefix stays
// stable across steps. effort and thinking are independent controls and may be sent
// together (effort is soft guidance for how much adaptive thinking Claude does).
type outputConfig struct {
	Effort string `json:"effort"`
}

// thinkingConfig opts into adaptive extended thinking ({"type":"adaptive"}). In this
// mode Claude decides when and how much to think, and interleaved thinking is enabled
// automatically. Adaptive thinking is signed: in a multi-turn tool-use conversation
// the assistant turn's thinking/redacted_thinking blocks MUST be replayed verbatim
// (with their signature) on later requests — the client carries them across the
// provider-agnostic history via providers.Message.Opaque (see accumulate/toMessages).
//
// budget_tokens is deliberately never sent: it is rejected (400) on Claude 4.6+ and
// deprecated elsewhere — adaptive is the only supported on-mode for the target models.
type thinkingConfig struct {
	Type string `json:"type"` // "adaptive"
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
	// raw, when non-nil, is the verbatim JSON for this block and the struct fields are
	// ignored (see MarshalJSON). Used to replay a thinking/redacted_thinking block
	// byte-for-byte with its signature: struct-tag omitempty would drop a thinking
	// field that is empty under display:"omitted", and the API rejects a modified
	// block. raw is never decoded from a request — it exists only to re-emit captured
	// thinking blocks on replay.
	raw json.RawMessage

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

// MarshalJSON emits the verbatim raw bytes of a replayed thinking block when set,
// otherwise the normal struct encoding. The alias sheds block's own MarshalJSON to
// avoid infinite recursion; the unexported raw field is skipped by encoding/json.
func (b block) MarshalJSON() ([]byte, error) {
	if b.raw != nil {
		return b.raw, nil
	}
	type alias block
	return json.Marshal(alias(b))
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
		Type string `json:"type"` // "text" | "tool_use" | "thinking" | "redacted_thinking"
		ID   string `json:"id"`
		Name string `json:"name"`
		Data string `json:"data"` // redacted_thinking: the full opaque payload (no deltas follow)
	} `json:"content_block"`
	Delta *struct {
		Type        string `json:"type"` // "text_delta" | "input_json_delta" | "thinking_delta" | "signature_delta"
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
		Thinking    string `json:"thinking"`  // thinking_delta: reasoning text (empty under display:"omitted")
		Signature   string `json:"signature"` // signature_delta: the block's encrypted signature
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
	// Adaptive thinking is incompatible with a forced tool_choice (tool|any): the API
	// rejects that combination (400). The forced ToolChoice happens only on the
	// budget-forced conclusion step (submit_findings). Deterministic rule: when both
	// are requested, drop the thinking param for THIS request AND strip the replayed
	// thinking blocks from history (with thinking off, the API rejects thinking blocks
	// in the assistant turns) — see toMessages(includeThinking=false). Tradeoff:
	// switching adaptive→off invalidates the messages prompt-cache tier for this one
	// request (tools+system stay cached); acceptable, as it fires once, terminally.
	thinkingOn := c.thinking != "" && req.ToolChoice == ""
	msgs := toMessages(req.Messages, thinkingOn)
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
	areq := msgRequest{Model: c.Model, MaxTokens: c.MaxTokens, Stream: true, Messages: msgs}
	if req.ToolChoice != "" {
		areq.ToolChoice = &toolChoice{Type: "tool", Name: req.ToolChoice}
	}
	if c.effort != "" {
		areq.OutputConfig = &outputConfig{Effort: c.effort}
	}
	if thinkingOn {
		areq.Thinking = &thinkingConfig{Type: c.thinking}
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
	return c.Stream(ctx, clientcore.Request{
		Op:   "messages",
		URL:  c.BaseURL + "/v1/messages",
		Body: areq,
		SetHeaders: func(r *http.Request) {
			r.Header.Set("content-type", "application/json")
			r.Header.Set("anthropic-version", apiVersion)
			r.Header.Set("x-api-key", c.APIKey)
		},
		ErrorDetail: anthropicErrorDetail,
	}, accumulate)
}

// anthropicErrorDetail extracts a sanitized ": type: message" suffix from an
// Anthropic error response body, or "" if the body can't be parsed as one.
// Only the structured error.type / error.message fields are surfaced (never
// the raw bytes); sanitization and truncation live in clientcore.Detail.
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
	return clientcore.Detail(e.Error.Type, e.Error.Message)
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
	// Thinking capture (adaptive thinking): a thinking block's text arrives via
	// thinking_delta and its signature via signature_delta, both keyed by content-block
	// index; a redacted_thinking block carries its full payload on content_block_start.
	// Thinking text is kept OUT of out.Text and folded into out.Opaque instead.
	thinkText := map[int]*strings.Builder{}
	thinkMeta := map[int]*thinkBlock{}
	var thinkOrder []int // thinking block indices, in first-seen order
	sawStop := false

	for ev, err := range clientcore.SSEEvents[streamEvent](r) {
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
			if ev.ContentBlock == nil {
				continue
			}
			switch ev.ContentBlock.Type {
			case "tool_use":
				toolArgs[ev.Index] = &strings.Builder{}
				toolMeta[ev.Index] = &providers.ToolCall{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name}
				order = append(order, ev.Index)
			case "thinking":
				thinkText[ev.Index] = &strings.Builder{}
				thinkMeta[ev.Index] = &thinkBlock{Type: "thinking"}
				thinkOrder = append(thinkOrder, ev.Index)
			case "redacted_thinking":
				// No deltas follow; the opaque payload is fully present here.
				thinkMeta[ev.Index] = &thinkBlock{Type: "redacted_thinking", Data: ev.ContentBlock.Data}
				thinkOrder = append(thinkOrder, ev.Index)
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
			case "thinking_delta":
				if b := thinkText[ev.Index]; b != nil {
					b.WriteString(ev.Delta.Thinking)
				}
			case "signature_delta":
				if m := thinkMeta[ev.Index]; m != nil {
					m.Signature += ev.Delta.Signature
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
	// Serialize completed thinking blocks (in index order, with signatures) into
	// Opaque so the loop can replay them verbatim on the next request. A "thinking"
	// block is only completed once its signature has arrived (a mid-thinking truncation
	// yields no signature); replaying an unsigned block would be rejected, so drop it.
	// redacted_thinking blocks are always complete at content_block_start.
	if raw := marshalThinking(thinkOrder, thinkMeta, thinkText); raw != nil {
		out.Opaque = raw
	}
	return out, nil
}

// thinkBlock is accumulation state for one captured thinking block: its type, the
// signature reassembled from signature_delta events ("thinking"), and the opaque
// payload from content_block_start ("redacted_thinking"). The thinking text lives in
// a separate builder (keyed by index) since it arrives via thinking_delta.
type thinkBlock struct {
	Type      string // "thinking" | "redacted_thinking"
	Signature string // "thinking" only
	Data      string // "redacted_thinking" only
}

// marshalThinking renders the captured thinking blocks as a JSON array of block
// objects, or nil when there are none to replay. Each element is emitted with a
// fixed field set (thinking + signature for thinking; data for redacted_thinking) so
// the wire bytes are stable and reproducible for verbatim replay.
func marshalThinking(order []int, meta map[int]*thinkBlock, text map[int]*strings.Builder) json.RawMessage {
	var elems []json.RawMessage
	for _, idx := range order {
		b := meta[idx]
		switch b.Type {
		case "thinking":
			if b.Signature == "" {
				continue // incomplete/truncated thinking — cannot be replayed
			}
			raw, err := json.Marshal(struct {
				Type      string `json:"type"`
				Thinking  string `json:"thinking"`
				Signature string `json:"signature"`
			}{"thinking", text[idx].String(), b.Signature})
			if err != nil {
				continue
			}
			elems = append(elems, raw)
		case "redacted_thinking":
			raw, err := json.Marshal(struct {
				Type string `json:"type"`
				Data string `json:"data"`
			}{"redacted_thinking", b.Data})
			if err != nil {
				continue
			}
			elems = append(elems, raw)
		}
	}
	if len(elems) == 0 {
		return nil
	}
	raw, err := json.Marshal(elems)
	if err != nil {
		return nil
	}
	return raw
}

// toMessages maps engine-agnostic messages to Anthropic messages. Consecutive
// "tool" results are coalesced into a single user message (Anthropic requires all
// tool_result blocks answering an assistant turn to share one user message).
//
// When includeThinking is true, an assistant turn's replayable thinking blocks
// (carried verbatim in Message.Opaque, produced by accumulate) are PREPENDED to that
// turn's content — adaptive thinking requires the signed thinking blocks first, ahead
// of text and tool_use, and unchanged. When false (thinking disabled for this request,
// e.g. a forced tool_choice), the thinking blocks are stripped so the history stays
// valid with thinking off.
func toMessages(in []providers.Message, includeThinking bool) []message {
	var out []message
	for i := 0; i < len(in); i++ {
		m := in[i]
		switch m.Role {
		case "assistant":
			var blocks []block
			if includeThinking && len(m.Opaque) > 0 {
				var thinks []json.RawMessage
				if err := json.Unmarshal(m.Opaque, &thinks); err == nil {
					for _, t := range thinks {
						blocks = append(blocks, block{raw: t})
					}
				}
			}
			if m.Content != "" {
				blocks = append(blocks, block{Type: "text", Text: m.Content})
			}
			for _, tc := range m.ToolCalls {
				blocks = append(blocks, block{Type: "tool_use", ID: tc.ID, Name: tc.Name, Input: clientcore.RawObject(tc.Args)})
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
