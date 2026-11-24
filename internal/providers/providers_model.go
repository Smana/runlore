// SPDX-License-Identifier: Apache-2.0

package providers

import (
	"context"
	"encoding/json"
	"strings"
)

// ModelProvider abstracts the LLM (Anthropic | OpenAI-compatible: vLLM/Ollama).
type ModelProvider interface {
	Complete(ctx context.Context, req CompletionRequest) (CompletionResponse, error)
}

// QuestionKind is the primitive a Question asks for. The names are the wire values
// of TypeSafe's System One API, carried verbatim — "noul" is that vendor's own name
// for its boolean primitive and is not a typo for "bool".
type QuestionKind string

// The three question kinds a Decider answers. Which Answer fields are meaningful
// follows from the kind — see Answer.
const (
	KindChoice QuestionKind = "choice" // pick one option; returns Choice + Probabilities + Confidence
	KindScore  QuestionKind = "score"  // place the state on an ordered rubric; returns Score + Confidence
	KindNoul   QuestionKind = "noul"   // is this statement true; returns Noul, a probability in [0,1]
)

// Question is one bounded judgement to make about a state.
type Question struct {
	ID           string
	Kind         QuestionKind
	Instructions string
	// Choices maps an option id to its criterion; KindChoice only. The option ids are
	// what comes back in Answer.Choice, so a caller that needs "none of these" must
	// offer it as an explicit option.
	Choices map[string]string
	// Levels are the ordered rubric descriptions, lowest first; KindScore only.
	Levels []string
}

// Answer is one typed judgement. Which fields are meaningful follows the Question's
// Kind: Choice and Confidence for KindChoice, Score and Confidence for KindScore,
// Noul alone for KindNoul — a noul IS its own probability, so it carries no separate
// confidence and a caller thresholds on Noul directly.
type Answer struct {
	Choice        string
	Noul          float64
	Score         float64
	Probabilities map[string]float64
	Confidence    float64
}

// Answers maps each Question's ID to its Answer.
type Answers map[string]Answer

// Decider answers bounded, typed questions about a state and returns calibrated
// probabilities. It is deliberately NOT a ModelProvider: it generates no text, so
// forcing it behind Complete would mean synthesising a tool call and discarding the
// probabilities, which is the mismatch it exists to avoid. It also means content
// embedded in the state cannot instruct it.
//
// Questions in one call are evaluated independently against the same state, so a
// caller needing two judgements pays one round trip.
type Decider interface {
	Decide(ctx context.Context, state string, qs []Question) (Answers, error)
}

// CompletionRequest / CompletionResponse are the minimal LLM exchange types.
type CompletionRequest struct {
	System   string
	Messages []Message
	Tools    []ToolSpec
	// ToolChoice optionally names one tool from Tools that the model MUST call on
	// this turn ("" = provider default: the model chooses freely between prose and
	// any tool). Set it on structured-output turns — submit_verdicts, submit_review,
	// submit_grade, and the post-budget-nudge submit_findings — where a prose reply
	// is never acceptable; leave it empty on normal investigation steps so the model
	// keeps the freedom to pick tools or answer.
	ToolChoice string
}

// Message is one turn in an LLM exchange.
type Message struct {
	Role       string // system | user | assistant | tool
	Content    string
	ToolCalls  []ToolCall // assistant turn requesting tools
	ToolCallID string     // tool turn: the call this answers
	// Opaque is provider-specific content the client must replay verbatim; produced
	// and consumed only by the same provider, empty otherwise. The loop carries it
	// from a completion's CompletionResponse.Opaque onto the assistant turn it stores
	// in history, so the same provider can prepend it on the next request. Currently
	// the Anthropic client uses it to replay signed adaptive-thinking blocks; other
	// providers ignore it.
	Opaque json.RawMessage
}

// ToolSpec describes a tool offered to the model.
type ToolSpec struct {
	Name        string
	Description string
	Schema      string // JSON Schema
}

// CompletionResponse is the model's reply (text and/or tool calls).
//
// An ERROR return is not necessarily the zero value. A provider reports token
// counts DURING the stream — Anthropic's input usage arrives on the first event
// of all — so a completion that fails after generating (and being billed for)
// thousands of tokens already knows what it cost. Every client in this repo
// therefore returns that cost alongside the error: Usage carries whatever the
// fold observed before it failed, and Attempts how many upstream requests the
// failed exchange took. A caller charging a spend budget bills those; charging
// zero would make a flaky endpoint look free while the bill grows.
//
// EVERY OTHER FIELD MUST BE TREATED AS UNUSABLE when err != nil. Text,
// ToolCalls, StopReason, Truncated and Opaque describe a reply that did not
// happen: the clients here zero them on an error return, and no caller may read
// them off a failed call regardless.
type CompletionResponse struct {
	Text      string
	ToolCalls []ToolCall
	// Usage is the provider-reported token count for this completion. Zero when
	// the provider omits it (older endpoints, or a provider that does not report
	// it), and zero on a failure that happened before the provider reported
	// anything — callers treat the zero value as "unknown", not "zero tokens".
	Usage Usage
	// Truncated is true when the provider stopped because it hit the output-token
	// ceiling (Anthropic stop_reason "max_tokens", OpenAI finish_reason "length",
	// Gemini finishReason "MAX_TOKENS"). It distinguishes a cut-off answer from a
	// complete one, so the loop need not treat a truncated reply as final.
	Truncated bool
	// StopReason is the provider's raw turn-termination reason, normalized to the
	// provider's own vocabulary (Anthropic stop_reason, OpenAI finish_reason, Gemini
	// finishReason). It is empty when the provider omits it. Refused() interprets it;
	// the loop uses Refused() rather than matching strings itself.
	StopReason string
	// Opaque is provider-specific content the client must replay verbatim; produced
	// and consumed only by the same provider, empty otherwise. The loop copies it onto
	// the assistant Message it appends to history so the same provider can replay it on
	// the next request. The Anthropic client serializes completed adaptive-thinking
	// (and redacted_thinking) blocks — in order, with their signatures — into it;
	// OpenAI/Gemini leave it empty and ignore it.
	Opaque json.RawMessage
	// Attempts is how many upstream HTTP requests this completion actually cost:
	// 1 normally, more when the client retried a transient failure (a network
	// error, 429 or 5xx) before this one succeeded. Usage describes only the
	// attempt that returned a body, so a caller charging a spend budget needs
	// this to bill the ones that failed — a provider bills every request it
	// accepted. 0 when the client does not report it (see AttemptsOf on the
	// error path): callers read 0 as "unknown", i.e. at least one.
	Attempts int
}

