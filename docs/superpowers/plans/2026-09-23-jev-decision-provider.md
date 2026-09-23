# Decision Provider Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Let the recall reranker and curation dedup answer their one bounded question with a System One decision model (TypeSafe Jev) instead of a forced-tool LLM call, behind a shadow mode that proves agreement before anything switches.

**Architecture:** A new `Decider` provider kind takes a state string plus typed questions and returns typed answers with calibrated confidences. One implementation speaks TypeSafe's HTTP API, modelled on `internal/embed` (plain JSON over `httpx.SecureClient` + `DoWithRetry`, no SDK). Two consumers opt in per call site; every failure path falls through to today's behaviour, so a decider can never be the reason an incident goes uninvestigated.

**Tech Stack:** Go 1.26, stdlib `net/http` + `encoding/json`, `internal/httpx`, `internal/redact`, `internal/telemetry` (OpenTelemetry), `httptest` for fixtures.

**Spec:** [`docs/superpowers/specs/2026-09-22-jev-decision-provider-design.md`](../specs/2026-09-22-jev-decision-provider-design.md)

## Global Constraints

- **Off by default.** Every consumer keeps its current backend when `decision_model:` is absent or `enabled: false`. No behaviour changes without an explicit opt-in.
- **`enabled: false` wins and never errors.** It is a kill switch for use during an incident; a consumer still pointing at `jev` falls back with one warning per consumer at startup, not a validation failure.
- **Credentials are `api_key_env` only** — the name of an environment variable. `deploy/helm/runlore/templates/configmap.yaml:23` renders the whole `config:` block verbatim, so a literal key would sit in a ConfigMap in plaintext.
- **No new third-party dependency.** Hand-rolled client, matching the repo's single-static-binary rule. TypeSafe ships no Go SDK.
- **Fall through, never fail.** Reranker: a full investigation. Dedup: the BM25 path. No circuit breaker, no second backend per call.
- **`noul` is TypeSafe's own name** for its boolean primitive (returns a probability in `[0,1]`). Carry it verbatim into the wire types; it is not a typo for `bool`.
- **Request budget:** 64k tokens per request, of which state plus the longest question may use 32k. Every state here is a few kilobytes, so no chunking is needed — but the client must not silently send more.
- **Redact before the wire.** `redact.Secrets` on every state, as at the model boundary.
- **Quality gate, run before every commit:** `go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh` — `gofmt -l .` prints nothing, `hack/lint.sh` reports `0 issues`.
- **Never co-author commits.**

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/providers/providers.go` (modify) | The `Decider` interface and its `Question` / `Answer` / `Answers` types, beside the other provider contracts |
| `internal/decide/typesafe/typesafe.go` (create) | The TypeSafe client: wire types, one POST, answer mapping |
| `internal/decide/typesafe/typesafe_test.go` (create) | `httptest` fixtures per primitive plus every failure shape |
| `internal/config/config.go` (modify) | The `decision_model:` block, `forge` dedup keys, `rerank_backend`, defaults and validation |
| `internal/telemetry/metrics.go` (modify) | Two instruments: confidence histogram, shadow-agreement counter |
| `internal/telemetry/setup.go` (modify) | Register the confidence histogram's buckets (it is a probability, not a BM25 score) |
| `internal/investigate/rerank.go` (modify) | Backend selection, the Jev question, the shadow arm |
| `internal/app/investigate.go` (modify) | Build the decider once and wire it into the reranker |
| `internal/app/curator.go` (modify) | Wire the decider into the curator |
| `internal/curator/curator.go` (modify) | The three dedup tiers |
| `internal/eval/replay_report.go`, `internal/eval/scorecard.go` (modify) | Shadow agreement in the report and the scorecard |

---

## Task 1: The `Decider` contract and the TypeSafe client

**Files:**
- Modify: `internal/providers/providers.go` (add after the `ModelProvider` interface, around line 1491)
- Create: `internal/decide/typesafe/typesafe.go`
- Test: `internal/decide/typesafe/typesafe_test.go`

**Interfaces:**
- Consumes: `internal/httpx` (`SecureClient`, `DoWithRetry`, `Drain`, `RequestID`), `internal/redact` (`Secrets`).
- Produces: `providers.Decider` with `Decide(ctx, state string, qs []providers.Question) (providers.Answers, error)`; `providers.Question{ID, Kind, Instructions, Choices, Levels}`; `providers.QuestionKind` constants `KindChoice`/`KindScore`/`KindNoul`; `providers.Answer{Choice, Noul, Score, Probabilities, Confidence}`; `providers.Answers` as `map[string]Answer`; `typesafe.New(baseURL, model, apiKey string) *typesafe.Client`.

- [ ] **Step 1: Write the failing test for a choice answer**

Create `internal/decide/typesafe/typesafe_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package typesafe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fixtureServer answers one POST with the given status and body, and captures the
// request it received so a test can assert what went on the wire.
func fixtureServer(t *testing.T, status int, body string) (*httptest.Server, *string) {
	t.Helper()
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &got
}

func TestDecideChoiceCarriesConfidenceAndProbabilities(t *testing.T) {
	srv, sent := fixtureServer(t, http.StatusOK, `{
	  "model":"jev-1.13.0",
	  "answers":{"pick":{"type":"choice","choice":"runbook-b","confidence":0.84,
	    "probabilities":{"runbook-a":0.16,"runbook-b":0.84,"none":0.0}}},
	  "usage":{"input_tokens":312,"output_tokens":48}
	}`)

	c := New(srv.URL, "jev-latest", "")
	ans, err := c.Decide(context.Background(), "an incident", []providers.Question{{
		ID:           "pick",
		Kind:         providers.KindChoice,
		Instructions: "which runbook",
		Choices:      map[string]string{"runbook-a": "first", "runbook-b": "second", "none": "no match"},
	}})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	a, ok := ans["pick"]
	if !ok {
		t.Fatalf("no answer for the question id, got %+v", ans)
	}
	if a.Choice != "runbook-b" || a.Confidence != 0.84 {
		t.Fatalf("want runbook-b at 0.84, got %+v", a)
	}
	if a.Probabilities["runbook-a"] != 0.16 {
		t.Fatalf("probabilities not carried: %+v", a.Probabilities)
	}

	// The wire shape is the vendor's, not ours: criteria is a map for a choice.
	var req struct {
		State     string `json:"state"`
		Model     string `json:"model"`
		Questions map[string]struct {
			Type     string            `json:"type"`
			Criteria map[string]string `json:"criteria"`
		} `json:"questions"`
	}
	if err := json.Unmarshal([]byte(*sent), &req); err != nil {
		t.Fatalf("request is not the documented shape: %v\n%s", err, *sent)
	}
	if req.Model != "jev-latest" || req.State != "an incident" {
		t.Fatalf("state/model not sent: %+v", req)
	}
	if q := req.Questions["pick"]; q.Type != "choice" || len(q.Criteria) != 3 {
		t.Fatalf("choice criteria not sent as a map: %+v", q)
	}
}
```

- [ ] **Step 2: Run it to verify it fails**

Run: `go test ./internal/decide/typesafe/ -run TestDecideChoiceCarriesConfidenceAndProbabilities`
Expected: FAIL to build — `undefined: New`, `undefined: providers.KindChoice`.

- [ ] **Step 3: Add the contract to `internal/providers/providers.go`**

Insert immediately after the `ModelProvider` interface (it ends around line 1491), so the contract sits with its siblings:

```go
// QuestionKind is the primitive a Question asks for. The names are the wire values
// of TypeSafe's System One API, carried verbatim — "noul" is that vendor's own name
// for its boolean primitive and is not a typo for "bool".
type QuestionKind string

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
```

- [ ] **Step 4: Write the client**

Create `internal/decide/typesafe/typesafe.go`. Note `decidePath`: the docs page gives `/v1/systemone` while the Python SDK reference shows `/system_one`. It is a `const` for that reason — **confirm against the live API before the first real call and correct it here if needed**; a wrong path shows up as a 404 from `Decide`, not as a silent wrong answer.

```go
// SPDX-License-Identifier: Apache-2.0

