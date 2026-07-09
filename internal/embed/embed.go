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

	"github.com/Smana/runlore/internal/httpx"
)

// Client calls an OpenAI-compatible /embeddings endpoint.
type Client struct {
	baseURL string
	model   string
	apiKey  string
	http    *http.Client
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
}

// Embed returns one vector per input text, in input order. Empty input → nil.
func (c *Client) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	body, err := json.Marshal(embedRequest{Model: c.model, Input: texts})
	if err != nil {
		return nil, fmt.Errorf("marshal request: %w", err)
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
	resp, err := httpx.DoWithRetry(ctx, c.http, 3, newReq)
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
