// SPDX-License-Identifier: Apache-2.0

// Package clientcore provides the plumbing shared by the hand-rolled model
// provider clients (anthropic, gemini, openai): common construction defaults,
// the streaming request pipeline (retry, status classification, idle-timeout
// guard), SSE event decoding, and error-detail sanitization. Everything
// provider-specific — wire types, SSE event-accumulation grammar, error-body
// JSON shapes, auth headers — stays in the provider packages.
package clientcore

import (
	"net/http"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/httpx"
)

const (
	// DefaultMaxTokens is the output-token ceiling used when the caller passes
	// <= 0. The anthropic client previously defaulted to 4096, diverging from
	// openai/gemini; the app layer always passes a resolved value, so that
	// divergence was dead in practice — aligned to 8192 here.
	DefaultMaxTokens = 8192
	// ResponseHeaderTimeout caps the wait for response headers
	// (time-to-first-byte); the streamed body then has no flat deadline (a
	// long completion is legitimate).
	ResponseHeaderTimeout = 2 * time.Minute
	// IdleTimeout aborts a stream that stalls (no bytes) for this long — the
	// streaming counterpart of an overall deadline, without killing an
	// actively-sending stream.
	IdleTimeout = 2 * time.Minute
)

// Base carries the configuration fields common to every provider client.
// Provider clients embed it and add their own knobs (e.g. reasoning effort).
type Base struct {
	BaseURL   string
	Model     string
	APIKey    string
	MaxTokens int
	HTTP      *http.Client
}

// NewBase normalizes the shared constructor inputs: an empty baseURL falls
// back to defaultBaseURL (which may itself be empty for providers without a
// public default), trailing slashes are trimmed, maxTokens <= 0 falls back to
// DefaultMaxTokens, and the HTTP client is the hardened streaming client.
func NewBase(baseURL, defaultBaseURL, model, apiKey string, maxTokens int) Base {
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if maxTokens <= 0 {
		maxTokens = DefaultMaxTokens
	}
	return Base{
		BaseURL:   strings.TrimRight(baseURL, "/"),
		Model:     model,
		APIKey:    apiKey,
		MaxTokens: maxTokens,
		HTTP:      httpx.SecureStreamingClient(ResponseHeaderTimeout),
	}
}