// Package typesafe speaks TypeSafe's System One HTTP API, the first implementation of
// providers.Decider. Hand-rolled over net/http like internal/embed and the model
// clients: the vendor ships no Go SDK, and the single static binary is the point.
package typesafe

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
	"github.com/Smana/runlore/internal/redact"
)

// decidePath is the System One route. The published docs and the Python SDK reference
// disagree on its spelling ("/v1/systemone" vs "/system_one"); confirm against the live
// API. A wrong value fails loudly with a 404 rather than answering wrongly.
const decidePath = "/v1/systemone"

// maxStateBytes is a local guard, not the vendor's limit. The documented budget is 64k
// tokens per request with 32k for the state plus the longest question; every state this
// repo sends is a few kilobytes, so anything approaching the budget means a caller built
// its state wrong. Refusing beats paying for a truncated judgement.
const maxStateBytes = 60_000

// Client is a Decider backed by TypeSafe.
type Client struct {
	baseURL string
	model   string
	apiKey  string
	http    *http.Client
}

// New builds a client. apiKey may be empty for a gateway that injects the credential.
func New(baseURL, model, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  apiKey,
		http:    httpx.SecureClient(30 * time.Second),
	}
}

type wireQuestion struct {
	Type         string `json:"type"`
	Instructions string `json:"instructions"`
	// Criteria is the vendor's polymorphic field: a map of option id to criterion for a
	// choice, an ordered []string for a score, absent for a noul.
	Criteria any `json:"criteria,omitempty"`
}

type wireRequest struct {
	State     string                  `json:"state"`
	Model     string                  `json:"model"`
	Questions map[string]wireQuestion `json:"questions"`
}

type wireAnswer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice"`
	Noul          float64            `json:"noul"`
	Score         float64            `json:"score"`
	Probabilities map[string]float64 `json:"probabilities"`
	Confidence    float64            `json:"confidence"`
}

type wireResponse struct {
	Model   string                `json:"model"`
	Answers map[string]wireAnswer `json:"answers"`
}

// Decide sends one request carrying every question and maps the typed answers back.
// The state is redacted first: it is assembled from incident text and catalog content,
// and this is an egress boundary exactly like the model clients'.
func (c *Client) Decide(ctx context.Context, state string, qs []providers.Question) (providers.Answers, error) {
	if len(qs) == 0 {
		return nil, fmt.Errorf("no questions")
	}
	state = redact.Secrets(state)
	if len(state) > maxStateBytes {
		return nil, fmt.Errorf("state is %d bytes, over the %d-byte guard: build a smaller state rather than paying for a truncated judgement", len(state), maxStateBytes)
	}
	wq := make(map[string]wireQuestion, len(qs))
	for _, q := range qs {
		w := wireQuestion{Type: string(q.Kind), Instructions: q.Instructions}
		switch q.Kind {
		case providers.KindChoice:
			if len(q.Choices) == 0 {
				return nil, fmt.Errorf("question %q is a choice with no options", q.ID)
			}
			w.Criteria = q.Choices
		case providers.KindScore:
			if len(q.Levels) == 0 {
				return nil, fmt.Errorf("question %q is a score with no levels", q.ID)
			}
			w.Criteria = q.Levels
		case providers.KindNoul:
			// no criteria: the instructions ARE the statement being judged
		default:
			return nil, fmt.Errorf("question %q has unknown kind %q", q.ID, q.Kind)
		}
		wq[q.ID] = w
	}
	body, err := json.Marshal(wireRequest{State: state, Model: c.model, Questions: wq})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+decidePath, bytes.NewReader(body))
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
		return nil, fmt.Errorf("decide request: %w", err)
	}
	defer func() { httpx.Drain(resp.Body); _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// base_url is operator-configurable, so never echo the upstream body: it is an
		// information-disclosure and log-injection surface. Status + request id only,
		// matching internal/embed.
		return nil, fmt.Errorf("decide status %d (request-id %q)", resp.StatusCode, httpx.RequestID(resp.Header))
	}
	var wr wireResponse
	if err := json.Unmarshal(data, &wr); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	if len(wr.Answers) == 0 {
		return nil, fmt.Errorf("response carried no answers")
	}
	out := make(providers.Answers, len(wr.Answers))
	for id, a := range wr.Answers {
		out[id] = providers.Answer{
			Choice:        a.Choice,
			Noul:          a.Noul,
			Score:         a.Score,
			Probabilities: a.Probabilities,
			Confidence:    a.Confidence,
		}
	}
	return out, nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/decide/typesafe/ -run TestDecideChoiceCarriesConfidenceAndProbabilities -v`
Expected: PASS.

- [ ] **Step 6: Write the failing tests for noul and every failure shape**

Append to `internal/decide/typesafe/typesafe_test.go`:

```go
func TestDecideNoulIsItsOwnProbability(t *testing.T) {
	srv, _ := fixtureServer(t, http.StatusOK,
		`{"model":"jev-1.13.0","answers":{"same":{"type":"noul","noul":0.93}}}`)
	c := New(srv.URL, "jev-latest", "k")
	ans, err := c.Decide(context.Background(), "two entries", []providers.Question{{
		ID: "same", Kind: providers.KindNoul, Instructions: "these describe the same incident pattern",
	}})
	if err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if ans["same"].Noul != 0.93 {
		t.Fatalf("want noul 0.93, got %+v", ans["same"])
	}
}

func TestDecideFailureShapes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"a rate limit is an error, not a retry storm", http.StatusTooManyRequests, `{"error":"slow down"}`, "decide status 429"},
		{"a server error surfaces status only", http.StatusInternalServerError, `{"error":"boom"}`, "decide status 500"},
		{"a malformed body is an error", http.StatusOK, `{"answers":`, "parse response"},
		{"an empty answer set is an error", http.StatusOK, `{"model":"jev","answers":{}}`, "no answers"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, _ := fixtureServer(t, tc.status, tc.body)
			c := New(srv.URL, "jev-latest", "")
			_, err := c.Decide(context.Background(), "s", []providers.Question{{
				ID: "q", Kind: providers.KindNoul, Instructions: "i",
			}})
			if err == nil {
				t.Fatal("want an error")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want an error containing %q, got %v", tc.want, err)
			}
			if strings.Contains(err.Error(), "boom") || strings.Contains(err.Error(), "slow down") {
				t.Fatalf("the upstream body must not reach the error: %v", err)
			}
		})
	}
}

