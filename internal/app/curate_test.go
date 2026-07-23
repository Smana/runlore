// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curate"
	github "github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/outcome"
)

// The retirement pass is wired in RunCurate with *github.Client as its forge and
// *outcome.Ledger as its stats source; pin both seams at compile time so a drift in
// either interface fails here rather than in the wiring block.
var (
	_ curate.RetireForge  = (*github.Client)(nil)
	_ curate.RetireStats  = (*outcome.Ledger)(nil)
	_ curate.GuardedForge = (*github.Client)(nil)
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
