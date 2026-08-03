// SPDX-License-Identifier: Apache-2.0

package app

// TestServeGuardFailClosed locks the fail-closed invariant for RequireWebhookAuth
// on the serve startup path (internal/app/config.go): a model-configured deployment
// with an empty webhook token must refuse to start; a non-empty token must be
// accepted. This is entirely self-contained — it constructs configs in-code so it
// does not depend on any YAML file that another workstream may be editing.
//
// The invariant being locked: once a model is wired, every alert webhook POST
// drives a paid LLM call, so an anonymous webhook would let anyone in the network
// run up an arbitrary bill (or poison the investigation history). The guard lives
// on the serve path only (not in config.Validate) because `lore investigate`
// legitimately needs a model without a webhook.
import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

func TestServeGuardFailClosed(t *testing.T) {
	t.Run("model configured, no webhook token → refused (fail closed)", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Model.Provider = "anthropic" // built-in endpoint; no base_url needed
		// Server.WebhookTokenEnv names the env var — it is set here to show intent,
		// but the resolved token (second arg) is what matters for the guard.
		cfg.Server.WebhookTokenEnv = "RUNLORE_WEBHOOK_TOKEN"

		if err := RequireWebhookAuth(cfg, "" /* empty resolved token */); err == nil {
			t.Fatal("RequireWebhookAuth must refuse when model is configured and webhook token is empty")
		}
	})

	t.Run("model configured, webhook token set → accepted", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Model.Provider = "anthropic"
		cfg.Server.WebhookTokenEnv = "RUNLORE_WEBHOOK_TOKEN"

		if err := RequireWebhookAuth(cfg, "some-secret-token"); err != nil {
			t.Fatalf("RequireWebhookAuth must accept when model is configured and webhook token is non-empty: %v", err)
		}
	})

	t.Run("no model, no webhook token → accepted (log-only investigator has no billing exposure)", func(t *testing.T) {
		cfg := &config.Config{} // zero Model: no provider, no base_url
		if err := RequireWebhookAuth(cfg, ""); err != nil {
			t.Fatalf("RequireWebhookAuth must accept when no model is configured: %v", err)
		}
	})
}

// TestRecallDecayWarningIsEmittedOnceAtStartup pins WHERE the warning is raised.
//
// The condition is pure config, so it is equally true on every incident — which is
// exactly the failure mode to prevent. Called from the investigation path (or from
// wireRecall, or the recall gate) it would repeat the same paragraph on every
// alert and be muted within a day. It belongs on the serve startup path, once,
// next to the other startup config warnings.
//
// `lore investigate` is deliberately NOT a caller: that one-shot CLI never wires a
// ledger into Recall at all (see wireRecall — only the serve path calls it), so
// outcome decay is off there by construction rather than by misconfiguration, and
// warning about it would be noise. `lore curate` already has its own ledger
// startup report (LogLedgerStartup).
func TestRecallDecayWarningIsEmittedOnceAtStartup(t *testing.T) {
	const guarded, caller = "RecallDecayWarning", "RunServe"

	files, err := filepath.Glob(filepath.Join(".", "*.go"))
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	fset := token.NewFileSet()
	callerCalls := 0

	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		src, err := os.ReadFile(path) //nolint:gosec // test-owned path in this package
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(src), guarded) {
			continue
		}
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", path, err)
		}
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil || fn.Name.Name == guarded {
				continue
			}
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != guarded {
					return true
				}
				if fn.Name.Name == caller {
					callerCalls++
				} else {
					t.Errorf("%s: %s calls %s — it must be raised once on the serve startup path, "+
						"not per investigation", filepath.Base(path), fn.Name.Name, guarded)
				}
				return true
			})
		}
	}

	if callerCalls != 1 {
		t.Fatalf("%s must call %s exactly once (got %d) — otherwise the startup warning "+
			"is either absent or duplicated", caller, guarded, callerCalls)
	}
}