func TestDecideRejectsMalformedQuestionsBeforeTheWire(t *testing.T) {
	c := New("http://unused", "jev-latest", "")
	for _, tc := range []struct {
		name string
		q    providers.Question
		want string
	}{
		{"a choice with no options", providers.Question{ID: "q", Kind: providers.KindChoice}, "no options"},
		{"a score with no levels", providers.Question{ID: "q", Kind: providers.KindScore}, "no levels"},
		{"an unknown kind", providers.Question{ID: "q", Kind: "guess"}, "unknown kind"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := c.Decide(context.Background(), "s", []providers.Question{tc.q}); err == nil ||
				!strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDecideRedactsTheStateAndGuardsItsSize(t *testing.T) {
	srv, sent := fixtureServer(t, http.StatusOK,
		`{"model":"jev","answers":{"q":{"type":"noul","noul":0.1}}}`)
	c := New(srv.URL, "jev-latest", "")
	q := []providers.Question{{ID: "q", Kind: providers.KindNoul, Instructions: "i"}}

	if _, err := c.Decide(context.Background(), "token ghp_0123456789abcdefghij0123456789abcdefg", q); err != nil {
		t.Fatalf("Decide: %v", err)
	}
	if strings.Contains(*sent, "ghp_0123456789abcdefghij0123456789abcdefg") {
		t.Fatalf("a secret-shaped value reached the wire: %s", *sent)
	}
	if _, err := c.Decide(context.Background(), strings.Repeat("x", maxStateBytes+1), q); err == nil ||
		!strings.Contains(err.Error(), "guard") {
		t.Fatalf("want the size guard to refuse, got %v", err)
	}
}
```

- [ ] **Step 7: Run them**

Run: `go test ./internal/decide/typesafe/ -v`
Expected: all PASS. If `TestDecideRedactsTheStateAndGuardsItsSize` fails on the redaction assertion, the `redact.Secrets` call is missing or placed after the marshal.

- [ ] **Step 8: Run the quality gate and commit**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
git add internal/providers/providers.go internal/decide/
git commit -m "feat(providers): add a Decider contract and a TypeSafe System One client

A decision model answers one bounded question and returns a calibrated
probability. It is not a ModelProvider: it generates no text, so putting it
behind Complete would mean synthesising a tool call and throwing the
probabilities away, which is the mismatch it exists to avoid.

The client is hand-rolled over net/http like internal/embed and the model
clients, because the vendor ships no Go SDK and the single static binary is the
point. The state is redacted before the wire, an upstream error body never
reaches a log line, and a state approaching the documented 32k budget is
refused rather than silently truncated."
```

---

## Task 2: Config — the `decision_model:` block

**Files:**
- Modify: `internal/config/config.go` (new `DecisionModel` struct; `InstantRecall.RerankBackend` + `RerankThresholdJev`; `Forge.DedupBackend` + band edges; `ApplyDefaults`; `Validate`)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: nothing from Task 1 at compile time.
- Produces: `config.DecisionModel{Enabled bool, Provider, BaseURL, Model, APIKeyEnv string}` at `Config.DecisionModel`; `config.InstantRecall.RerankBackend string` and `.RerankThresholdJev float64`; `config.Forge.DedupBackend string`, `.DedupSkipAbove float64`, `.DedupAnnotateAbove float64`; helper `func (c *Config) DecisionModelUsable() bool`.

- [ ] **Step 1: Write the failing validation tests**

Append to `internal/config/config_test.go`:

```go
func TestDecisionModelValidation(t *testing.T) {
	for _, tc := range []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "enabled with no endpoint is a config error",
			mutate: func(c *Config) {
				c.DecisionModel = DecisionModel{Enabled: true, Provider: "typesafe"}
			},
			wantErr: "decision_model.base_url",
		},
		{
			name: "the jev reranker needs its own threshold",
			mutate: func(c *Config) {
				c.DecisionModel = DecisionModel{Enabled: true, Provider: "typesafe", BaseURL: "https://api.typesafe.ai", Model: "jev-latest"}
				c.Catalog.InstantRecall.Enabled = true
				c.Catalog.InstantRecall.RerankBackend = "jev"
			},
			wantErr: "rerank_threshold_jev",
		},
		{
			name: "jev dedup needs both band edges",
			mutate: func(c *Config) {
				c.DecisionModel = DecisionModel{Enabled: true, Provider: "typesafe", BaseURL: "https://api.typesafe.ai", Model: "jev-latest"}
				c.Forge.DedupBackend = "jev"
			},
			wantErr: "dedup_skip_above",
		},
		{
			name: "an unorderable band is rejected",
			mutate: func(c *Config) {
				c.DecisionModel = DecisionModel{Enabled: true, Provider: "typesafe", BaseURL: "https://api.typesafe.ai", Model: "jev-latest"}
				c.Forge.DedupBackend = "jev"
				c.Forge.DedupSkipAbove = 0.5
				c.Forge.DedupAnnotateAbove = 0.8
			},
			wantErr: "must be greater than",
		},
		{
			name: "an unknown backend is rejected",
			mutate: func(c *Config) {
				c.Catalog.InstantRecall.Enabled = true
				c.Catalog.InstantRecall.RerankBackend = "magic"
			},
			wantErr: "rerank_backend",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := minimalValidConfig(t)
			tc.mutate(c)
			c.ApplyDefaults()
			err := c.Validate()
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("want an error mentioning %q, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestDisabledDecisionModelFallsBackWithoutErroring pins the kill switch. An operator
// turns enabled off DURING an incident; a startup error would make the switch useless
// exactly when it is needed, so a consumer still pointing at jev must degrade.
func TestDisabledDecisionModelFallsBackWithoutErroring(t *testing.T) {
	c := minimalValidConfig(t)
	c.DecisionModel = DecisionModel{Enabled: false, Provider: "typesafe", BaseURL: "https://api.typesafe.ai", Model: "jev-latest"}
	c.Catalog.InstantRecall.Enabled = true
	c.Catalog.InstantRecall.RerankBackend = "jev"
	c.Forge.DedupBackend = "jev"
	c.ApplyDefaults()
	if err := c.Validate(); err != nil {
		t.Fatalf("a disabled decision model must not fail validation: %v", err)
	}
	if c.DecisionModelUsable() {
		t.Fatal("a disabled block must not be usable")
	}
}

func TestRerankBackendDefaultsToTheLLM(t *testing.T) {
	c := minimalValidConfig(t)
	c.Catalog.InstantRecall.Enabled = true
	c.ApplyDefaults()
	if got := c.Catalog.InstantRecall.RerankBackend; got != "llm" {
		t.Fatalf("want the llm backend by default, got %q", got)
	}
	if c.Forge.DedupBackend != "bm25" {
		t.Fatalf("want bm25 dedup by default, got %q", c.Forge.DedupBackend)
	}
}
```

If `minimalValidConfig` does not exist in the test file, find the helper the existing config tests use to build a valid `*Config` (grep `func minimal` in `internal/config/`) and use that name instead; do not invent a second helper.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/config/ -run 'TestDecisionModel|TestRerankBackend|TestDisabledDecisionModel'`
Expected: FAIL to build — `undefined: DecisionModel`, `RerankBackend`, `DedupBackend`.

- [ ] **Step 3: Add the config types**

In `internal/config/config.go`, add the struct near the other top-level blocks and a field on `Config`:

```go
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
	Enabled  bool   `yaml:"enabled"`
	Provider string `yaml:"provider"`  // "typesafe" is the only implementation
	BaseURL  string `yaml:"base_url"`  // required when enabled
	Model    string `yaml:"model"`     // e.g. jev-latest
	APIKeyEnv string `yaml:"api_key_env"` // env var NAME; empty = keyless behind a gateway
}

// DecisionModelUsable reports whether a decider can actually be built: the block is
// enabled and carries an endpoint. Every consumer checks this rather than Enabled, so
// "configured but switched off" and "not configured" behave identically.
func (c *Config) DecisionModelUsable() bool {
	d := c.DecisionModel
	return d.Enabled && d.BaseURL != "" && d.Model != ""
}
```

Add `DecisionModel DecisionModel \`yaml:"decision_model"\`` to `Config`, `RerankBackend string \`yaml:"rerank_backend"\`` and `RerankThresholdJev float64 \`yaml:"rerank_threshold_jev"\`` to `InstantRecall`, and to `Forge`:

```go
	// DedupBackend selects the file-time dedup gate: "bm25" (default, the DupScore
	// threshold below) or "jev" (a decision model answers "same incident pattern?").
	DedupBackend string `yaml:"dedup_backend"`
	// DedupSkipAbove / DedupAnnotateAbove are the jev backend's band edges. Above skip:
	// do not file, record a confirmation. Between: file, naming the suspect in the body.
	// Below annotate: file as today. Both are REQUIRED when DedupBackend is jev — a
	// default nobody measured would be invented rather than chosen.
	DedupSkipAbove     float64 `yaml:"dedup_skip_above"`
	DedupAnnotateAbove float64 `yaml:"dedup_annotate_above"`
```

- [ ] **Step 4: Add defaults and validation**

In `ApplyDefaults`:

```go
	if c.Catalog.InstantRecall.RerankBackend == "" {
		c.Catalog.InstantRecall.RerankBackend = "llm"
	}
	if c.Forge.DedupBackend == "" {
		c.Forge.DedupBackend = "bm25"
	}
```

In `Validate`, beside the existing instant-recall block:

```go
	// A decision model that is enabled must be reachable; an unknown backend name is a
	// typo the operator needs told about. But a DISABLED block never errors, however a
	// consumer is set — see DecisionModel's doc for why the kill switch outranks
	// consistency here.
	if c.DecisionModel.Enabled {
		if c.DecisionModel.BaseURL == "" {
			return fmt.Errorf("decision_model.base_url is required when decision_model.enabled is true")
		}
		if c.DecisionModel.Model == "" {
			return fmt.Errorf("decision_model.model is required when decision_model.enabled is true")
		}
		if p := c.DecisionModel.Provider; p != "" && p != "typesafe" {
			return fmt.Errorf("decision_model.provider %q is not supported (valid: typesafe)", p)
		}
	}
	switch b := c.Catalog.InstantRecall.RerankBackend; b {
	case "llm", "shadow":
	case "jev":
		// Only the live backend needs a bar. Shadow records the confidence distribution
		// instead, which is how the value gets chosen in the first place.
		if c.DecisionModelUsable() {
			if t := c.Catalog.InstantRecall.RerankThresholdJev; t <= 0 || t > 1 {
				return fmt.Errorf("catalog.instant_recall.rerank_threshold_jev must be in (0,1] and is required when rerank_backend is jev, got %g", t)
			}
		}
	default:
		return fmt.Errorf("catalog.instant_recall.rerank_backend %q is not valid (llm|jev|shadow)", b)
	}
	switch b := c.Forge.DedupBackend; b {
	case "bm25":
	case "jev":
		if c.DecisionModelUsable() {
			if c.Forge.DedupSkipAbove <= 0 || c.Forge.DedupSkipAbove > 1 {
				return fmt.Errorf("forge.dedup_skip_above must be in (0,1] and is required when dedup_backend is jev, got %g", c.Forge.DedupSkipAbove)
			}
			if c.Forge.DedupAnnotateAbove <= 0 || c.Forge.DedupAnnotateAbove > 1 {
				return fmt.Errorf("forge.dedup_annotate_above must be in (0,1] and is required when dedup_backend is jev, got %g", c.Forge.DedupAnnotateAbove)
			}
			if c.Forge.DedupSkipAbove <= c.Forge.DedupAnnotateAbove {
				return fmt.Errorf("forge.dedup_skip_above (%g) must be greater than forge.dedup_annotate_above (%g), or the tiers are unorderable",
					c.Forge.DedupSkipAbove, c.Forge.DedupAnnotateAbove)
			}
		}
	default:
		return fmt.Errorf("forge.dedup_backend %q is not valid (bm25|jev)", b)
	}
```

- [ ] **Step 5: Run the tests**

Run: `go test ./internal/config/ -run 'TestDecisionModel|TestRerankBackend|TestDisabledDecisionModel' -v`
Expected: all PASS.

- [ ] **Step 6: Document the keys in the chart**

In `deploy/helm/runlore/values.yaml`, add a commented-out template beside the `model:` block:

```yaml
  # A System One decision model — NOT an LLM. It answers one bounded question and
  # returns a calibrated probability, and it carries none of model:'s knobs because
  # none of them mean anything to a model that emits no tokens.
  #
  # Enabling this sends alert titles, labels and runbook excerpts to a HOSTED third
  # party with no published retention window; zero data retention exists but must be
  # arranged with the vendor. There is no self-hosted option. Off by default.
  # decision_model:
  #   enabled: true
  #   provider: typesafe
  #   base_url: https://api.typesafe.ai
  #   model: jev-latest
  #   api_key_env: TYPESAFE_API_KEY   # the env var NAME, never the key
```

- [ ] **Step 7: Run the quality gate and commit**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
git add internal/config/ deploy/helm/runlore/values.yaml
git commit -m "feat(config): add the decision_model block and per-consumer backends

Its own top-level block, not a slot under model:, because an operator reading
model: expects an LLM endpoint where max_tokens, effort and thinking mean
something — and for a model that emits no tokens they do not.

enabled is a kill switch rather than a presence check: a consumer still
pointing at jev while the block is off falls back with a warning instead of
failing validation, because the switch exists for use during an incident and a
startup error would make it useless then. Errors are reserved for a config
that cannot be acted on at all.

Credentials are api_key_env, the variable NAME: configmap.yaml renders the
whole config block verbatim, so a literal key would sit in a ConfigMap."
```

---

## Task 3: Telemetry — two instruments

**Files:**
- Modify: `internal/telemetry/metrics.go` (two fields + their constructors)
- Modify: `internal/telemetry/setup.go` (register probability buckets)
- Test: `internal/telemetry/metrics_extra_test.go`

**Interfaces:**
- Produces: `Metrics.DecisionConfidence` (`metric.Float64Histogram`) and `Metrics.DecisionShadow` (`metric.Int64Counter`).

- [ ] **Step 1: Write the failing test**

Append to `internal/telemetry/metrics_extra_test.go`, following the existing scrape assertions in that file:

```go
func TestDecisionModelInstrumentsAreExported(t *testing.T) {
	body := scrapeAll(t, func(ctx context.Context, m *Metrics) {
		m.DecisionConfidence.Record(ctx, 0.84, metric.WithAttributes(attribute.String("consumer", "rerank")))
		m.DecisionShadow.Add(ctx, 1, metric.WithAttributes(
			attribute.String("consumer", "rerank"), attribute.String("agreement", "agree")))
	})
	for _, want := range []string{
		"runlore_decision_model_confidence_bucket",
		"runlore_decision_model_shadow_total",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metric %q missing from the scrape", want)
		}
	}
	// A probability belongs in probability buckets: the BM25 score buckets start at 0.1
	// and run to 10, which would put almost every confidence in one bucket.
	if !strings.Contains(body, `runlore_decision_model_confidence_bucket{consumer="rerank",le="0.9"}`) {
		t.Errorf("confidence is not on probability buckets:\n%s", body)
	}
}
```

`scrapeAll` is the helper the neighbouring tests use; if its name differs, grep the file and use the existing one rather than adding another.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/telemetry/ -run TestDecisionModelInstrumentsAreExported`
Expected: FAIL — `m.DecisionConfidence undefined`.

- [ ] **Step 3: Add the instruments**

In `internal/telemetry/metrics.go`, add to the `Metrics` struct and its constructor:

```go
	// DecisionConfidence is the calibrated confidence a decision model returned, by
	// consumer. It is the distribution a threshold is read OFF: shadow mode exists to
	// fill this histogram so the live bar is chosen from data rather than invented.
	DecisionConfidence metric.Float64Histogram
	// DecisionShadow counts shadow-mode comparisons (labels: consumer, agreement =
	// agree|disagree|error), which is what says whether a backend is ready to go live.
	DecisionShadow metric.Int64Counter
```

```go
		DecisionConfidence: histF("decision_model_confidence", "calibrated confidence returned by the decision model (label: consumer)"),
		DecisionShadow:     ctr("decision_model_shadow_total", "shadow-mode comparisons against the live backend (labels: consumer, agreement)"),
```

In `internal/telemetry/setup.go`, add probability buckets and register the instrument:

```go
// probabilityBuckets fit a calibrated 0–1 confidence. The BM25 scoreBuckets above start
// at 0.1 and run to 10, so a probability put through them collapses into the low buckets
// and the distribution a threshold is read off would be unreadable.
var probabilityBuckets = []float64{0.05, 0.1, 0.2, 0.3, 0.4, 0.5, 0.6, 0.7, 0.8, 0.9, 0.95, 1}

// probabilityHistograms are the instrument names that get probabilityBuckets.
var probabilityHistograms = []string{
	"runlore_decision_model_confidence",
}
```

and extend the view list where `bucketViews(scoreHistograms, scoreBuckets)` is used to also append `bucketViews(probabilityHistograms, probabilityBuckets)...`.

- [ ] **Step 4: Run the test**

Run: `go test ./internal/telemetry/ -v -run 'TestDecisionModelInstruments|TestBucket'`
Expected: PASS, and the existing bucket tests still pass.

- [ ] **Step 5: Quality gate and commit**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
git add internal/telemetry/
git commit -m "feat(telemetry): instrument decision-model confidence and shadow agreement

The confidence histogram is the distribution a live threshold is read off, so
shadow mode has somewhere to write; the counter is what says whether a backend
is ready to go live at all.

Confidence gets its own probability buckets rather than reusing the BM25 score
buckets, which start at 0.1 and run to 10 — a calibrated 0-1 value put through
those collapses into the low buckets and the distribution becomes unreadable."
```

---

## Task 4: The reranker's three backends

**Files:**
- Modify: `internal/investigate/rerank.go`
- Test: `internal/investigate/rerank_test.go`

**Interfaces:**
- Consumes: `providers.Decider`, `providers.Question`, `providers.KindChoice`, `providers.Answers` (Task 1); `Metrics.DecisionConfidence`, `Metrics.DecisionShadow` (Task 3).
- Produces: `Reranker.Decider providers.Decider`, `Reranker.Backend string`, `Reranker.ThresholdJev float64`; unexported `rerankQuestionID` / `rerankNoneOption` consts, `(rr *Reranker) decideRerank(ctx, req, cands) (providers.Answer, error)`, and `(rr *Reranker) rankJev(ctx, req, cands, spend) (catalog.Entry, float64, bool)`.

- [ ] **Step 1: Write the failing tests with a fake Decider**

Append to `internal/investigate/rerank_test.go`:

```go
// fakeDecider returns canned answers and records the state it was handed.
type fakeDecider struct {
	answers providers.Answers
	err     error
	state   string
	calls   int
}

func (f *fakeDecider) Decide(_ context.Context, state string, _ []providers.Question) (providers.Answers, error) {
	f.calls++
	f.state = state
	return f.answers, f.err
}

func TestRerankJevFiresOnItsOwnThreshold(t *testing.T) {
	cands := []catalog.ScoredEntry{
		{Entry: catalog.Entry{Path: "a.md", Title: "A"}, Score: 1},
		{Entry: catalog.Entry{Path: "b.md", Title: "B"}, Score: 0.5},
	}
	for _, tc := range []struct {
		name     string
		answers  providers.Answers
		err      error
		wantFire bool
		wantPath string
	}{
		{
			name:     "a named candidate above the bar fires",
			answers:  providers.Answers{rerankQuestionID: {Choice: "b.md", Confidence: 0.9}},
			wantFire: true, wantPath: "b.md",
		},
		{
			name:    "below the bar falls through",
			answers: providers.Answers{rerankQuestionID: {Choice: "b.md", Confidence: 0.4}},
		},
		{
			name:    "the none option falls through",
			answers: providers.Answers{rerankQuestionID: {Choice: rerankNoneOption, Confidence: 0.99}},
		},
		{
			name:    "an id outside the candidate set can never fire",
			answers: providers.Answers{rerankQuestionID: {Choice: "invented.md", Confidence: 0.99}},
		},
		{
			name:    "a missing answer falls through",
			answers: providers.Answers{},
		},
		{
			name: "an error falls through",
			err:  errors.New("upstream down"),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rr := &Reranker{
				Decider: &fakeDecider{answers: tc.answers, err: tc.err},
				Backend: "jev", ThresholdJev: 0.7, K: 5,
			}
			got, conf, ok := rr.rankJev(context.Background(), Request{Title: "t"}, cands, &recallSpend{})
			if ok != tc.wantFire {
				t.Fatalf("want fire=%v, got %v (conf %v)", tc.wantFire, ok, conf)
			}
			if tc.wantFire && got.Path != tc.wantPath {
				t.Fatalf("want %s, got %s", tc.wantPath, got.Path)
			}
		})
	}
}

