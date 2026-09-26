// SPDX-License-Identifier: Apache-2.0

// Package typesafe speaks TypeSafe's System One HTTP API, the first implementation of
// providers.Decider. Hand-rolled over net/http like internal/embed and the model
// clients: the vendor ships no Go SDK, and the single static binary is the point.
package typesafe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+decidePath, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	// One attempt, deliberately: a rejected or failed decide falls through to the full
	// investigation that was going to run anyway, so a retry (unlike internal/embed's,
	// which guards a call with no such fall-through) would only add backoff latency in
	// front of a gate that does not need it. A plain Do, because httpx.DoWithRetry at
	// one attempt is the same call behind a loop that never loops.
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("decide request: %w", err)
	}
	defer func() { httpx.Drain(resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		// base_url is operator-configurable, so never echo the upstream body: it is an
		// information-disclosure and log-injection surface. Status + request id only,
		// as the logs and metrics clients do. Decided BEFORE the body is read, so a
		// misrouted base_url's multi-MB error page reports its status, not its size.
		return nil, fmt.Errorf("decide status %d (request-id %q)", resp.StatusCode, httpx.RequestID(resp.Header))
	}
	// Capped, as the logs and metrics clients are: the body is untrusted in size as well
	// as content, and a decide response is a few KB of JSON on the recall path. The
	// cap's own message names a query to narrow; there is none here, so the error is
	// built from the sentinel with the remedy that applies.
	data, err := httpx.ReadBody(resp.Body)
	if err != nil {
		if errors.Is(err, httpx.ErrResponseTooLarge) {
			return nil, fmt.Errorf("read response: %w: over %d bytes — decision_model.base_url is answering with something that is not a decide response",
				httpx.ErrResponseTooLarge, httpx.MaxResponseBytes)
		}
		return nil, fmt.Errorf("read response: %w", err)
	}
	var wr wireResponse
	if err := json.Unmarshal(data, &wr); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	// Every question asked must be answered, so a 200 whose payload lacks one (vendor
	// schema drift, a gateway rewriting ids) fails here, for every consumer, rather
	// than being re-detected in each of them.
	for _, q := range qs {
		if _, ok := wr.Answers[q.ID]; !ok {
			return nil, fmt.Errorf("response carried no answer for question %q", q.ID)
		}
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
