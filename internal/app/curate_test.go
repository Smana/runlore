// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
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
	"github.com/Smana/runlore/internal/providers"
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
	agent := BuildCurateAgent(&config.Config{}, forge, nil, nil, discardLog())
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
	agent = BuildCurateAgent(cfg, forge, ledger, nil, discardLog())
	if len(agent.Passes) != 7 {
		t.Fatalf("full agent: want 7 passes (suppress dedup lifecycle queue recurrence contested retirement), got %d", len(agent.Passes))
	}
}

// The Queue reads the evidence only under factory.curation.defer: adopters keep today's path.
func TestBuildCurateAgentWiresTheQueuesEvidence(t *testing.T) {
	forge := github.New("https://forge.invalid", "o", "r", "main", nil)
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	queue := func(cfg *config.Config) curate.Queue {
		t.Helper()
		for _, p := range BuildCurateAgent(cfg, forge, ledger, nil, discardLog()).Passes {
			if q, ok := p.(curate.Queue); ok {
				return q
			}
		}
		t.Fatal("no Queue pass")
		return curate.Queue{}
	}
	cfg := &config.Config{}
	cfg.Forge.KBRepo = "acme/kb"
	if q := queue(cfg); q.Evidence != nil {
		t.Fatal("defer off: Evidence must stay nil")
	}
	cfg.Factory.Curation.Defer = true
	if q := queue(cfg); q.Evidence == nil || q.KBRepo != "acme/kb" || q.Checker == nil {
		t.Fatalf("defer on: want Evidence, KBRepo and Checker: %+v", q)
	}
}

// The Queue's rebuild force-pushes, so it comes with factory.curation.defer ("off by default"), and
// the agent's Queue has no Promote: Queue.Run lists right after. The queue issue and the command
// promote through a Queue over the same forge, once there is a ledger to read.
func TestQueueIssueScreensUnderDeferOnly(t *testing.T) {
	forge := github.New("https://forge.invalid", "o", "r", "main", nil)
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Forge.KBRepo = "acme/kb"
	if q := buildQueueIssue(cfg, forge, ledger, nil, discardLog()); q.ScreenEntries || q.Evidence == nil {
		t.Fatalf("defer off, with a ledger: Evidence for the rows, and no threat check: %+v", q)
	}
	if q := buildQueueIssue(cfg, forge, nil, nil, discardLog()); q.ScreenEntries {
		t.Fatal("defer off, no ledger: no threat check")
	}
	cfg.Factory.Curation.Defer = true
	if q := buildQueueIssue(cfg, forge, ledger, nil, discardLog()); !q.ScreenEntries {
		t.Fatal("defer on: the threat check runs")
	}
}

