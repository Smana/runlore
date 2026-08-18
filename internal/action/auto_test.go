// SPDX-License-Identifier: Apache-2.0

package action

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// autoPolicy allows the "apps" namespace so re-validation at the exec boundary passes.
func autoPolicy() *Policy {
	return New(config.ActionPolicy{Mode: config.ActionAuto, Allow: config.ActionAllow{ReversibleOnly: true, Namespaces: []string{"apps"}}})
}

var revAction = providers.Action{Op: "suspend", Reversible: true, Target: providers.Workload{Kind: "Kustomization", Name: "web", Namespace: "apps"}}

func autoInv(conf float64, acts ...providers.Action) providers.Investigation {
	return providers.Investigation{Confidence: conf, Actions: acts}
}

func TestAutoExecutes(t *testing.T) {
	exec := &fakeExec{}
	a := NewAuto(exec, config.AutoPolicy{MinConfidence: 0.5}, autoPolicy(), nil, discardLog())
	a.Resume() // NewAuto starts paused (fail closed); opt in to execution
	out := a.Run(context.Background(), autoInv(0.9, revAction))
	if len(exec.ran) != 1 || exec.ran[0].Op != "suspend" {
		t.Fatalf("expected one execution, got %+v", exec.ran)
	}
	if !strings.Contains(out[0].Description, "auto-executed") {
		t.Fatalf("description not annotated: %q", out[0].Description)
	}
}

func TestAutoRefusesIrreversible(t *testing.T) {
	exec := &fakeExec{}
	a := NewAuto(exec, config.AutoPolicy{MinConfidence: 0.5}, autoPolicy(), nil, discardLog())
	a.Resume() // NewAuto starts paused (fail closed); opt in to execution
	out := a.Run(context.Background(), autoInv(0.9, providers.Action{Op: "delete", Reversible: false}))
	if len(exec.ran) != 0 {
		t.Fatal("irreversible action must never auto-execute")
	}
	if !strings.Contains(out[0].Description, "irreversible") {
		t.Fatalf("expected irreversible annotation: %q", out[0].Description)
	}
}

func TestAutoConfidenceGate(t *testing.T) {
	exec := &fakeExec{}
	a := NewAuto(exec, config.AutoPolicy{MinConfidence: 0.8}, autoPolicy(), nil, discardLog())
	a.Resume()
	out := a.Run(context.Background(), autoInv(0.5, revAction))
	if len(exec.ran) != 0 {
		t.Fatal("must not execute below the confidence threshold")
	}
	if !strings.Contains(out[0].Description, "confidence") {
		t.Fatalf("expected confidence annotation: %q", out[0].Description)
	}
}

func TestAutoDryRun(t *testing.T) {
	exec := &fakeExec{}
	a := NewAuto(exec, config.AutoPolicy{DryRun: true}, autoPolicy(), nil, discardLog())
	a.Resume()
	out := a.Run(context.Background(), autoInv(0.9, revAction))
	if len(exec.ran) != 0 {
		t.Fatal("dry-run must not execute")
	}
	if !strings.Contains(out[0].Description, "dry-run") {
		t.Fatalf("expected dry-run annotation: %q", out[0].Description)
	}
}

func TestAutoKillSwitch(t *testing.T) {
	exec := &fakeExec{}
	a := NewAuto(exec, config.AutoPolicy{}, autoPolicy(), nil, discardLog())
	a.Pause()
	if !a.Paused() {
		t.Fatal("Pause should engage the kill-switch")
	}
	out := a.Run(context.Background(), autoInv(1.0, revAction))
	if len(exec.ran) != 0 {
		t.Fatal("paused auto must not execute")
	}
	if !strings.Contains(out[0].Description, "paused") {
		t.Fatalf("expected paused annotation: %q", out[0].Description)
	}
	a.Resume()
	if a.Paused() {
		t.Fatal("Resume should clear the kill-switch")
	}
	a.Run(context.Background(), autoInv(1.0, revAction))
	if len(exec.ran) != 1 {
		t.Fatal("resumed auto should execute")
	}
}

