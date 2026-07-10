// SPDX-License-Identifier: Apache-2.0

package source

import (
	"errors"
	"io"
	"net/http"
)

// Authenticator is implemented by a webhook source that authenticates each
// delivery from the request body itself (e.g. an HMAC signature over the raw
// body) rather than via the shared bearer token. When a source implements it,
// the core calls Authenticate after reading the body and skips the shared auth:
// a source that signs its own requests (PagerDuty) cannot present the operator's
// bearer token, so it owns authentication end-to-end.
type Authenticator interface {
	Authenticate(body []byte, h http.Header) bool
}

// Handler builds the HTTP handler for a webhook source: shared auth, body cap,
// decode, and ingest. auth (may be nil) authenticates the request via the shared
// bearer token; a source implementing Authenticator authenticates itself from the
// body instead (shared auth is skipped for it). bodyCap is the max body size in
// bytes; pipe ingests the decoded result.
func (b Built) Handler(auth func(*http.Request) bool, bodyCap int64, pipe *Pipeline) http.HandlerFunc {
	wh := b.Impl.(WebhookSource)
	selfAuth, selfAuths := b.Impl.(Authenticator)
	return func(w http.ResponseWriter, r *http.Request) {
		if !selfAuths && auth != nil && !auth(r) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, bodyCap)
		body, err := io.ReadAll(r.Body)
		if err != nil {
			var mbe *http.MaxBytesError
			if errors.As(err, &mbe) {
				http.Error(w, "payload too large", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if selfAuths && !selfAuth.Authenticate(body, r.Header) {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		res, derr := wh.Decode(body, r.Header)
		if derr != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		pipe.Ingest(r.Context(), b.Desc.Admission, res)
		w.WriteHeader(http.StatusAccepted)
	}
}

// MountWebhooks registers every Webhook-kind source at its Path on mux. wrap
// (optional, nil = none) decorates each handler — the serve path passes the
// leader-forwarding middleware (#264) so a non-leader replica proxies webhook
// deliveries to the leader instead of ingesting into its own idle queue.
func MountWebhooks(mux *http.ServeMux, built []Built, auth func(*http.Request) bool, pipe *Pipeline, wrap func(http.Handler) http.Handler) {
	for _, b := range built {
		if b.Desc.Kind != Webhook {
			continue
		}
		var h http.Handler = b.Handler(auth, 1<<20, pipe)
		if wrap != nil {
			h = wrap(h)
		}
		mux.Handle("POST "+b.Desc.Path, h)
	}
}
