// SPDX-License-Identifier: Apache-2.0

// Package embed provides an OpenAI-compatible embeddings client and the
// similarity + fusion primitives for hybrid (BM25 + vector) catalog retrieval.
//
// It is the in-house alternative to a Python embedding sidecar or a vector-DB
// dependency: embeddings are served by the same OpenAI-compatible endpoint as the
// model (in-cluster vLLM, Ollama, OpenAI), and similarity is brute-force cosine
// over the (small) catalog — no new datastore, still a single static binary.
//
// This package is the foundation only; wiring hybrid retrieval into the catalog
// index and the recall gates is an eval-validated follow-up (the BM25-tuned recall
// thresholds must be re-measured for fused scores, not guessed).
package embed

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"sort"
	"strings"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/telemetry"
)

// Client calls an OpenAI-compatible /embeddings endpoint.
type Client struct {
	baseURL string
	model   string
	apiKey  string
	http    *http.Client

	// Metrics is optional (nil-safe) telemetry for the embeddings endpoint. It is a
	// PAID API — called once per hybrid-recall query, from inside an investigation
	// that has spend ceilings, and in bulk on every catalog reload — and until it was
	// wired here nothing counted its calls, its latency or its tokens. Recording
	// under provider="embed" on the shared model instruments keeps it separable by
	// label instead of inventing a metric name, matching the reranker's precedent.
	//
	// Metrics makes that spend VISIBLE for every caller; UsageSink is what makes it
	// ATTRIBUTABLE to one, and only an attributable figure can be bounded. A caller
	// that installs a sink on the context — internal/investigate does, for the duration
	// of one investigation — folds these tokens into the totals its spend ceilings
	// compare against, which is how the cost reaches its payer despite the Embedder
	// interface returning vectors only.
	//
	// The BULK corpus embed has no such payer: it runs on the git-sync goroutine with
	// no investigation to charge, so it stays visible but UNBOUNDED, deliberately — see
	// the spend inventory in docs/configuration/configuration.md for why a ceiling
	// there would be a knob with nothing to bound.
	Metrics *telemetry.Metrics
}

// New builds an embeddings client. apiKey may be empty (keyless vLLM/Ollama).
func New(baseURL, model, apiKey string) *Client {
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		model:   model,
		apiKey:  apiKey,
		http:    httpx.SecureClient(60 * time.Second),
	}
}

type embedRequest struct {
	Model string   `json:"model"`
	Input []string `json:"input"`
}

type embedResponse struct {
	Data []struct {
		Index     int       `json:"index"`
		Embedding []float32 `json:"embedding"`
	} `json:"data"`
	// Usage is what the endpoint says it billed. The OpenAI-compatible /embeddings
	// wire format returns it (vLLM and Ollama's shims included) and this struct used
	// to discard it, which is why embedding spend had no token figure anywhere.
	// Absent from a server that omits it ⇒ zero, and zero is simply not recorded.
	Usage struct {
		PromptTokens int `json:"prompt_tokens"`
		TotalTokens  int `json:"total_tokens"`
	} `json:"usage"`
}

// maxEmbedBatch bounds the inputs per /embeddings request. Providers cap the
// batch (OpenAI ~2048; many vLLM/Ollama deployments far less), and one oversized
// request would fail the WHOLE corpus embed — chunking keeps a growing KB
// embeddable and each request well under every common cap.
const maxEmbedBatch = 256

// Embed returns one vector per input text, in input order. Empty input → nil.
// Large inputs are transparently split into maxEmbedBatch-sized requests; any
// failing chunk fails the whole call (callers rely on all-or-nothing).
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	out := make([][]float32, 0, len(texts))
	for start := 0; start < len(texts); start += maxEmbedBatch {
		end := min(start+maxEmbedBatch, len(texts))
		vecs, err := c.embedBatch(ctx, texts[start:end])
		if err != nil {
			return nil, fmt.Errorf("inputs %d-%d: %w", start, end-1, err)
		}
		out = append(out, vecs...)
	}
	return out, nil
}

