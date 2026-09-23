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

// fixtureServer answers every POST with the given status and body, capturing the
// last request body received and the total number of requests it has answered — the
// latter is what lets a test tell "failed once" from "hit by a retry storm".
func fixtureServer(t *testing.T, status int, body string) (*httptest.Server, *string, *int) {
	t.Helper()
	var got string
	var count int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count++
		b, _ := io.ReadAll(r.Body)
		got = string(b)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &got, &count
}

func TestDecideChoiceCarriesConfidenceAndProbabilities(t *testing.T) {
	srv, sent, _ := fixtureServer(t, http.StatusOK, `{
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

func TestDecideNoulIsItsOwnProbability(t *testing.T) {
	srv, _, _ := fixtureServer(t, http.StatusOK,
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
			srv, _, count := fixtureServer(t, tc.status, tc.body)
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
			if tc.status == http.StatusTooManyRequests && *count != 1 {
				t.Fatalf("a rate limit must hit the server exactly once (no retry storm in front of a free fall-through), got %d", *count)
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
	srv, sent, _ := fixtureServer(t, http.StatusOK,
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