// CostOnly reduces a response to what the exchange COST — its Usage and
// Attempts — dropping every field that describes a reply. It is what a client
// returns alongside an error: the token counts the provider reported before the
// failure are real and billed, while a half-folded Text or an unterminated tool
// call is not an answer and must never reach a caller. See CompletionResponse.
func (r CompletionResponse) CostOnly() CompletionResponse {
	return CompletionResponse{Usage: r.Usage, Attempts: r.Attempts}
}

// refusalStopReasons is the set of stop reasons (across providers, lower-cased) that
// mean the model declined the request on safety/policy grounds rather than producing
// an answer. Anthropic emits "refusal"; OpenAI "content_filter"; Gemini "SAFETY",
// "PROHIBITED_CONTENT", "BLOCKLIST", "SPII".
var refusalStopReasons = map[string]bool{
	"refusal":            true,
	"content_filter":     true,
	"safety":             true,
	"prohibited_content": true,
	"blocklist":          true,
	"spii":               true,
}

// Refused reports whether the model declined the request on safety/policy grounds
// (a successful response with no usable answer) rather than terminating normally.
// The comparison is case-insensitive so a provider's casing (e.g. Gemini's "SAFETY")
// does not matter. The loop treats a refusal as a first-class unresolved outcome.
func (r CompletionResponse) Refused() bool {
	return refusalStopReasons[strings.ToLower(r.StopReason)]
}

// Usage is the provider-reported token accounting for one completion.
type Usage struct {
	InputTokens  int `json:"input_tokens"`  // total prompt/input tokens billed, INCLUDING any served from cache (normalized across providers)
	OutputTokens int `json:"output_tokens"` // generated/output tokens in the reply
	// CachedInputTokens is the subset of InputTokens that was a cache READ (Anthropic
	// cache_read_input_tokens, OpenAI prompt_tokens_details.cached_tokens, Gemini
	// cachedContentTokenCount) — the saving. 0 when the provider reports none.
	CachedInputTokens int `json:"cached_input_tokens"`
	// CacheWriteTokens is input tokens WRITTEN to the cache this request (Anthropic
	// cache_creation_input_tokens, billed ~1.25x). 0 for providers that don't report it.
	CacheWriteTokens int `json:"cache_write_tokens"`
}

// Total is the billable token count for one completion: input plus output.
// Neither cache field is added — both CachedInputTokens and CacheWriteTokens
// are subsets of InputTokens after normalization (the Anthropic client, the
// only one reporting a cache write, sets InputTokens to input + cache_read +
// cache_creation), so adding either would double-count. It returns int64
// because every consumer accumulates or reports in that width.
//
// A zero Usage totals 0, which callers must not read as "this call was free":
// see CompletionResponse.Usage on the zero value meaning "unknown".
func (u Usage) Total() int64 {
	return int64(u.InputTokens) + int64(u.OutputTokens)
}

// EstimateTokens approximates a request's input size (~4 chars/token) over
// everything that actually goes over the wire: the system prompt, the full tool
// specs (name + description + JSON Schema), and the message history — including
// the assistant tool-call JSON (m.ToolCalls[].Args). Counting only m.Content
// systematically under-estimates a tool-heavy request. It ignores JSON envelope
// and role overhead, so it stays a mild under-estimate of the true wire size,
// but the right order of magnitude.
//
// It lives here because two callers need a request's size where Usage cannot
// supply one, for opposite reasons: internal/investigate sizes the NEXT request
// against its per-investigation budget, BEFORE any response exists (and
// calibrates this heuristic against provider-reported usage afterwards), while
// internal/thread charges its chat budget an estimate precisely when a
// completion came back with no usage at all — CompletionResponse.Usage's zero
// value meaning "unknown", not "zero tokens".
func EstimateTokens(system string, msgs []Message, tools []ToolSpec) int {
	n := len(system)
	for _, t := range tools {
		n += len(t.Name) + len(t.Description) + len(t.Schema)
	}
	for _, m := range msgs {
		n += len(m.Content)
		for _, tc := range m.ToolCalls {
			n += len(tc.Args)
		}
	}
	return n / 4
}

// ToolCall is a model request to invoke a tool.
type ToolCall struct {
	ID   string
	Name string
	Args string // JSON
}