// TestRerankShadowLetsTheLLMDecide pins the property that makes shadow safe to enable:
// the investigation's outcome must not depend on the shadow arm, including when it fails.
func TestRerankShadowLetsTheLLMDecide(t *testing.T) {
	cands := []catalog.ScoredEntry{{Entry: catalog.Entry{Path: "a.md", Title: "A"}, Score: 1}}
	fake := &fakeDecider{err: errors.New("shadow is down")}
	rr := &Reranker{
		Model:     &stubRerankModel{match: true, entryID: "a.md", confidence: 0.95},
		Decider:   fake,
		Backend:   "shadow",
		Threshold: 0.7, ThresholdJev: 0.7, K: 5,
	}
	got, conf, ok := rr.rank(context.Background(), Request{Title: "t"}, cands, &recallSpend{})
	if !ok || got.Path != "a.md" || conf != 0.95 {
		t.Fatalf("the LLM verdict must stand in shadow mode: ok=%v path=%s conf=%v", ok, got.Path, conf)
	}
	if fake.calls != 1 {
		t.Fatalf("the shadow arm must still be called once, got %d", fake.calls)
	}
}
```

`stubRerankModel` is the fake `providers.ModelProvider` the existing reranker tests use; grep `rerank_test.go` for its real name and fields and use those. If no such stub exists, the existing tests build one inline — reuse that shape rather than adding a second.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/investigate/ -run TestRerankJev`
Expected: FAIL to build — `rr.rankJev undefined`, `rerankQuestionID undefined`.

