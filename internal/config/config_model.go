// SPDX-License-Identifier: Apache-2.0

package config

// Investigation holds cost/throughput controls on the alert→investigation→LLM path.
type Investigation struct {
	Coalesce                  Coalesce  `yaml:"coalesce"`
	RateLimit                 RateLimit `yaml:"rate_limit"`
	MaxSteps                  int       `yaml:"max_steps"`                    // 0 ⇒ loop default (20)
	MaxToolOutputBytes        int       `yaml:"max_tool_output_bytes"`        // unset/0 ⇒ bounded default (32768); -1 ⇒ unlimited
	MaxTokensPerInvestigation int       `yaml:"max_tokens_per_investigation"` // CUMULATIVE token ceiling for one investigation, and a quarter of it bounds any single request; unset/0 ⇒ bounded default (400000); -1 ⇒ unlimited
	Timeout                   Duration  `yaml:"timeout"`                      // per-investigation deadline; 0 ⇒ default (10m) via ApplyDefaults
	ToolTimeout               Duration  `yaml:"tool_timeout"`                 // per-TOOL-call timeout so one hung tool can't eat the budget; 0 ⇒ default (60s) at construction

	// MaxCostPerInvestigation caps one investigation's ESTIMATED spend in USD, priced
	// from model.pricing (and model.verify.pricing for the verify pass). It feeds the
	// same nudge-then-hard-stop ladder as the token ceiling.
	//
	// OPT-IN, with no default and no -1 opt-out — deliberately unlike the token key
	// above. A token is provider-neutral, so 400000 means the same thing on every
	// deployment and can be chosen on an operator's behalf. A dollar cannot: a
	// self-hosted operator picks their own model and supplies their own rates, so any
	// shipped USD figure would be generous for one deployment and punitive for the
	// next. 0 already means "no ceiling", which leaves -1 nothing to express; a
	// negative value is rejected by Validate rather than quietly read as an opt-out.
	//
	// INERT without model.pricing — no cost is computed, so nothing can be compared.
	// app.CostCeilingWithoutPricingWarning says so at startup.
	MaxCostPerInvestigation float64 `yaml:"max_cost_per_investigation"` // unset/0 ⇒ no cost ceiling

	// RecurrenceCooldown (opt-in, 0 = off) suppresses re-investigating a trigger
	// whose previous investigation completed less than this long ago, provided some
	// prior investigation of it reached a conclusion (a verdict other than
	// inconclusive — not necessarily the most recent one) and no 👎 feedback stands
	// against it. Without it a still-firing alert re-investigates on every
	// Alertmanager repeat_interval and a persistently-failing GitOps resource on
	// every informer resync (~10m).
	// Requires outcome.ledger_path (the gate reads the ledger's trigger index).
	RecurrenceCooldown Duration `yaml:"recurrence_cooldown"`

	// Compaction selects how mid-loop history compaction treats the tool outputs it
	// elides once the estimate crosses the compaction target. "" / "elide" is the
	// default: drop their bodies for short markers (lossy). "summarize" first asks a
	// model (the verify-tier model when configured, else the main model) for one
	// compact factual digest of the batch and keeps that in place of the markers,
	// falling back to plain elision on any summarizer error/refusal/truncation.
	Compaction string `yaml:"compaction"` // "" | "elide" (default) | "summarize"

	// PodLogNamespaces lists extra namespaces (beyond the incident's own) that
	// pod_logs may read controller/crash logs from. pod_logs streams raw pod logs
	// (which carry secrets/PII) to the external LLM, so the model is constrained to
	// the incident namespace plus this allowlist at the application layer — not just
	// by Kubernetes RBAC. Set this to match the Helm rbac.controllerLogNamespaces
	// (e.g. [flux-system]); empty means the incident namespace only.
	PodLogNamespaces []string `yaml:"pod_log_namespaces"`

	// ProgressUpdates opts into interim progress notifications during a long
	// investigation. Off by default (zero behaviour change, zero extra model calls).
	ProgressUpdates ProgressUpdates `yaml:"progress_updates"`
}

// ProgressUpdates configures opt-in interim progress notifications: a long
// investigation (up to 20 steps) is otherwise silent until the final message.
// Off by default. When enabled, the loop emits one ping every EverySteps steps
// to any notifier that supports it (Slack first); a ping is best-effort and never
// fails the investigation.
type ProgressUpdates struct {
	Enabled    bool `yaml:"enabled"`
	EverySteps int  `yaml:"every_steps"` // emit a ping every N steps; 0 ⇒ default 5 (ApplyDefaults). Must be > 0 when enabled.
}