func TestAutoRateLimit(t *testing.T) {
	exec := &fakeExec{}
	a := NewAuto(exec, config.AutoPolicy{MaxPerWindow: 1}, autoPolicy(), nil, discardLog())
	a.Resume()
	out := a.Run(context.Background(), autoInv(0.9, revAction, revAction)) // two actions, budget 1
	if len(exec.ran) != 1 {
		t.Fatalf("rate limit should allow exactly one, got %d", len(exec.ran))
	}
	if !strings.Contains(out[1].Description, "rate-limited") {
		t.Fatalf("expected second action rate-limited: %q", out[1].Description)
	}
}

func TestNewAutoStartsPaused(t *testing.T) {
	exec := &fakeExec{}
	a := NewAuto(exec, config.AutoPolicy{MinConfidence: 0.5}, autoPolicy(), nil, discardLog())
	if !a.Paused() {
		t.Fatal("NewAuto must start paused (fail closed by construction)")
	}
	if out := a.Run(context.Background(), autoInv(1.0, revAction)); len(exec.ran) != 0 {
		t.Fatalf("a freshly built (paused) auto must not execute before Resume; ran=%+v out=%+v", exec.ran, out)
	}
}

func TestAutoDeniesProtectedNamespace(t *testing.T) {
	exec := &fakeExec{}
	a := NewAuto(exec, config.AutoPolicy{MinConfidence: 0.5}, autoPolicy(), nil, discardLog())
	a.Resume()
	// flux-system is a built-in protected namespace; the exec-boundary re-validation
	// must deny it even though it's reversible and confident.
	protected := providers.Action{Op: "suspend", Reversible: true, Target: providers.Workload{Kind: "Kustomization", Name: "x", Namespace: "flux-system"}}
	out := a.Run(context.Background(), autoInv(1.0, protected))
	if len(exec.ran) != 0 {
		t.Fatalf("auto must deny a protected-namespace target at the exec boundary; ran=%+v", exec.ran)
	}
	if !strings.Contains(out[0].Description, "denied") {
		t.Fatalf("expected denied annotation: %q", out[0].Description)
	}
}

// TestAutoRedactsTheExecutorError closes the third field written PAST the single
// egress chokepoint (after Prior and CurateError).
//
// investigate.LoopInvestigator.deliver redacts and THEN calls OnComplete;
// app.onInvestigationComplete then replaces found.Actions with Auto.Run's output, so
// this annotation lands on an Investigation that will never be scrubbed again. The
// spliced text is a live executor error — a Kubernetes API response body, whatever
// the apiserver chose to put in it — and notify.Format renders Action.Description
// verbatim into the webhook payload's text with no second redaction pass.
func TestAutoRedactsTheExecutorError(t *testing.T) {
	const token = "ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"
	exec := &fakeExec{err: fmt.Errorf("apiserver rejected the patch: bearer %s", token)}
	a := NewAuto(exec, config.AutoPolicy{MinConfidence: 0.5}, autoPolicy(), nil, discardLog())
	a.Resume() // NewAuto starts paused (fail closed); opt in to execution

	out := a.Run(context.Background(), autoInv(0.9, revAction))

	if len(out) != 1 {
		t.Fatalf("expected one annotated action, got %d", len(out))
	}
	if !strings.Contains(out[0].Description, "auto: FAILED") {
		t.Fatalf("a failed execution must still be reported to the human: %q", out[0].Description)
	}
	if strings.Contains(out[0].Description, token) {
		t.Errorf("the executor's error reached a delivered field verbatim — it is annotated "+
			"AFTER redactInvestigation, so nothing downstream will scrub it: %q", out[0].Description)
	}
	// Masking the credential must not cost the operator the diagnosis.
	if !strings.Contains(out[0].Description, "apiserver rejected the patch") {
		t.Errorf("redaction ate the non-secret part of the reason: %q", out[0].Description)
	}
}