- [ ] **Step 3: Implement the Jev path and the backend switch**

In `internal/investigate/rerank.go`, add the fields to `Reranker`:

```go
	// Decider is the optional decision-model backend. Backend selects which one
	// decides: "llm" (default, today's forced-tool call), "jev" (the decider), or
	// "shadow" (both run, the LLM decides, agreement is recorded). Nil Decider with a
	// non-llm Backend falls back to the LLM — the kill switch's fallback path.
	Decider providers.Decider
	Backend string
	// ThresholdJev is the decider's own fire bar. It is NOT Threshold: the two
	// confidences are computed differently — the LLM asserts one, the decider derives
	// one from probability spread — so a shared number would mean different things.
	ThresholdJev float64
```

Add the question shape:

```go
// rerankQuestionID names the single question the decider is asked; the answer comes
// back keyed by it.
const rerankQuestionID = "runbook"

// rerankNoneOption is the explicit "no candidate matches" option. A choice primitive
// always returns one of the options it was given, so the fall-through case has to BE an
// option — without it the decider would be forced to name a runbook it does not believe.
const rerankNoneOption = "none"

// decideRerank makes the raw call and returns the decider's answer. Split out from
// rankJev so the shadow arm can tell an ERROR from a no-match: rankJev collapses both
// into ok=false, which is right for a fire decision and useless for measuring agreement.
func (rr *Reranker) decideRerank(ctx context.Context, req Request, cands []catalog.ScoredEntry) (providers.Answer, error) {
	choices := make(map[string]string, len(cands)+1)
	for _, c := range cands {
		desc := c.Entry.Title
		if c.Entry.Description != "" {
			desc += " — " + c.Entry.Description
		}
		choices[c.Entry.Path] = desc
	}
	choices[rerankNoneOption] = "none of these covers this resource failing this way"

	start := time.Now()
	ans, err := rr.Decider.Decide(ctx, renderRerankCandidates(req, cands), []providers.Question{{
		ID:           rerankQuestionID,
		Kind:         providers.KindChoice,
		Instructions: rerankQuestionInstructions,
		Choices:      choices,
	}})
	if rr.Metrics != nil {
		res := "ok"
		if err != nil {
			res = "error"
		}
		rr.Metrics.ModelRequests.Add(ctx, 1, metric.WithAttributes(
			attribute.String("provider", "decide"), attribute.String("result", res)))
		rr.Metrics.ModelRequestDuration.Record(ctx, time.Since(start).Seconds(),
			metric.WithAttributes(attribute.String("provider", "decide")))
	}
	if err != nil {
		return providers.Answer{}, err
	}
	a, ok := ans[rerankQuestionID]
	if !ok {
		return providers.Answer{}, fmt.Errorf("no answer for question %q", rerankQuestionID)
	}
	if rr.Metrics != nil {
		rr.Metrics.DecisionConfidence.Record(ctx, a.Confidence,
			metric.WithAttributes(attribute.String("consumer", "rerank")))
	}
	return a, nil
}

// rankJev asks the decision model which candidate, if any, is the right runbook. It
// fails SAFE exactly as rankLLM does: an error, a missing answer, the none option, a
// confidence below ThresholdJev, or an id outside the candidate set all return
// ok=false, and the caller falls through to a full investigation.
func (rr *Reranker) rankJev(ctx context.Context, req Request, cands []catalog.ScoredEntry, _ *recallSpend) (catalog.Entry, float64, bool) {
	a, err := rr.decideRerank(ctx, req, cands)
	if err != nil {
		if rr.Log != nil {
			rr.Log.Warn("decision-model reranker failed; falling through to full investigation", "title", req.Title, "err", err)
		}
		return catalog.Entry{}, 0, false
	}
	if rr.Log != nil {
		rr.Log.Info("decision-model rerank decision",
			"title", req.Title, "choice", a.Choice, "confidence", a.Confidence)
	}
	if a.Choice == rerankNoneOption || a.Confidence < rr.ThresholdJev {
		return catalog.Entry{}, 0, false
	}
	// Only ever an id it was offered: a backend that names something else must never
	// fire a recall, the same false-recall guard the LLM path applies.
	for _, c := range cands {
		if c.Entry.Path == a.Choice {
			return c.Entry, clampF(a.Confidence, 0, 1), true
		}
	}
	if rr.Log != nil {
		rr.Log.Warn("decision-model reranker named an id outside the candidate set; ignoring (no recall)",
			"title", req.Title, "choice", a.Choice)
	}
	return catalog.Entry{}, 0, false
}

// rerankQuestionInstructions is the decider's instruction text. It carries the same
// rule as rerankPrompt — canonical runbook for this resource + symptom, and a wrong
// match is worse than no match — without the prose the LLM needs, because a decision
// model is not being asked to reason in text.
const rerankQuestionInstructions = "Which candidate runbook, if any, is the canonical one for THIS incident: " +
	"the resource it covers and the failure it describes match this incident's resource and symptom. " +
	"Pick the none option unless one candidate clearly matches — a wrong match is worse than no match. " +
	"Do not require the alert to prove the runbook's exact root cause: that is re-confirmed against live state afterwards."
```

