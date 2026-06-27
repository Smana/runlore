package source

import (
	"errors"
	"io"
	"net/http"
)

// Handler builds the HTTP handler for a webhook source: shared auth, body cap,
// decode, and ingest. auth (may be nil) authenticates the request; bodyCap is
// the max body size in bytes; pipe ingests the decoded result.
func (b Built) Handler(auth func(*http.Request) bool, bodyCap int64, pipe *Pipeline) http.HandlerFunc {
	wh := b.Impl.(WebhookSource)
	return func(w http.ResponseWriter, r *http.Request) {
		if auth != nil && !auth(r) {
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
		res, derr := wh.Decode(body, r.Header)
		if derr != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		pipe.Ingest(r.Context(), b.Desc.Admission, res)
		w.WriteHeader(http.StatusAccepted)
	}
}

// MountWebhooks registers every Webhook-kind source at its Path on mux.
func MountWebhooks(mux *http.ServeMux, built []Built, auth func(*http.Request) bool, pipe *Pipeline) {
	for _, b := range built {
		if b.Desc.Kind != Webhook {
			continue
		}
		mux.HandleFunc("POST "+b.Desc.Path, b.Handler(auth, 1<<20, pipe))
	}
}