// Coalesce folds correlated incidents into one investigation.
type Coalesce struct {
	Enabled           bool     `yaml:"enabled"`
	Debounce          Duration `yaml:"debounce"`
	MaxWait           Duration `yaml:"max_wait"`
	MaxBatch          int      `yaml:"max_batch"`
	Cooldown          Duration `yaml:"cooldown"`
	CorrelationLabels []string `yaml:"correlation_labels"` // empty ⇒ AM groupKey, else namespace+label values
}

// RateLimit caps investigation starts per sliding window.
type RateLimit struct {
	// MaxPerWindow caps investigation STARTS per Window. nil (unset) defaults to
	// 30 (see ApplyDefaults) — a cost-DoS guard, since per-incident spend is
	// bounded but the count of incidents was not. An EXPLICIT 0 preserves the
	// pre-default unlimited behavior for configs that opted into it.
	MaxPerWindow *int     `yaml:"max_per_window"`
	Window       Duration `yaml:"window"`
	MaxRequeues  int      `yaml:"max_requeues"` // drop a key after this many backoff requeues
}

// DecisionModel configures a System One decision model — a backend that answers one
// bounded, typed question and returns a calibrated probability. It is deliberately NOT
// nested under `model:`: it is a different protocol from a different vendor answering a
// different question, and none of model's vocabulary (max_tokens, effort, thinking)
// means anything to a model that emits no tokens.
//
// Enabled is a kill switch rather than a presence check, unlike model.verify and
// model.chat. An operator has to be able to stop every decision call during an incident
// without also deleting the endpoint and key-env config, so a consumer still pointing
// here falls back with a warning instead of failing validation. See Validate.
type DecisionModel struct {
	Enabled   bool   `yaml:"enabled"`
	Provider  string `yaml:"provider"`    // "typesafe" is the only implementation
	BaseURL   string `yaml:"base_url"`    // required when enabled
	Model     string `yaml:"model"`       // e.g. jev-latest
	APIKeyEnv string `yaml:"api_key_env"` // env var NAME; empty = keyless behind a gateway
}

// Model configures the LLM used for investigation. Provider selects the wire
// protocol: "openai" (default, OpenAI-compatible: vLLM/Ollama/OpenAI) or
// "anthropic" (native Messages API). When unconfigured, serve uses the log-only
// investigator.
type Model struct {
	Provider  string `yaml:"provider"`    // "openai" (default) | "anthropic" | "gemini"
	BaseURL   string `yaml:"base_url"`    // OpenAI: required; Anthropic/Gemini: optional (built-in default endpoint). Must be https when api_key_env is set on a public host (validated).
	Model     string `yaml:"model"`       // model name
	APIKeyEnv string `yaml:"api_key_env"` // env var holding the API key (empty = keyless)
	// MaxTokens caps the model's output (generated) tokens per request. 0 = use the
	// 8192 default. Streaming providers send it (Anthropic max_tokens, OpenAI
	// max_tokens, Gemini generationConfig.maxOutputTokens); a too-low value truncates.
	MaxTokens int `yaml:"max_tokens"`
	// Effort opts into deeper model reasoning per request. Provider-specific
	// vocabulary, validated at startup: anthropic low|medium|high|max (sent as
	// output_config.effort), openai minimal|low|medium|high (sent as
	// reasoning_effort), gemini minimal|low|medium|high|xhigh (sent uppercase as
	// generationConfig.thinkingConfig.thinkingLevel). Empty = omitted from
	// requests (preserving each model's native default). Models that don't
	// support a level (e.g. minimal on Gemini 4) return a 400, which is
	// classified permanent.
	Effort string `yaml:"effort"`
	// Thinking opts into adaptive extended thinking, sent as thinking:{type:"adaptive"}.
	// Only value is "adaptive"; only supported for provider anthropic (the client
	// replays the signed thinking blocks across the tool loop). Empty = omitted from
	// requests (today's behavior, byte-for-byte). Validated at startup; any other value
	// or any non-anthropic provider is a clear config error. Interacts with effort:
	// both may be set (effort is soft guidance for how much thinking Claude does).
	// Give max_tokens headroom — thinking consumes output tokens.
	Thinking string `yaml:"thinking"`
	// Verify optionally routes the adversarial verify pass to a cheaper/faster model;
	// unset fields inherit from the parent above (so `verify: {model: <cheap>}` reuses
	// the same provider/endpoint/key). Absent ⇒ verify runs on the main model.
	Verify *ModelOverride `yaml:"verify"`
	// Chat routes the thread-conversation layer to its own (cheaper) model.
	// PRESENT ⇒ conversational replies are enabled; ABSENT ⇒ thread capture
	// stays deterministic note-capture only. Same inherit-when-empty semantics
	// as Verify — with one deliberate exception: `model` may not be empty.
	//
	// Verify inherits the investigation model harmlessly, because verify runs
	// once per investigation on a path RunLore itself initiates. Chat runs once
	// per addressed message, on a path any channel or room member can trigger.
	// Silently inheriting a frontier model there makes the cheapest way to turn
	// the feature on the most expensive way to run it, so Validate requires the
	// model be named explicitly.
	Chat *ModelOverride `yaml:"chat"`
	// Embeddings optionally configures an OpenAI-compatible /embeddings endpoint used
	// for hybrid recall (instant_recall.hybrid). Unset ⇒ BM25-only recall.
	Embeddings *Embeddings `yaml:"embeddings"`
	// Pricing optionally sets token rates so RunLore can estimate and report a
	// per-investigation cost. Unset ⇒ token totals are reported without a dollar
	// figure. Rates are validated non-negative.
	Pricing *Pricing `yaml:"pricing"`
}