Rename the existing `rank` body to `rankLLM` (a pure rename, no behaviour change) and make `rank` the dispatcher:

```go
// rank routes the decision to the configured backend. The LLM path is the default and
// is unchanged; a decider that is configured but absent (the kill switch) falls back to
// it rather than failing, so an investigation's outcome never depends on the decider
// being reachable.
func (rr *Reranker) rank(ctx context.Context, req Request, cands []catalog.ScoredEntry, spend *recallSpend) (catalog.Entry, float64, bool) {
	if rr.Decider == nil || rr.Backend == "" || rr.Backend == "llm" {
		return rr.rankLLM(ctx, req, cands, spend)
	}
	if rr.Backend == "jev" {
		return rr.rankJev(ctx, req, cands, spend)
	}
	// shadow: the LLM decides, the decider is measured against it. The shadow arm must
	// never change the outcome, including when it errors, so its answer is recorded and
	// discarded. decideRerank rather than rankJev, because agreement needs to distinguish
	// a genuine disagreement from an outage.
	entry, conf, ok := rr.rankLLM(ctx, req, cands, spend)
	a, err := rr.decideRerank(ctx, req, cands)
	shadowFired := err == nil && a.Choice != rerankNoneOption && a.Confidence >= rr.ThresholdJev
	shadowPath := ""
	if shadowFired {
		shadowPath = a.Choice
	}
	if rr.Metrics != nil {
		agreement := "disagree"
		switch {
		case err != nil:
			agreement = "error"
		case ok == shadowFired && (!ok || entry.Path == shadowPath):
			agreement = "agree"
		}
		rr.Metrics.DecisionShadow.Add(ctx, 1, metric.WithAttributes(
			attribute.String("consumer", "rerank"), attribute.String("agreement", agreement)))
	}
	if rr.Log != nil {
		rr.Log.Info("rerank shadow comparison",
			"title", req.Title, "llm_fired", ok, "llm_entry", entry.Path,
			"shadow_fired", shadowFired, "shadow_entry", shadowPath, "shadow_err", err)
	}
	return entry, conf, ok
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/investigate/ -run 'TestRerank' -v`
Expected: all PASS, including the pre-existing reranker tests — the LLM path must be byte-for-byte unchanged in behaviour.

- [ ] **Step 5: Quality gate and commit**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
git add internal/investigate/
git commit -m "feat(investigate): give the recall reranker llm, jev and shadow backends

The decision model is asked one choice question over the candidate ids plus an
explicit none option — a choice primitive always returns one of the options it
was given, so the fall-through case has to BE an option or the model would be
forced to name a runbook it does not believe.

Its fire bar is a separate key from the LLM's. The two confidences are computed
differently, one asserted by a model and one derived from probability spread,
so a shared number would mean different things on either side.