func TestRefreshWiring(t *testing.T) {
	forge := github.New("https://forge.invalid", "o", "r", "main", nil)
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Forge.KBRepo = "acme/kb"
	queue := func() curate.Queue {
		t.Helper()
		for _, p := range BuildCurateAgent(cfg, forge, ledger, nil, discardLog()).Passes {
			if q, ok := p.(curate.Queue); ok {
				return q
			}
		}
		t.Fatal("no Queue pass")
		return curate.Queue{}
	}
	if q := queue(); q.Refresh != nil {
		t.Fatal("defer off: the Queue must not rebuild")
	}
	cfg.Factory.Curation.Defer = true
	if q := queue(); q.Refresh == nil || q.Refresh.Forge == nil || q.Refresh.Promote != nil {
		t.Fatalf("defer on: the Queue rebuilds, and lists right after: %+v", q.Refresh)
	}
	if q := queue(); q.Refresh.Evidence == nil {
		t.Fatal("defer on: held evidence makes an unlabelled PR worth rebuilding")
	}
	if qi := buildQueueIssue(cfg, forge, ledger, nil, discardLog()); qi.Reconcile == nil || qi.Refresh.Evidence == nil {
		t.Fatalf("queue-issue reconciles labels first, a ledger being there: %+v", qi)
	}
	if qi := buildQueueIssue(cfg, forge, nil, nil, discardLog()); qi.Reconcile != nil {
		t.Fatal("no ledger: no typed-nil Queue")
	}
	if qi := buildQueueIssue(cfg, forge, nil, nil, discardLog()); qi.Refresh == nil || qi.Refresh.Promote != nil {
		t.Fatalf("no ledger: nothing to promote from: %+v", qi.Refresh)
	}
	if qi := buildQueueIssue(cfg, forge, ledger, nil, discardLog()); qi.Refresh == nil || qi.Refresh.Promote == nil {
		t.Fatalf("a ledger: the queue issue promotes: %+v", qi.Refresh)
	}
	if r := buildRefresh(cfg, forge, nil, discardLog()); r.Promote != nil {
		t.Fatal("no ledger: nothing to promote from")
	}
	if r := buildRefresh(cfg, forge, ledger, discardLog()); r.Promote == nil || r.Forge == nil {
		t.Fatalf("a ledger: the command promotes: %+v", r)
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
	if n := len(BuildCurateAgent(cfg, forge, ledger, nil, discardLog()).Passes); n != 6 {
		t.Fatalf("revalidation must be opt-in: want 6 passes, got %d", n)
	}

	cfg.Curate.Revalidation = config.Revalidation{
		Enabled: true, MinInterval: config.Duration(168 * time.Hour), MaxOpen: 3, Floor: 0.5, Prior: 2.0,
	}
	passes := BuildCurateAgent(cfg, forge, ledger, nil, discardLog()).Passes
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

// With curate.queue_issue.schedule set the Knowledge queue pass is the last pass, whether or not
// there is a ledger; unset, nothing is added.
func TestBuildCurateAgentAddsTheQueueIssuePass(t *testing.T) {
	forge := github.New("https://forge.invalid", "o", "r", "main", nil)
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	last := func(cfg *config.Config, l *outcome.Ledger) curate.Pass {
		t.Helper()
		ps := BuildCurateAgent(cfg, forge, l, nil, discardLog()).Passes
		return ps[len(ps)-1]
	}
	cfg := &config.Config{}
	cfg.Forge.KBRepo = "acme/kb"
	if _, ok := last(cfg, nil).(curate.QueueIssue); ok {
		t.Fatal("no schedule: no queue-issue pass")
	}
	cfg.Curate.QueueIssue = config.QueueIssue{Schedule: "Tue 09:30"}
	for name, l := range map[string]*outcome.Ledger{"no ledger": nil, "ledger": ledger} {
		qi, ok := last(cfg, l).(curate.QueueIssue)
		if !ok {
			t.Fatalf("%s: the queue-issue pass must be the last one", name)
		}
		if qi.Weekday != time.Tuesday || qi.Hour != 9 || qi.Minute != 30 || qi.Unscheduled || qi.KBRepo != "acme/kb" {
			t.Fatalf("%s: %+v", name, qi)
		}
		if (l == nil) != (qi.Stats == nil) || (l == nil) != (qi.Evidence == nil) {
			t.Fatalf("%s: a nil ledger must not become a typed-nil interface: %+v", name, qi)
		}
	}
}

// writeCurateConfig writes a config with working GitHub App credentials and returns its path.
func writeCurateConfig(t *testing.T, extra string) string {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_CURATE_SUB_KEY", string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})))
	p := filepath.Join(t.TempDir(), "runlore.yaml")
	body := "forge:\n  kb_repo: acme/kb\n  github_app: {app_id: 1, installation_id: 2, private_key_env: TEST_CURATE_SUB_KEY}\n" + extra
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// The scheduled pass takes the same gate as Queue.Refresh: a schedule alone does not force-push.
// The explicit commands stay available. The agent's Queue just ran, so it does not run again.
func TestScheduledQueueIssueRefreshesOnlyUnderDefer(t *testing.T) {
	forge := github.New("https://forge.invalid", "o", "r", "main", nil)
	ledger, err := outcome.New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{}
	cfg.Forge.KBRepo = "acme/kb"
	cfg.Curate.QueueIssue = config.QueueIssue{Schedule: "Tue 09:30"}
	last := func() curate.QueueIssue {
		t.Helper()
		ps := BuildCurateAgent(cfg, forge, ledger, nil, discardLog()).Passes
		qi, ok := ps[len(ps)-1].(curate.QueueIssue)
		if !ok {
			t.Fatalf("last pass %T", ps[len(ps)-1])
		}
		return qi
	}
	if qi := last(); qi.Refresh != nil || qi.Reconcile != nil {
		t.Fatalf("defer off: %+v", qi)
	}
	cfg.Factory.Curation.Defer = true
	if qi := last(); qi.Refresh == nil || qi.Reconcile != nil {
		t.Fatalf("defer on: %+v", qi)
	}
	cfg.Factory.Curation.Defer = false
	if qi := buildQueueIssue(cfg, forge, ledger, nil, discardLog()); qi.Refresh == nil {
		t.Fatal("the explicit command refreshes either way")
	}
}

func TestRunCurateSubcommand(t *testing.T) {
	var groomed, issued, refreshed, gotOnly int
	var gotDry bool
	oldGroom, oldIssue, oldRefresh := runGroom, runQueueIssue, runRefresh
	t.Cleanup(func() { runGroom, runQueueIssue, runRefresh = oldGroom, oldIssue, oldRefresh })
	runRefresh = func(_ context.Context, r curate.RefreshEntries) error {
		refreshed++
		gotOnly = r.Only
		gotDry = r.Forge.(curate.Guard).DryRun
		return nil
	}
	runGroom = func(context.Context, *config.Config, curate.GuardedForge, *outcome.Ledger, providers.LineNotifier, *slog.Logger) {
		groomed++
	}
	runQueueIssue = func(_ context.Context, q curate.QueueIssue) error {
		issued++
		gotDry = q.Forge.(curate.Guard).DryRun
		return nil
	}
	f := writeCurateConfig(t, "")
	reset := func() { groomed, issued, refreshed, gotDry = 0, 0, 0, false }

	if err := RunCurate([]string{"--config", f}); err != nil || groomed != 1 || issued != 0 {
		t.Fatalf("plain curate grooms and writes no issue: %v groomed=%d issued=%d", err, groomed, issued)
	}
	reset()
	if err := RunCurate([]string{"queue-issue", "--config", f}); err != nil || groomed != 0 || issued != 1 || gotDry {
		t.Fatalf("queue-issue does not groom: it reconciles labels, refreshes and rewrites, all inside QueueIssue: %v groomed=%d issued=%d dry=%v", err, groomed, issued, gotDry)
	}
	reset()
	if err := RunCurate([]string{"queue-issue", "--dry-run", "--config", f}); err != nil || groomed != 0 || issued != 1 || !gotDry {
		t.Fatalf("queue-issue --dry-run passes DryRun: %v groomed=%d issued=%d dry=%v", err, groomed, issued, gotDry)
	}
	reset()
	if err := RunCurate([]string{"refresh-entries", "--config", f}); err != nil || groomed != 0 || issued != 0 || refreshed != 1 || gotDry {
		t.Fatalf("refresh-entries runs that pass alone: %v groomed=%d issued=%d refreshed=%d dry=%v", err, groomed, issued, refreshed, gotDry)
	}
	reset()
	if err := RunCurate([]string{"refresh-entries", "--dry-run", "--config", f}); err != nil || groomed != 0 || issued != 0 || refreshed != 1 || !gotDry {
		t.Fatalf("refresh-entries --dry-run passes DryRun: %v groomed=%d issued=%d refreshed=%d dry=%v", err, groomed, issued, refreshed, gotDry)
	}
	reset()
	if err := RunCurate([]string{"refresh-entries", "--pr", "42", "--config", f}); err != nil || refreshed != 1 || gotOnly != 42 {
		t.Fatalf("refresh-entries --pr names the one: %v refreshed=%d only=%d", err, refreshed, gotOnly)
	}
	gotOnly = 0
	for _, args := range [][]string{{"queue-issue", "--pr", "42", "--config", f}, {"--pr", "42", "--config", f}, {"refresh-entries", "--pr", "-1", "--config", f}} {
		reset()
		if err := RunCurate(args); err == nil || groomed != 0 || issued != 0 || refreshed != 0 {
			t.Fatalf("%v: --pr belongs to refresh-entries, and a number: %v groomed=%d issued=%d refreshed=%d", args, err, groomed, issued, refreshed)
		}
	}
	for _, args := range [][]string{{"--config", f, "queue-issue"}, {"--config", f, "refresh-entries"}, {"bogus", "--config", f}, {"--config", f, "bogus"}} {
		reset()
		if err := RunCurate(args); err == nil || groomed != 0 || issued != 0 || refreshed != 0 {
			t.Fatalf("%v: a positional that is not the first argument is an error and runs nothing: %v groomed=%d issued=%d refreshed=%d", args, err, groomed, issued, refreshed)
		}
	}
}

func TestQueueLine(t *testing.T) {
	cfg := &config.Config{}
	t.Setenv("TEST_QUEUE_HOOK", "https://hooks.invalid/x")
	cfg.Notify.Slack.WebhookURLEnv = "TEST_QUEUE_HOOK"
	if queueLine(cfg, false, discardLog()) != nil {
		t.Fatal("slack off: no line")
	}
	cfg.Curate.QueueIssue.Slack = true
	if queueLine(cfg, true, discardLog()) != nil {
		t.Fatal("dry-run: no line")
	}
	if queueLine(cfg, false, discardLog()) == nil {
		t.Fatal("slack on and a notifier configured: a line")
	}
	if queueLine(&config.Config{Curate: config.Curate{QueueIssue: config.QueueIssue{Slack: true}}}, false, discardLog()) != nil {
		t.Fatal("no notifier configured: no line, not a typed nil")
	}
}
