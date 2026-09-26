// SPDX-License-Identifier: Apache-2.0

package typesafe

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
)

// TestDecideRefusesAnOversizedResponse: base_url is operator-configurable, so the
// response body is untrusted in size as well as content. It was read whole with an
// unbounded io.ReadAll, so a proxy that streams an unbounded or multi-hundred-MB body
// was allocated in full on the recall path for a call that is supposed to be a few
// KB. The capped reader every other backend client uses refuses past
// httpx.MaxResponseBytes with an actionable sentence.
func TestDecideRefusesAnOversizedResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(strings.Repeat("x", httpx.MaxResponseBytes+1)))
	}))
	t.Cleanup(srv.Close)

	c := New(srv.URL, "jev-latest", "")
	_, err := c.Decide(context.Background(), "an incident", []providers.Question{{
		ID: "q", Kind: providers.KindNoul, Instructions: "same pattern?",
	}})
	if !errors.Is(err, httpx.ErrResponseTooLarge) {
		t.Fatalf("want an error wrapping httpx.ErrResponseTooLarge, got %v", err)
	}
}
