// SPDX-License-Identifier: Apache-2.0

package typesafe

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/providers"
)

// decideAgainst asks one noul question of a fixture server answering status/body and
// returns Decide's error.
func decideAgainst(t *testing.T, status int, body string) error {
	t.Helper()
	srv, _, _ := fixtureServer(t, status, body)
	_, err := New(srv.URL, "jev-latest", "").Decide(context.Background(), "an incident", []providers.Question{{
		ID: "q", Kind: providers.KindNoul, Instructions: "same pattern?",
	}})
	return err
}

// TestDecideRefusesAnOversizedResponse: base_url is operator-configurable, so the body
// is untrusted in size as well as content. Past httpx.MaxResponseBytes the read is
// refused, and the error names the remedy rather than the capped reader's "narrow the
// query" hint, which has no query to point at here.
func TestDecideRefusesAnOversizedResponse(t *testing.T) {
	err := decideAgainst(t, http.StatusOK, strings.Repeat("x", httpx.MaxResponseBytes+1))
	if !errors.Is(err, httpx.ErrResponseTooLarge) {
		t.Fatalf("want an error wrapping httpx.ErrResponseTooLarge, got %v", err)
	}
	if !strings.Contains(err.Error(), "decision_model.base_url") || strings.Contains(err.Error(), "narrow the query") {
		t.Fatalf("the error must name the remedy and not the query hint, got %v", err)
	}
}

// TestDecideReportsTheStatusBeforeTheBodySize: a non-200 is reported by its status
// whatever its body holds. Reading the body first turned a misrouted base_url's 502
// error page into "response too large".
func TestDecideReportsTheStatusBeforeTheBodySize(t *testing.T) {
	err := decideAgainst(t, http.StatusBadGateway, strings.Repeat("<html>", httpx.MaxResponseBytes/6+1))
	if err == nil || !strings.Contains(err.Error(), "decide status 502") || errors.Is(err, httpx.ErrResponseTooLarge) {
		t.Fatalf("want the status reported and the body's size not to mask it, got %v", err)
	}
}

// TestDecideRequiresAnAnswerToEveryQuestionAsked: a 200 whose payload does not carry
// the question asked (vendor schema drift, a gateway rewriting ids) is an error from
// the client, so every consumer sees the same failure and none has to check for it.
func TestDecideRequiresAnAnswerToEveryQuestionAsked(t *testing.T) {
	err := decideAgainst(t, http.StatusOK, `{"model":"jev","answers":{"other":{"type":"noul","noul":0.9}}}`)
	if err == nil || !strings.Contains(err.Error(), `no answer for question "q"`) {
		t.Fatalf("want the missing question named, got %v", err)
	}
}