// embedBatch performs one bounded /embeddings request (the pre-chunking Embed body).
func (c *Client) embedBatch(ctx context.Context, texts []string) ([][]float32, error) {
	body, err := json.Marshal(embedRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
	}
	// Charge this round trip to whoever is accounting for it. Deferred, and placed
	// after the marshal (which sends nothing), so EVERY path that reaches the wire is
	// charged — including the ones that return an error: a request the endpoint
	// accepted is billed whether or not we could read its answer, the same rule the
	// investigation loop's own completions follow. promptTokens is filled in below
	// when the endpoint reports it and stays 0 otherwise, meaning unknown rather than
	// free.
	promptTokens := 0
	if sink := UsageSinkFrom(ctx); sink != nil {
		defer func() { sink.record(promptTokens) }()
	}
	newReq := func() (*http.Request, error) {
		r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/embeddings", bytes.NewReader(body))
		if err != nil {
			return nil, err
		}
		r.Header.Set("Content-Type", "application/json")
		if c.apiKey != "" {
			r.Header.Set("Authorization", "Bearer "+c.apiKey)
		}
		return r, nil
	}
	start := time.Now()
	resp, err := httpx.DoWithRetry(ctx, c.http, 3, newReq)
	c.recordRequest(ctx, start, err)
	if err != nil {
		return nil, fmt.Errorf("embeddings request: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		// base_url is operator-configurable; don't echo the upstream body (info
		// disclosure + log injection). Surface status + sanitized request-id only.
		return nil, fmt.Errorf("embeddings status %d (request-id %q)", resp.StatusCode, httpx.RequestID(resp.Header))
	}
	var er embedResponse
	if err := json.Unmarshal(data, &er); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	promptTokens = er.Usage.PromptTokens
	if c.Metrics != nil && er.Usage.PromptTokens > 0 {
		c.Metrics.ModelInputTokens.Add(ctx, int64(er.Usage.PromptTokens), embedAttrs)
	}
	if len(er.Data) != len(texts) {
		return nil, fmt.Errorf("embeddings: got %d vectors for %d inputs", len(er.Data), len(texts))
	}
	// The API may return data out of order; place each by its declared index.
	out := make([][]float32, len(texts))
	for _, d := range er.Data {
		if d.Index < 0 || d.Index >= len(out) {
			return nil, fmt.Errorf("embeddings: index %d out of range", d.Index)
		}
		out[d.Index] = d.Embedding
	}
	for i, v := range out {
		if len(v) == 0 {
			return nil, fmt.Errorf("embeddings: missing vector for input %d", i)
		}
	}
	return out, nil
}

// embedAttrs labels every embeddings sample with provider="embed", so the endpoint's
// call volume, latency and tokens sit beside the main/verify/rerank tiers on the same
// instruments and a dashboard can select or exclude it with one label matcher.
var embedAttrs = metric.WithAttributes(attribute.String("provider", "embed"))

// recordRequest emits one /embeddings round trip's call count and latency. Nil-safe:
// a Client with no Metrics (every CLI path) records nothing.
func (c *Client) recordRequest(ctx context.Context, start time.Time, err error) {
	if c.Metrics == nil {
		return
	}
	res := "ok"
	if err != nil {
		res = "error"
	}
	c.Metrics.ModelRequests.Add(ctx, 1, metric.WithAttributes(
		attribute.String("provider", "embed"), attribute.String("result", res)))
	c.Metrics.ModelRequestDuration.Record(ctx, time.Since(start).Seconds(), embedAttrs)
}

// Cosine returns the cosine similarity of two equal-length vectors, in [-1, 1].
// Mismatched-length or zero-norm vectors return 0 ("no signal").
func Cosine(a, b []float32) float64 {
	if len(a) != len(b) || len(a) == 0 {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// Fused is an id with its fused relevance score.
type Fused struct {
	ID    string
	Score float64
}

// FuseRRF combines several rankings of the same id space by Reciprocal Rank Fusion:
// score(id) = Σ 1/(k + rank), rank 0-based within each ranking; an id absent from a
// ranking contributes nothing. RRF is scale-free — it fuses BM25 and cosine rankings
// without their different score magnitudes distorting the result. k dampens low ranks
// (60 is the common default). Results are sorted by descending fused score, ties
// broken by id for determinism.
func FuseRRF(k float64, rankings ...[]string) []Fused {
	if k <= 0 {
		k = 60
	}
	score := map[string]float64{}
	for _, r := range rankings {
		for rank, id := range r {
			score[id] += 1.0 / (k + float64(rank))
		}
	}
	out := make([]Fused, 0, len(score))
	for id, s := range score {
		out = append(out, Fused{ID: id, Score: s})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].ID < out[j].ID
	})
	return out
}