Shadow mode runs both and keeps the LLM's verdict. The property that makes it
safe to enable is that the investigation's outcome cannot depend on the shadow
arm, including when it errors, and that is what the test pins."
```

---

## Task 5: Wire the decider into the reranker

**Files:**
- Modify: `internal/app/investigate.go:105-124` (the `RerankEnabled()` block)
- Test: `internal/app/model_test.go` or the nearest existing wiring test

**Interfaces:**
- Consumes: `config.DecisionModelUsable()`, `config.DecisionModel` (Task 2); `typesafe.New` (Task 1); `Reranker.Decider/Backend/ThresholdJev` (Task 4).
- Produces: `func BuildDecider(cfg *config.Config) providers.Decider` in `internal/app`.

- [ ] **Step 1: Write the failing test**

```go
func TestBuildDeciderIsNilUnlessUsable(t *testing.T) {
	for _, tc := range []struct {
		name string
		dm   config.DecisionModel
		want bool
	}{
		{"absent", config.DecisionModel{}, false},
		{"configured but disabled", config.DecisionModel{Provider: "typesafe", BaseURL: "https://x", Model: "jev-latest"}, false},
		{"enabled", config.DecisionModel{Enabled: true, Provider: "typesafe", BaseURL: "https://x", Model: "jev-latest"}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &config.Config{DecisionModel: tc.dm}
			got := BuildDecider(cfg)
			if (got != nil) != tc.want {
				t.Fatalf("want built=%v, got %v", tc.want, got != nil)
			}
		})
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/app/ -run TestBuildDecider`
Expected: FAIL — `undefined: BuildDecider`.

- [ ] **Step 3: Implement the builder and the wiring**

Add to `internal/app/model.go`:

```go
// BuildDecider builds the decision-model provider, or nil when the block is absent or
// switched off. Nil is the fallback signal every consumer reads: "configured but
// disabled" and "not configured" must behave identically, which is what makes
// decision_model.enabled a usable kill switch.
func BuildDecider(cfg *config.Config) providers.Decider {
	if !cfg.DecisionModelUsable() {
		return nil
	}
	return typesafe.New(cfg.DecisionModel.BaseURL, cfg.DecisionModel.Model,
		os.Getenv(cfg.DecisionModel.APIKeyEnv))
}
```

In `internal/app/investigate.go`, inside the existing `if cfg.Catalog.InstantRecall.RerankEnabled() {` block, after the `recall.Rerank = &investigate.Reranker{...}` literal:

```go
				// Decision-model backend, when one is configured and switched on. The
				// warning is the load-bearing part of the fallback: an operator who
				// flipped enabled off during an incident must see that the consumer
				// still pointing here degraded, rather than wonder why nothing changed.
				backend := cfg.Catalog.InstantRecall.RerankBackend
				if backend != "llm" {
					if dec := BuildDecider(cfg); dec != nil {
						recall.Rerank.Decider = dec
						recall.Rerank.Backend = backend
						recall.Rerank.ThresholdJev = cfg.Catalog.InstantRecall.RerankThresholdJev
					} else {
						log.Warn("rerank_backend is set but no decision model is usable; falling back to the llm backend",
							"rerank_backend", backend, "decision_model_enabled", cfg.DecisionModel.Enabled)
					}
				}
```

Add the `typesafe` import to `internal/app/model.go`.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/app/ -run 'TestBuildDecider' -v && go test ./internal/app/`
Expected: PASS, and no existing app test regresses.

- [ ] **Step 5: Quality gate and commit**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
git add internal/app/
git commit -m "feat(app): wire the decision model into the recall reranker

BuildDecider returns nil when the block is absent OR switched off, so those
two states behave identically downstream — which is what makes enabled a kill
switch rather than a second way to spell presence.

The fallback logs a warning rather than passing quietly. An operator who
flipped enabled off mid-incident needs to see that the consumer still pointing
at it degraded, instead of wondering why nothing changed."
```

---

## Task 6: Shadow agreement in the eval report

**Files:**
- Modify: `internal/eval/replay_report.go` (one field on `ReportCase`, mirrored on `CaseAggregate` in `internal/eval/eval.go`)
- Modify: `internal/eval/scorecard.go` (one column, rendered only when present)
- Test: `internal/eval/replay_report_test.go`

**Interfaces:**
- Consumes: nothing from earlier tasks at compile time — the eval reads counters the reranker already writes.
- Produces: `CaseAggregate.ShadowAgree int` and `.ShadowTotal int`, mirrored as `ReportCase.ShadowAgree` / `.ShadowTotal` with tags `shadow_agree,omitempty` / `shadow_total,omitempty`.

**Note on field order:** `Report()` converts `CaseAggregate` to `ReportCase` by struct conversion, so the two must stay identical in field order and type. Add both fields at the same position in both structs or the build fails — which is the intended guard.

- [ ] **Step 1: Write the failing test**

```go
func TestReportCarriesShadowAgreement(t *testing.T) {
	camp := Campaign{N: 5, Aggregates: []CaseAggregate{
		{Name: "c", Runs: 5, PassRate: 1, Reached: true, ShadowAgree: 4, ShadowTotal: 5},
	}}
	b, err := camp.Report("t", "m", providers.Usage{}, nil).JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	var got Report
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if got.Cases[0].ShadowAgree != 4 || got.Cases[0].ShadowTotal != 5 {
		t.Fatalf("shadow agreement not carried: %+v", got.Cases[0])
	}
	// A run with no shadow arm must not ship the keys at all.
	clean := Campaign{N: 1, Aggregates: []CaseAggregate{{Name: "c", Runs: 1, PassRate: 1}}}
	cb, err := clean.Report("t", "m", providers.Usage{}, nil).JSON()
	if err != nil {
		t.Fatalf("JSON: %v", err)
	}
	if strings.Contains(string(cb), "shadow_") {
		t.Fatalf("a non-shadow run must omit the shadow keys:\n%s", cb)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/eval/ -run TestReportCarriesShadowAgreement`
Expected: FAIL to build — `unknown field ShadowAgree`.

- [ ] **Step 3: Add the fields and the scorecard column**

Add to `CaseAggregate` (in `internal/eval/eval.go`), immediately after the recall telemetry fields:

```go
	// ShadowAgree / ShadowTotal count shadow-mode rerank comparisons over the repeats:
	// how often the decision model reached the same fire decision as the LLM. Zero when
	// no shadow arm ran, which is why the report omits them rather than printing 0/0.
	ShadowAgree int
	ShadowTotal int
```

and at the same position in `ReportCase` (in `internal/eval/replay_report.go`):

```go
	ShadowAgree int `json:"shadow_agree,omitempty"`
	ShadowTotal int `json:"shadow_total,omitempty"`
```

In `internal/eval/scorecard.go`, in the per-scenario table, add a `shadow` cell that renders `—` when `ShadowTotal == 0` and `4/5 (80%)` otherwise. Follow `recallCell`'s shape and place the new helper beside it:

```go
// shadowCell renders shadow-mode rerank agreement, or an em dash when no shadow arm
// ran. A published number here is the evidence for promoting a backend from shadow to
// live, so it belongs in the scorecard rather than only in the metrics.
func shadowCell(c ReportCase) string {
	if c.ShadowTotal == 0 {
		return "—"
	}
	return fmt.Sprintf("%d/%d (%.0f%%)", c.ShadowAgree, c.ShadowTotal,
		100*float64(c.ShadowAgree)/float64(c.ShadowTotal))
}
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/eval/ -v -run 'TestReportCarriesShadow|TestScorecard'`
Expected: PASS. Update any scorecard golden-output test that now has an extra column.

- [ ] **Step 5: Quality gate and commit**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
git add internal/eval/
git commit -m "feat(eval): publish shadow-mode rerank agreement per case

The number that decides whether a decision-model backend is ready to go live
belongs in the artifact a reader opens, not only in the metrics a deployment
may not scrape. Omitted entirely when no shadow arm ran, so a normal run's
report and scorecard are unchanged."
```

---

## Task 7: Curation dedup tiers

**Files:**
- Modify: `internal/curator/curator.go` (the dedup block at lines 92-135)
- Modify: `internal/app/curator.go` (wiring)
- Test: `internal/curator/curator_test.go`

**Interfaces:**
- Consumes: `providers.Decider`, `providers.KindNoul` (Task 1); `config.Forge.DedupBackend/DedupSkipAbove/DedupAnnotateAbove` (Task 2); `app.BuildDecider` (Task 5).
- Produces: `Curator.Decider providers.Decider`, `Curator.DedupSkipAbove float64`, `Curator.DedupAnnotateAbove float64`; unexported `dedupQuestionID` and `(c *Curator) sameIncidentPattern(ctx, inv, hit) (float64, bool)`.

- [ ] **Step 1: Write the failing test**

```go
// fakeDecider answers one noul question with a canned probability.
type fakeDecider struct {
	noul float64
	err  error
}

func (f fakeDecider) Decide(_ context.Context, _ string, qs []providers.Question) (providers.Answers, error) {
	if f.err != nil {
		return nil, f.err
	}
	return providers.Answers{qs[0].ID: {Noul: f.noul}}, nil
}

func TestDedupTiers(t *testing.T) {
	// One catalog hit whose BM25 score is BELOW the legacy dup_score default, so the
	// tiering can only come from the decider — the same regime the real 50-entry
	// catalog is in, and the reason the BM25 gate was inert there.
	hit := catalog.ScoredEntry{
		Entry: catalog.Entry{Path: "incidents/harbor-db-migration-lock.md", Title: "harbor-db migration lock"},
		Score: 0.49,
	}
	for _, tc := range []struct {
		name         string
		dec          providers.Decider
		wantFiled    bool
		wantNamesHit bool
	}{
		{"a confident duplicate is not filed", fakeDecider{noul: 0.95}, false, false},
		{"a middling match is filed with the suspect named", fakeDecider{noul: 0.70}, true, true},
		{"a weak match files as today", fakeDecider{noul: 0.20}, true, false},
		{"a decider outage falls back to the BM25 path", fakeDecider{err: errors.New("down")}, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeForge{}
			c := newCurator(f, multiScored{hits: []catalog.ScoredEntry{hit}})
			c.Decider = tc.dec
			c.DedupSkipAbove = 0.85
			c.DedupAnnotateAbove = 0.60

			if _, err := c.Curate(context.Background(), goodFinding()); err != nil {
				t.Fatalf("Curate: %v", err)
			}
			if filed := f.openedPR != nil; filed != tc.wantFiled {
				t.Fatalf("want filed=%v, got %v", tc.wantFiled, filed)
			}
			if !tc.wantFiled {
				return
			}
			names := strings.Contains(f.openedPR.Body, hit.Entry.Path)
			if names != tc.wantNamesHit {
				t.Fatalf("want the PR body to name the suspect=%v, got %v\nbody:\n%s",
					tc.wantNamesHit, names, f.openedPR.Body)
			}
		})
	}
}
```

`fakeForge`, `multiScored`, `newCurator` and `goodFinding` are the fakes and helpers the existing curator tests already use; do not add parallel ones. Confirm `fakeForge.openedPR`'s field for the PR body is named `Body` (it holds a `providers.KBEntry`) and adjust the assertion to the real field name if it differs.

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/curator/ -run TestDedupTiers`
Expected: FAIL — `unknown field Decider`.

- [ ] **Step 3: Implement the tiers**

Add to the `Curator` struct:

```go
	// Decider optionally replaces the BM25 dup_score gate with a calibrated
	// same-pattern judgement. Nil ⇒ the BM25 path, unchanged. The band edges are
	// required when it is set (config validates that), because a default nobody
	// measured would be invented rather than chosen.
	Decider            providers.Decider
	DedupSkipAbove     float64
	DedupAnnotateAbove float64
```

Add the question and its helper:

```go
// dedupQuestionID names the single question the decider is asked.
const dedupQuestionID = "same_pattern"

// sameIncidentPattern asks whether a finding and a catalog hit describe the same
// incident pattern. Returns the probability and whether it could be obtained at all;
// false means the caller uses the BM25 path, so a decider outage degrades to today's
// behaviour rather than blocking curation.
func (c *Curator) sameIncidentPattern(ctx context.Context, inv providers.Investigation, hit catalog.ScoredEntry) (float64, bool) {
	state := "PROPOSED FINDING\n" + Fingerprint(inv) +
		"\n\nEXISTING CATALOG ENTRY\n" + hit.Entry.Title + "\n" + hit.Entry.Description
	ans, err := c.Decider.Decide(ctx, state, []providers.Question{{
		ID:   dedupQuestionID,
		Kind: providers.KindNoul,
		Instructions: "The proposed finding and the existing catalog entry describe the SAME incident pattern: " +
			"the same resource failing the same way for the same reason. A different fault on the same workload is NOT the same pattern.",
	}})
	if err != nil {
		c.Log.Warn("dedup decider failed; falling back to the BM25 gate", "err", err)
		return 0, false
	}
	a, ok := ans[dedupQuestionID]
	if !ok {
		c.Log.Warn("dedup decider returned no answer; falling back to the BM25 gate")
		return 0, false
	}
	if c.Metrics != nil {
		c.Metrics.DecisionConfidence.Record(ctx, a.Noul,
			metric.WithAttributes(attribute.String("consumer", "dedup")))
	}
	return a.Noul, true
}
```

In `Curate`, replace the BM25 threshold comparison with the tiered decision when a decider is wired. Keep the exact-fingerprint check ahead of it, untouched: it needs no threshold and must stay first. When the score lands in the annotate band, pass the hit's path into the PR body the same way `RelatedEntry` context already reaches it, so the reviewer sees the suspect.

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/curator/ -v`
Expected: PASS, including the existing dedup tests on the BM25 path.

- [ ] **Step 5: Wire it and run the gate**

In `internal/app/curator.go`, set `Decider`, `DedupSkipAbove` and `DedupAnnotateAbove` from config when `cfg.Forge.DedupBackend == "jev"` and `BuildDecider` returns non-nil, warning and leaving them unset otherwise — the same fallback shape as Task 5.

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && hack/lint.sh
git add internal/curator/ internal/app/
git commit -m "feat(curator): tier the file-time dedup gate on a calibrated judgement

The BM25 dup_score gate is inert at its default: on a 50-entry catalog measured
2026-08-14 the scores run below 1.0 against a threshold of 5.0, and RunLore
re-filed an entry for a fault the catalog already held. A calibrated
same-pattern probability needs no per-corpus tuning.

Three tiers rather than one bar, because the middle case is real: a confident
duplicate is skipped and recorded as a confirmation, a middling one is filed
with the suspect named in the body so the reviewer decides, and a weak one
files as today. The exact-fingerprint check stays first and untouched — it
needs no threshold.

A decider outage falls back to the BM25 path: curation must not block on a
third party being reachable."
```

---

## Self-Review

**Spec coverage.** Contract → Task 1. Client → Task 1. Reranker three backends and the none option → Task 4. Per-backend thresholds → Tasks 2 and 4. Shadow agreement recorded → Tasks 3, 4 and 6. Dedup three tiers → Task 7. `decision_model:` block with `enabled` and `api_key_env` → Task 2. Dedup knobs under `forge:` → Task 2. Error handling table → Tasks 1, 4, 5, 7. Testing section → every task. Egress disclosure → Task 2's values.yaml comment.

**Two spec items are deliberately not tasks.** The semantic knowledge-base advisory was specced as a stretch item with a mechanical include-or-defer test, and it fails that test until the `forge` wiring in Task 7 exists, so it is a follow-up rather than a task here. The published-docs page for the egress and retention disclosure is a `website/` change that belongs with whatever release ships this, not with the code.

**One open question the implementer must resolve, not guess.** `decidePath` in Task 1: the docs page and the SDK reference disagree on the route's spelling. It is a named constant with both candidates in its comment and a loud failure mode, which is the honest handling — but the first live call must confirm it.

**Type consistency.** `rerankQuestionID`, `rerankNoneOption`, `decideRerank` and `rankJev` are defined in Task 4 and used only there; `rankLLM` is the pure rename of today's `rank` body, also in Task 4. `dedupQuestionID` is defined and used in Task 7. `providers.KindChoice` / `KindNoul` / `Answer.Choice` / `Answer.Noul` / `Answer.Confidence` are defined in Task 1 and used with those exact names in Tasks 4 and 7. `DecisionConfidence` and `DecisionShadow` are defined in Task 3 and used in Tasks 4 and 7. `BuildDecider` is defined in Task 5 and used in Tasks 5 and 7. `DecisionModelUsable` is defined in Task 2 and used in Task 5.

**Four defects this review caught and fixed in place.** A `wireQuestion` carrying a blank scaffolding field with a step telling the implementer to delete it. A fixture helper reading the request body with a single `Read` rather than `io.ReadAll`, which would flake on a chunked body. `BuildDecider` taking a logger it never used, which `revive` would reject. And a shadow-agreement switch whose `"error"` label was unreachable, because `rankJev` collapses an outage and a no-match into the same `false` — fixed by splitting `decideRerank` out so the shadow arm sees the error.

**Ordering constraint.** Task 6 adds a field to both `CaseAggregate` and `ReportCase`; they are converted by struct conversion, so both must change together in the same position or the build breaks. That is stated in the task.
