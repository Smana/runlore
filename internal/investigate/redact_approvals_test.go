// This file lives in the external test package so it can wire the real
// investigate loop to the real action.Approvals queue AND the real HTTP server:
// internal/server transitively imports internal/investigate (via internal/source),
// so an in-package test importing server would be an import cycle.
package investigate_test

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/server"
)

// scriptedModel replays canned completion responses in order.
type scriptedModel struct {
	responses []providers.CompletionResponse
	i         int
}

func (m *scriptedModel) Complete(context.Context, providers.CompletionRequest) (providers.CompletionResponse, error) {
	r := m.responses[m.i]
	m.i++
	return r, nil
}

// TestApprovalQueueServesRedactedActions locks in the redaction ordering for the
// rung-2 (approve) path: deliver() runs redactInvestigation BEFORE OnComplete
// (internal/investigate/loop.go), and the app wiring registers actions with the
// approvals queue INSIDE OnComplete (internal/app/investigate.go) — so the copies
// the approvals queue holds, and that GET /actions serves, are the post-redaction
// ones. If registration ever moves ahead of the egress redaction (or the
// redaction moves after OnComplete), this test fails.
func TestApprovalQueueServesRedactedActions(t *testing.T) {
	const secret = "hunter2horse"
	// The model proposes an envelope-compliant action whose description carries a
	// secret, as quoted evidence from logs might.
	findingsArgs := `{"confidence":0.9,` +
		`"root_causes":[{"summary":"credential rotation broke the app","confidence":0.9,"evidence":["e"]}],` +
		`"actions":[{"description":"suspend web; logs showed password=` + secret + `",` +
		`"op":"suspend","reversible":true,"target":{"kind":"Kustomization","name":"web","namespace":"apps"}}]}`
	model := &scriptedModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "submit_findings", Args: findingsArgs}}},
	}}

	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	pol := action.New(config.ActionPolicy{
		Mode:  config.ActionApprove,
		Allow: config.ActionAllow{ReversibleOnly: true, Namespaces: []string{"apps"}},
	})
	approvals := action.NewApprovals(nil, pol, nil, discard)

	li := &investigate.LoopInvestigator{
		Model:    model,
		MaxSteps: 3,
		Actions:  pol,
		Log:      discard,
		OnComplete: func(found providers.Investigation) {
			// Mirrors the rung-2 wiring in internal/app/investigate.go: register each
			// envelope-compliant action for human approval.
			for i := range found.Actions {
				found.Actions[i].ApprovalID = approvals.Register(found.Actions[i])
			}
		},
	}
	if err := li.Investigate(context.Background(), investigate.Request{Title: "web down"}); err != nil {
		t.Fatalf("Investigate: %v", err)
	}

	// The queue itself must hold the redacted copy.
	pending := approvals.List()
	if len(pending) != 1 {
		t.Fatalf("pending actions = %d, want 1", len(pending))
	}
	desc := pending[0].Action.Description
	if strings.Contains(desc, secret) {
		t.Fatalf("approvals queue holds an unredacted secret: %q", desc)
	}
	// Sanity: the assertion above isn't passing because the description was
	// dropped or rewritten — the redaction marker replaced the secret in place.
	if !strings.Contains(desc, "[REDACTED]") {
		t.Fatalf("expected a redaction marker in the queued description, got %q", desc)
	}

	// And the HTTP surface: GET /actions serves the queue as JSON to a
	// token-holding caller — the body must not contain the secret either.
	srv := server.New(nil, server.Actions{Approvals: approvals, Token: "tok"}, nil, nil, nil, discard)
	req := httptest.NewRequest(http.MethodGet, "/actions", nil)
	req.Header.Set("X-Approval-Token", "tok")
	rr := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("GET /actions = %d, want 200", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, secret) {
		t.Fatalf("GET /actions served an unredacted secret: %q", body)
	}
	if !strings.Contains(body, "[REDACTED]") {
		t.Fatalf("expected the redacted action in the /actions response, got %q", body)
	}
}
