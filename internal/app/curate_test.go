// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curate"
	github "github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/outcome"
)

// The retirement and revalidation passes are wired in RunCurate with
// *github.Client as their forge and *outcome.Ledger as their stats source; pin
// every seam at compile time so a drift in any of them fails here rather than in
// the wiring block.
var (
	_ curate.RetireForge     = (*github.Client)(nil)
	_ curate.RevalidateForge = (*github.Client)(nil)
	_ curate.EntryStats      = (*outcome.Ledger)(nil)
	_ curate.GuardedForge    = (*github.Client)(nil)
)

// captureLog returns a logger writing JSON records into buf so a test can assert
// the level/message emitted by LogLedgerStartup.
func captureLog(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// lastRecord decodes the final JSON log line in buf.
func lastRecord(t *testing.T, buf *bytes.Buffer) map[string]any {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(buf.String()), "\n")
	if len(lines) == 0 || lines[0] == "" {
		t.Fatalf("no log records emitted")
	}
	var rec map[string]any
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &rec); err != nil {
		t.Fatalf("decode log line %q: %v", lines[len(lines)-1], err)
	}
	return rec
}

func TestLogLedgerStartupAbsentWarns(t *testing.T) {
	// ledger_path set but the file does not exist where curate runs — the silent
	// no-op the warning must surface.
	l, _ := outcome.New(filepath.Join(t.TempDir(), "missing.jsonl"))
	var buf bytes.Buffer
	LogLedgerStartup(captureLog(&buf), l.Status())
	rec := lastRecord(t, &buf)
	if rec["level"] != "WARN" {
		t.Fatalf("absent ledger must WARN, got level=%v msg=%v", rec["level"], rec["msg"])
	}
}

func TestLogLedgerStartupEmptyWarns(t *testing.T) {
	// Present but empty: passes will run yet do nothing — still worth a warning so a
	// misconfigured mount (fresh emptyDir per Job) is not mistaken for "no work".
	l, _ := outcome.New(filepath.Join(t.TempDir(), "o.jsonl")) // file never created
	var buf bytes.Buffer
	LogLedgerStartup(captureLog(&buf), l.Status())
	if rec := lastRecord(t, &buf); rec["level"] != "WARN" {
		t.Fatalf("empty ledger must WARN, got level=%v", rec["level"])
	}
}

func TestLogLedgerStartupPresentInfos(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := outcome.New(p)
	t0 := time.Unix(1000, 0)
	_ = l.Open(outcome.Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0})
	_, _, _ = l.Resolve("fp", t0.Add(time.Minute))
	var buf bytes.Buffer
	LogLedgerStartup(captureLog(&buf), l.Status())
	rec := lastRecord(t, &buf)
	if rec["level"] != "INFO" {
		t.Fatalf("a present, non-empty ledger must INFO, got level=%v", rec["level"])
	}
}

func TestLogLedgerStartupDisabledInfos(t *testing.T) {
	// Feature off (no ledger_path): a plain info that the passes are skipped, no warning.
	l, _ := outcome.New("")
	var buf bytes.Buffer
	LogLedgerStartup(captureLog(&buf), l.Status())
	rec := lastRecord(t, &buf)
	if rec["level"] != "INFO" {
		t.Fatalf("disabled ledger must INFO (not WARN), got level=%v", rec["level"])
	}
}

func TestBuildCurateAgentPassComposition(t *testing.T) {
	// Constructing a github.Client performs no I/O, so it is a safe stand-in.
	forge := github.New("https://forge.invalid", "o", "r", "main", nil)

	// No ledger: forge-only passes (Suppress, Dedup, Lifecycle).
	agent := BuildCurateAgent(&config.Config{}, forge, nil, discardLog())
	if len(agent.Passes) != 3 {
		t.Fatalf("no-ledger agent: want 3 passes, got %d", len(agent.Passes))
	}

	// Ledger + retirement enabled: all seven.
	cfg := &config.Config{}
	cfg.Curate.Retirement = config.Retirement{Enabled: true, MinObservations: 3, Floor: 0.5, Prior: 2.0}
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	agent = BuildCurateAgent(cfg, forge, ledger, discardLog())
	if len(agent.Passes) != 7 {
		t.Fatalf("full agent: want 7 passes (suppress dedup lifecycle queue recurrence contested retirement), got %d", len(agent.Passes))
	}
}

// TestBuildCurateAgentWiresRevalidation pins the opt-in wiring of the
// revalidation pass, including that its knobs come from config rather than from
// the pass's zero value (a MaxOpen of 0 would silently propose nothing).
func TestBuildCurateAgentWiresRevalidation(t *testing.T) {
	forge := github.New("https://forge.invalid", "o", "r", "main", nil)
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}

	// Default (disabled): the ledger-backed set stops at Contested.
	cfg := &config.Config{}
	if n := len(BuildCurateAgent(cfg, forge, ledger, discardLog()).Passes); n != 6 {
		t.Fatalf("revalidation must be opt-in: want 6 passes, got %d", n)
	}

	cfg.Curate.Revalidation = config.Revalidation{
		Enabled: true, MinInterval: config.Duration(168 * time.Hour), MaxOpen: 3, Floor: 0.5, Prior: 2.0,
	}
	passes := BuildCurateAgent(cfg, forge, ledger, discardLog()).Passes
	if len(passes) != 7 {
		t.Fatalf("want 7 passes with revalidation enabled, got %d", len(passes))
	}
	p, ok := passes[6].(curate.Revalidation)
	if !ok {
		t.Fatalf("last pass = %T, want curate.Revalidation", passes[6])
	}
	if p.MinInterval != 168*time.Hour || p.MaxOpen != 3 || p.Floor != 0.5 || p.Prior != 2.0 {
		t.Fatalf("revalidation knobs not wired from config: %+v", p)
	}
}

// TestRunCurateGitLabNamesTheRealLimitation pins the error a GitLab operator gets.
//
// BuildForgeTokenSource only mints GitHub App tokens, so a GitLab-configured
// deployment always fails this check — but NOT because anything is misconfigured:
// Phase-2 grooming simply has no GitLab code path. The old message told them to go
// configure a GitHub App, which they cannot have and which would not help. That is
// a wild goose chase, so the message must name the real limitation instead.
func TestRunCurateGitLabNamesTheRealLimitation(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "runlore.yaml")
	if err := os.WriteFile(cfgPath, []byte(`
forge:
  provider: gitlab
  kb_repo: acme/kb
  gitlab:
    token_env: TEST_CURATE_GL_TOKEN
`), 0o600); err != nil {
		t.Fatal(err)
	}

	err := RunCurate([]string{"-config", cfgPath})
	if err == nil {
		t.Fatal("lore curate must fail for a GitLab-configured deployment")
	}
	msg := err.Error()
	if strings.Contains(strings.ToLower(msg), "github app") {
		t.Fatalf("must not send a GitLab operator after a GitHub App they cannot have: %q", msg)
	}
	for _, want := range []string{"GitLab", "grooming"} {
		if !strings.Contains(msg, want) {
			t.Fatalf("error must name the real limitation (missing %q): %q", want, msg)
		}
	}
}