// Pricing sets model token rates in USD per MILLION tokens, used to estimate a
// per-investigation cost. All rates are optional and default to 0; a rate must be
// non-negative.
type Pricing struct {
	InputUSDPerMTok       float64 `yaml:"input_usd_per_mtok"`
	OutputUSDPerMTok      float64 `yaml:"output_usd_per_mtok"`
	CachedInputUSDPerMTok float64 `yaml:"cached_input_usd_per_mtok"`
}

// Embeddings configures an OpenAI-compatible /embeddings endpoint (vLLM/Ollama/
// OpenAI) for hybrid catalog recall — served by the same kind of endpoint as the
// model; keyless when APIKeyEnv is empty.
type Embeddings struct {
	BaseURL   string `yaml:"base_url"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env"`
}

// ModelOverride is a partial Model used to route the verify pass to a cheaper model;
// empty fields inherit from the parent Model.
type ModelOverride struct {
	Provider  string `yaml:"provider"`
	BaseURL   string `yaml:"base_url"`
	Model     string `yaml:"model"`
	APIKeyEnv string `yaml:"api_key_env"`
	// MaxTokens overrides the parent's effective output-token cap for the verify pass;
	// 0 inherits the parent's effective value.
	MaxTokens int `yaml:"max_tokens"`
	// Effort overrides the parent's effort for the verify pass; empty inherits the
	// parent's value (same vocabulary and validation as model.effort).
	Effort string `yaml:"effort"`
	// Thinking overrides the parent's thinking mode for the verify pass; empty inherits
	// the parent's value (same vocabulary and validation as model.thinking). Note the
	// verify pass always forces a tool_choice, so the Anthropic client drops thinking
	// for that request anyway — this knob only affects any non-forced verify calls.
	//
	// On model.chat it takes effect NOWHERE, not merely usually: Chat.Answer always
	// sets a ToolChoice (the single forced submit_thread_reply tool is what makes a
	// reply one call instead of an agent loop), the Anthropic client gates thinking
	// on an empty ToolChoice, and the gemini and openai clients are never handed a
	// thinking parameter at all. So model.chat.thinking — or a model.thinking
	// inherited onto it — is inert on every provider. Validate still rejects an
	// invalid value: a knob that is silently dropped is bad enough without also
	// accepting a mode that does not exist.
	Thinking string `yaml:"thinking"`
	// Pricing overrides the parent's token rates for the verify pass (a cheaper
	// verify model has its own cost); nil inherits model.pricing.
	//
	// On model.chat NOTHING reads it: per-investigation cost reporting covers the
	// main and verify passes only, and the chat path emits token counts
	// (runlore_thread_chat_tokens_total) with no currency conversion anywhere. An
	// operator therefore cannot graph what conversational replies spent in dollars
	// — not merely "there is no cost ceiling", there is no cost REPORT. Validate
	// still rejects a negative rate here, for the same reason it does above.
	Pricing *Pricing `yaml:"pricing"`
}
