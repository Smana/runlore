# D2 — Curator automation phase 2 Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the KB-lifecycle sweeps production-ready *inside the serve pod*: the grooming passes (suppression of human-rejected entries, dedup, stale-close, queue promotion, recurrence escalation, contested warnings, retirement) run on a leader-only timer with **no CronJob, no shared-volume footgun, and no new required config** — defaulting to a **dry-run (log/audit-only) posture** so the first release observes before it acts. This answers the project's #1 adoption objection: PR fatigue from an ungroomed queue.

**Architecture:** The pass library in `internal/curate/` is **already complete and tested** (see "Current state" below) — this plan does *not* rewrite it. The work is four seams around it: (1) a `curate.Guard` forge wrapper that gives every pass audit records + dry-run at a single choke point (mirroring `action.NewAuditedExecutor`, `internal/app/serve.go:129`); (2) one genuinely missing pass, `Suppress`, that closes re-drafted PRs for incidents a human already rejected via close-without-merge labels; (3) a `curate.Sweeper` ticker that runs the existing `curate.Agent` on an interval, started inside `startWork` in `RunServe` so it is **leader-only** and cancelled on leadership loss (`internal/app/serve.go:290-336`); (4) `curate.sweeps.{mode,interval}` config with defaults `dry-run`/`6h`, plus a shared `BuildCurateAgent` so the CLI (`lore curate`) and the in-server sweeper can never drift. The in-server sweeps use the **live** `*outcome.Ledger` the serve pod already holds (`internal/app/serve.go:174`), eliminating the CronJob's classic misconfiguration (ledger on an unshared volume — see `LogLedgerStartup`, `internal/app/curate.go:124`).

**Tech Stack:** Go 1.x (stdlib `time.Ticker`, `log/slog`), existing packages `internal/curate`, `internal/audit` (hash-chained JSONL auditor), `internal/forge/github`, `internal/outcome`, `internal/config` (yaml.v3). Helm chart `deploy/helm/runlore`. No new dependencies.

---

## Current state (research findings — verified 2026-07-23 on branch `roadmap/v0.11.0`)

Per-module status. **This item's "phase 2" passes are NOT skeletons** — they exist, are unit-tested, and are wired into the one-shot `lore curate` CLI. What is missing is scheduling, safety posture, auditability, and one pass.

| Module | Status | Evidence |
|---|---|---|
| `internal/curate/curate.go` — `Pass`/`Agent` runner | **exists and tested** | `curate_test.go` (per-pass error isolation) |
| `internal/curate/dedup.go` — duplicate-PR close (fingerprint-first, Jaccard fallback) | **exists and tested** | `dedup_test.go` (6 cases incl. protected labels) |
| `internal/curate/lifecycle.go` — stale-close sweep (`stale_after`), protected labels | **exists and tested** | `lifecycle_test.go` |
| `internal/curate/resolution.go` — `Queue` (solved→ready-to-merge) + `LedgerResolutionChecker` | **exists and tested** | `resolution_test.go` |
| `internal/curate/recurrence.go` — knowledge-gap issues + closed-PR escalation | **exists and tested** | `recurrence_test.go` |
| `internal/curate/suppression.go` — `ClosedPRSuppression` (derives suppressed fingerprints from closed-unmerged PRs, `wontfix`/`not-kb-worthy` vs `needs-work`) | **exists and tested** — but only *consulted by Recurrence*; nothing closes a **re-drafted open PR** for a suppressed fingerprint (the drafter's dedup checks only open PRs + merged entries, `internal/curator/curator.go:110-133`) | `suppression_test.go` |
| `internal/curate/contested.go` — 👎 warnings on open KB PRs | **exists and tested** | `contested_test.go` |
| `internal/curate/retirement.go` — retire-PR proposals for decayed entries, human-veto-aware | **exists and tested** | `retirement_test.go` |
| `internal/forge/github` — `ListPRsByLabel`, `ListClosedUnmergedPRsByLabel`, `Comment`, `Close`, `ReplaceLabel`, `OpenIssue`, `OpenRetirePR`, `ListIssueCommentBodies`, `IsPROpen` | **exists and tested** | `github.go:241,258,285,305,351,363,101,315,339`, `retire.go:64`; `github_test.go`, `retire_test.go` |
| `internal/app/curate.go` — `RunCurate` CLI wiring of all passes | **exists and tested** (compile-time seam pins at `curate_test.go:22-25`) | wiring block lines 59–107 |
| Helm CronJob (`curate.cronjob.enabled`, default off) | **exists** | `deploy/helm/runlore/templates/cronjob.yaml`, `values.yaml:494-500` |
| **In-server scheduler** (sweeps in the serve pod, leader-aware) | **missing — the D2 gap** | no `curate` reference anywhere in `internal/app/serve.go` |
| **Dry-run / log-only mode** for the passes | **missing** | no dry-run flag or wrapper anywhere in `internal/curate` |
| **Audit records** for automated forge actions | **missing** | `internal/audit` is only used by action executors (`internal/app/action.go:30`) |
| **Suppression sweep** (close re-drafts of human-rejected entries) | **missing** (see suppression row above) | — |
| `curate.*` config | `stale_after`, `recurrence_threshold`, `retirement.*` **exist, validated, defaulted** (`internal/config/config.go:1268-1287`, `:1221-1230`; `load.go:190-205`) — **no scheduling/mode keys** | `config_retirement_test.go`, `config_test.go:132-136` |
| Leader election | **exists** — `startWork(workCtx)` runs leader-only loops; ticker precedent: `Reinvestigator.Poll` (`internal/investigate/reinvestigate.go:34-48`) started at `serve.go:315` | `serve.go:290-336`, `leader_identity.go` |

**Locked design decisions:**

- **Default posture: dry-run.** Sweeps auto-enable whenever forge credentials (`forge.github_app` + `forge.kb_repo`) already exist, but in `dry-run` mode they perform **zero forge writes** — every candidate action is slog-logged and audit-recorded (`Decision: dry-run`). Auto-writing to the user's repo on upgrade would violate least-surprise; auto-*observing* is safe and demonstrates value ("sweeps would have closed 7 stale PRs"). `mode: apply` is one config line. This satisfies "no new REQUIRED config": all keys default.
- **Leader-only, flap-proof.** The sweeper starts inside `startWork` (cancelled with `workCtx` on leadership loss) and waits **one full interval before the first sweep**, so leader flaps can never stampede the forge with immediate re-sweeps.
- **One write seam.** Dry-run + audit live in a `Guard` wrapper around the forge client, not in each pass — the passes stay untouched and their tests stay valid.
- **CronJob stays** (documented as the alternative for users who want sweeps out of the serve pod); both paths share `BuildCurateAgent`, and every pass is idempotent, so running both is safe (just wasteful).

---

### Task 1: Config — `curate.sweeps.{mode,interval}` with dry-run/6h defaults and validation

**Files:**
- `internal/config/config.go` — add `Sweeps` field to `Curate` (struct at line 1268-1279), new `Sweeps` type + mode constants after `Retirement` (line 1281-1287), validation clause next to the retirement clause (after line 1230)
- `internal/config/load.go` — default fill in `applyDefaults` (after the retirement block, line 190-205)
- `internal/config/config_sweeps_test.go` — new

**Steps:**

- [ ] Write the failing test `internal/config/config_sweeps_test.go` (style: `config_retirement_test.go` — partial `Config` structs, `strings.Contains` on `Validate()` errors):

```go
// SPDX-License-Identifier: Apache-2.0

package config

import (
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

func TestSweepsDefaults(t *testing.T) {
	// Zero value ⇒ sweeps enabled in dry-run at 6h: safe-by-default, no required config.
	var c Config
	applyDefaults(&c)
	if got := c.Curate.Sweeps.Interval.Std(); got != 6*time.Hour {
		t.Fatalf("sweeps.interval default: want 6h, got %v", got)
	}
	if !c.Curate.Sweeps.Enabled() || !c.Curate.Sweeps.DryRun() {
		t.Fatalf("zero-value sweeps must be enabled in dry-run, got mode=%q", c.Curate.Sweeps.Mode)
	}
}

func TestSweepsModeParsesAndGates(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("curate:\n  sweeps:\n    mode: apply\n    interval: 1h\n"), &c); err != nil {
		t.Fatal(err)
	}
	if c.Curate.Sweeps.DryRun() || !c.Curate.Sweeps.Enabled() {
		t.Fatalf("mode: apply must be enabled and not dry-run, got %q", c.Curate.Sweeps.Mode)
	}
	if got := c.Curate.Sweeps.Interval.Std(); got != time.Hour {
		t.Fatalf("interval: want 1h, got %v", got)
	}
	var off Config
	if err := yaml.Unmarshal([]byte("curate:\n  sweeps:\n    mode: \"off\"\n"), &off); err != nil {
		t.Fatal(err)
	}
	if off.Curate.Sweeps.Enabled() {
		t.Fatal("mode: off must disable sweeps")
	}
}

func TestSweepsValidation(t *testing.T) {
	bad := Config{Curate: Curate{Sweeps: Sweeps{Mode: "sometimes"}}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "curate.sweeps.mode") {
		t.Fatalf("unknown mode must error on curate.sweeps.mode, got %v", err)
	}
	fast := Config{Curate: Curate{Sweeps: Sweeps{Interval: Duration(time.Minute)}}}
	if err := fast.Validate(); err == nil || !strings.Contains(err.Error(), "curate.sweeps.interval") {
		t.Fatalf("sub-10m interval must error on curate.sweeps.interval, got %v", err)
	}
}
```

- [ ] Run `go test ./internal/config/ -run 'TestSweeps' -v` — expect **FAIL** (compile error: `unknown field Sweeps in struct literal of type Curate`)
- [ ] Implement in `internal/config/config.go`. Add to the `Curate` struct (after the `Retirement` field, line 1278):

```go
	// Sweeps configures the in-server scheduled grooming loop (leader-only, run by
	// the serve pod). Default mode is dry-run: candidates are logged and audited but
	// no forge write happens until the operator sets mode: apply. mode: off disables
	// the loop entirely (the opt-in CronJob remains the out-of-server alternative).
	Sweeps Sweeps `yaml:"sweeps"`
```

  and after the `Retirement` type (line 1287):

```go
// Sweep modes: dry-run observes (log + audit, zero forge writes), apply acts,
// off disables the in-server loop. Empty means dry-run — safe by default.
const (
	SweepOff    = "off"
	SweepDryRun = "dry-run"
	SweepApply  = "apply"
)

// Sweeps configures the in-server scheduled grooming sweeps.
type Sweeps struct {
	Mode     string   `yaml:"mode"`     // "" ⇒ dry-run | "apply" | "off"
	Interval Duration `yaml:"interval"` // default 6h; the first sweep waits one full interval
}

// Enabled reports whether the in-server sweep loop should run at all.
func (s Sweeps) Enabled() bool { return s.Mode != SweepOff }

// DryRun reports whether sweeps must not write to the forge (the default posture).
func (s Sweeps) DryRun() bool { return s.Mode == "" || s.Mode == SweepDryRun }
```

- [ ] Add the validation clause in `Validate()` (immediately after the retirement clause ending at line 1230):

```go
	// In-server sweeps: an unknown mode must fail loud (a typo like "apply" silently
	// falling back to dry-run would mean the operator believes grooming is live when
	// it is not), and a sub-10m interval would hammer the forge listing endpoints.
	switch c.Curate.Sweeps.Mode {
	case "", SweepOff, SweepDryRun, SweepApply:
	default:
		return fmt.Errorf("unknown curate.sweeps.mode %q (want off|dry-run|apply; empty = dry-run)", c.Curate.Sweeps.Mode)
	}
	if iv := c.Curate.Sweeps.Interval.Std(); iv != 0 && iv < 10*time.Minute {
		return fmt.Errorf("curate.sweeps.interval must be >= 10m (forge-listing rate protection), got %v", iv)
	}
```

  (`time` is already imported by `config.go` for `Duration`, line 813.)
- [ ] Add the default in `applyDefaults` (`internal/config/load.go`, after the retirement block at line 205):

```go
	// In-server sweep interval: always filled (harmless when mode: off) so callers
	// never see a zero interval.
	if c.Curate.Sweeps.Interval == 0 {
		c.Curate.Sweeps.Interval = Duration(6 * time.Hour)
	}
```

- [ ] Run `go test ./internal/config/ -run 'TestSweeps' -v` — expect **PASS**; then `go test ./internal/config/` — expect **PASS** (no regression, incl. `shipped_configs_test.go` / `minimal_values_test.go`)
- [ ] Commit: `feat(config): curate.sweeps mode+interval — dry-run/6h defaults, validated`

---

### Task 2: `curate.Guard` — one audited, dry-run-able seam for every forge write

**Files:**
- `internal/curate/guard.go` — new
- `internal/curate/guard_test.go` — new
- `internal/app/curate_test.go` — extend the compile-time pin block (lines 22-25)

**Steps:**

- [ ] Write the failing test `internal/curate/guard_test.go` (fake style: `dedup_test.go:16-38` `fakeForge`; the recording-auditor pattern is new but mirrors `fakeForge`):

```go
// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/providers"
)

// Guard must satisfy every pass-facing forge surface, so one wrapper covers all passes.
var (
	_ Forge          = Guard{}
	_ RetireForge    = Guard{}
	_ ClosedPRLister = Guard{}
	_ ContestedForge = Guard{}
)

// fakeGuarded extends the shared fakeForge with the wider GuardedForge surface.
type fakeGuarded struct {
	fakeForge
	retired  []string
	closeErr error
}

func (f *fakeGuarded) ListClosedUnmergedPRsByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return nil, nil
}
func (f *fakeGuarded) ListIssueCommentBodies(context.Context, int) ([]string, error) { return nil, nil }
func (f *fakeGuarded) IsPROpen(context.Context, int) (bool, error)                   { return true, nil }
func (f *fakeGuarded) OpenRetirePR(_ context.Context, entryPath, _ string) (providers.Ref, error) {
	f.retired = append(f.retired, entryPath)
	return providers.Ref{URL: "https://forge/pr/1"}, nil
}
func (f *fakeGuarded) Close(ctx context.Context, n int) error {
	if f.closeErr != nil {
		return f.closeErr
	}
	return f.fakeForge.Close(ctx, n)
}

// recAudit records audit entries in memory.
type recAudit struct{ recs []audit.Record }

func (r *recAudit) Log(rec audit.Record) error { r.recs = append(r.recs, rec); return nil }

func discardLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestGuardDryRunSkipsWritesButAudits(t *testing.T) {
	inner, aud := &fakeGuarded{}, &recAudit{}
	g := Guard{Inner: inner, DryRun: true, Audit: aud, Log: discardLog()}
	if err := g.Close(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if _, err := g.OpenRetirePR(context.Background(), "entries/a.md", "body"); err != nil {
		t.Fatal(err)
	}
	if len(inner.closed) != 0 || len(inner.retired) != 0 {
		t.Fatalf("dry-run must not reach the forge: closed=%v retired=%v", inner.closed, inner.retired)
	}
	if len(aud.recs) != 2 || aud.recs[0].Decision != audit.DecisionDryRun || aud.recs[0].Actor != "curate" {
		t.Fatalf("want 2 dry-run audit records with actor=curate, got %+v", aud.recs)
	}
	if aud.recs[0].Op != "kb.close" || aud.recs[0].Target != "pr/7" {
		t.Fatalf("record[0] = %+v, want op kb.close target pr/7", aud.recs[0])
	}
}

func TestGuardApplyExecutesAndAudits(t *testing.T) {
	inner, aud := &fakeGuarded{}, &recAudit{}
	g := Guard{Inner: inner, DryRun: false, Audit: aud, Log: discardLog()}
	if err := g.Comment(context.Background(), 9, "back-ref\ndetails"); err != nil {
		t.Fatal(err)
	}
	if len(inner.commented) != 1 || inner.commented[0] != 9 {
		t.Fatalf("apply must reach the forge, got %v", inner.commented)
	}
	if len(aud.recs) != 1 || aud.recs[0].Decision != audit.DecisionExecuted || aud.recs[0].Reason != "back-ref" {
		t.Fatalf("want 1 executed record with first-line reason, got %+v", aud.recs)
	}
}

func TestGuardFailureIsAuditedAndPropagated(t *testing.T) {
	boom := errors.New("forge 502")
	inner, aud := &fakeGuarded{closeErr: boom}, &recAudit{}
	g := Guard{Inner: inner, DryRun: false, Audit: aud, Log: discardLog()}
	if err := g.Close(context.Background(), 3); !errors.Is(err, boom) {
		t.Fatalf("error must propagate, got %v", err)
	}
	if len(aud.recs) != 1 || aud.recs[0].Decision != audit.DecisionFailed {
		t.Fatalf("want a failed audit record, got %+v", aud.recs)
	}
}

func TestGuardReadsPassThrough(t *testing.T) {
	inner := &fakeGuarded{fakeForge: fakeForge{prs: []providers.CuratedIssue{{Number: 1}}}}
	g := Guard{Inner: inner, DryRun: true, Log: discardLog()} // nil Audit must be safe
	prs, err := g.ListPRsByLabel(context.Background(), "runlore")
	if err != nil || len(prs) != 1 {
		t.Fatalf("reads must pass through even in dry-run: %v %v", prs, err)
	}
}
```

- [ ] Run `go test ./internal/curate/ -run TestGuard -v` — expect **FAIL** (`undefined: Guard`)
- [ ] Implement `internal/curate/guard.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/providers"
)

// GuardedForge is the union of every read and write the grooming passes perform —
// the surface Guard wraps. *github.Client satisfies it (pinned in internal/app).
type GuardedForge interface {
	Forge
	ListClosedUnmergedPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	ListIssueCommentBodies(ctx context.Context, number int) ([]string, error)
	IsPROpen(ctx context.Context, number int) (bool, error)
	OpenRetirePR(ctx context.Context, entryPath, body string) (providers.Ref, error)
}

// Guard is the sweep-safety seam around the forge: reads pass through untouched;
// every write is recorded in the audit chain and, in dry-run, logged instead of
// executed. One wrapper gives every pass dry-run + audit without touching the
// passes themselves — the KB mirror of action.NewAuditedExecutor.
type Guard struct {
	Inner  GuardedForge
	DryRun bool
	Audit  audit.Auditor // nil-safe: nil drops records (actions are still slog-logged)
	Log    *slog.Logger
}

// Reads: pass-through (a dry-run sweep must still SEE the queue to report on it).

func (g Guard) ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	return g.Inner.ListPRsByLabel(ctx, label)
}

func (g Guard) ListIssuesByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	return g.Inner.ListIssuesByLabel(ctx, label)
}

func (g Guard) ListClosedUnmergedPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	return g.Inner.ListClosedUnmergedPRsByLabel(ctx, label)
}

func (g Guard) ListIssueCommentBodies(ctx context.Context, number int) ([]string, error) {
	return g.Inner.ListIssueCommentBodies(ctx, number)
}

func (g Guard) IsPROpen(ctx context.Context, number int) (bool, error) {
	return g.Inner.IsPROpen(ctx, number)
}

// Writes: audited, dry-run-able.

func (g Guard) Comment(ctx context.Context, number int, body string) error {
	return g.write("kb.comment", fmt.Sprintf("pr/%d", number), firstLine(body),
		func() error { return g.Inner.Comment(ctx, number, body) })
}

func (g Guard) ReplaceLabel(ctx context.Context, number int, remove, add string) error {
	return g.write("kb.relabel", fmt.Sprintf("pr/%d", number), fmt.Sprintf("%s -> %s", remove, add),
		func() error { return g.Inner.ReplaceLabel(ctx, number, remove, add) })
}

func (g Guard) Close(ctx context.Context, number int) error {
	return g.write("kb.close", fmt.Sprintf("pr/%d", number), "",
		func() error { return g.Inner.Close(ctx, number) })
}

func (g Guard) OpenIssue(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	var ref providers.Ref
	err := g.write("kb.open-issue", firstLine(inv.Title), "", func() error {
		var ierr error
		ref, ierr = g.Inner.OpenIssue(ctx, inv)
		return ierr
	})
	return ref, err
}

func (g Guard) OpenRetirePR(ctx context.Context, entryPath, body string) (providers.Ref, error) {
	var ref providers.Ref
	err := g.write("kb.retire-pr", entryPath, "", func() error {
		var ierr error
		ref, ierr = g.Inner.OpenRetirePR(ctx, entryPath, body)
		return ierr
	})
	return ref, err
}

// write is the single choke point for every forge mutation a grooming pass performs.
// Dry-run returns nil so a pass's comment-then-close sequencing (Lifecycle, Dedup,
// Suppress) walks both steps and both are visible in the dry-run report.
func (g Guard) write(op, target, reason string, do func() error) error {
	if g.DryRun {
		g.Log.Info("curate dry-run: skipped forge write", "op", op, "target", target, "detail", reason)
		g.record(op, target, audit.DecisionDryRun, reason)
		return nil
	}
	if err := do(); err != nil {
		g.record(op, target, audit.DecisionFailed, err.Error())
		return err
	}
	g.record(op, target, audit.DecisionExecuted, reason)
	return nil
}

// record appends to the audit chain; a failed audit write must never abort the
// sweep (the action itself already happened or was skipped) — warn and continue.
func (g Guard) record(op, target string, d audit.Decision, reason string) {
	if g.Audit == nil {
		return
	}
	if err := g.Audit.Log(audit.Record{Actor: "curate", Op: op, Target: target, Decision: d, Reason: reason}); err != nil {
		g.Log.Warn("curate audit write failed", "op", op, "target", target, "err", err)
	}
}

// firstLine caps free text to a one-line hint for the audit Reason field (the full
// body lives on the forge artifact itself).
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	const max = 120
	if len(s) > max {
		s = s[:max]
	}
	return s
}
```

  Note: reuses `audit.Record`/`Decision*` from `internal/audit/audit.go:31-49`; `DecisionDryRun` already exists (line 33).
- [ ] Run `go test ./internal/curate/ -run TestGuard -v` — expect **PASS**
- [ ] Pin the union against the real client in `internal/app/curate_test.go` (extend the var block at lines 22-25):

```go
	_ curate.GuardedForge = (*github.Client)(nil)
```

- [ ] Run `go test ./internal/curate/ ./internal/app/` — expect **PASS**
- [ ] Commit: `feat(curate): Guard — audited, dry-run-able seam for all forge writes`

---

### Task 3: `Suppress` pass — close re-drafts of human-rejected entries

The suppression *source* exists (`ClosedPRSuppression`, `internal/curate/suppression.go:55-83`) and Recurrence uses it to escalate. The missing consumer: when a permanently-benign incident recurs, the curator drafts a **fresh PR** (its dedup checks only open PRs and merged entries — `internal/curator/curator.go:110-133`), so humans re-close the same rejection forever. `Suppress` closes such re-drafts with a back-reference to the human's original close, honoring the label taxonomy already in place (`wontfix`/`not-kb-worthy` suppress; `needs-work` is revise-and-resubmit and does not — `suppression.go:41-47`).

**Files:**
- `internal/curate/suppression.go` — append the `Suppress` pass
- `internal/curate/suppression_test.go` — extend (reuses `fakeForge` from `dedup_test.go:16-38`, `fakeClosedPRs` + `body()` helper from `suppression_test.go:13-21`)

**Steps:**

- [ ] Write the failing tests (append to `internal/curate/suppression_test.go`):

```go
func TestSuppressClosesRedraftOfRejectedEntry(t *testing.T) {
	// A human closed PR #7 (fp-web) without merging; the incident recurred and the
	// curator re-drafted it as PR #12. The sweep must comment-then-close #12.
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 12, Title: "KB: apps/web DNS", Body: body("fp-web"), Labels: []string{"runlore"}},
		{Number: 13, Title: "KB: unrelated", Body: body("fp-other"), Labels: []string{"runlore"}},
	}}
	src := ClosedPRSuppression{Forge: fakeClosedPRs{prs: []providers.CuratedIssue{
		{Number: 7, Body: body("fp-web"), Labels: []string{"runlore", "wontfix"}},
	}}}
	s := Suppress{Forge: f, Source: src, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 1 || f.closed[0] != 12 {
		t.Fatalf("want close [12] only, got %v", f.closed)
	}
	if len(f.commented) != 1 || f.commented[0] != 12 {
		t.Fatalf("want a back-ref comment on 12 before closing, got %v", f.commented)
	}
}

func TestSuppressNeverTouchesProtectedOrMarkerless(t *testing.T) {
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 20, Body: body("fp-web"), Labels: []string{"runlore", "investigating"}}, // human-touched
		{Number: 21, Body: "no marker here", Labels: []string{"runlore"}},                // legacy/hand-filed
	}}
	src := ClosedPRSuppression{Forge: fakeClosedPRs{prs: []providers.CuratedIssue{
		{Number: 7, Body: body("fp-web"), Labels: []string{"runlore"}},
	}}}
	s := Suppress{Forge: f, Source: src, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 0 || len(f.commented) != 0 {
		t.Fatalf("protected/markerless PRs must be untouched: closed=%v commented=%v", f.closed, f.commented)
	}
}

func TestSuppressRespectsNeedsWorkAsRevise(t *testing.T) {
	// needs-work is accept-with-changes (suppression.go suppressReviseLabels): a
	// re-draft after such a close is the RESUBMIT — it must stay open.
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 30, Body: body("fp-y"), Labels: []string{"runlore"}},
	}}
	src := ClosedPRSuppression{Forge: fakeClosedPRs{prs: []providers.CuratedIssue{
		{Number: 8, Body: body("fp-y"), Labels: []string{"runlore", "needs-work"}},
	}}}
	s := Suppress{Forge: f, Source: src, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := s.Run(context.Background()); err != nil {
		t.Fatal(err)
	}
	if len(f.closed) != 0 {
		t.Fatalf("a needs-work resubmit must not be suppressed, got closed=%v", f.closed)
	}
}
```

  (Add `io`, `log/slog` to the test file's imports.)
- [ ] Run `go test ./internal/curate/ -run TestSuppress -v` — expect **FAIL** (`undefined: Suppress`)
- [ ] Implement — append to `internal/curate/suppression.go`:

```go
// Suppress closes open KB PRs that RE-DRAFT an entry a human already rejected
// (closed without merging). The file-time drafter's dedup checks only OPEN PRs and
// MERGED entries, so a recurring permanently-benign incident re-opens a fresh PR on
// every recurrence — the core of PR fatigue. Suppress honors the human "no": it
// closes the re-draft with a back-reference to the original close and its reason,
// and leaves the "reconsider" path to Recurrence's threshold escalation (which
// links, never reopens). Protected (human-touched) PRs are never closed, and the
// comment-first / don't-close-on-comment-failure ordering mirrors Lifecycle.
type Suppress struct {
	Forge  Forge
	Source SuppressionSource
	Log    *slog.Logger
}

// Run closes every unprotected open KB PR whose DupFingerprint a human previously
// rejected. Per-item forge failures are logged and skipped.
func (s Suppress) Run(ctx context.Context) error {
	prs, err := s.Forge.ListPRsByLabel(ctx, "runlore")
	if err != nil {
		return fmt.Errorf("suppress: list open KB PRs: %w", err)
	}
	// Fetch the (forge round-trip) suppression set lazily — only once an open,
	// unprotected, markered PR could match it. Mirrors Recurrence's lazy fetch.
	var suppressed map[string]SuppressedEntry
	for _, pr := range prs {
		fp := providers.ParseFingerprintMarker(pr.Body)
		if fp == "" || isProtected(pr.Labels) {
			continue
		}
		if suppressed == nil {
			if suppressed, err = s.Source.Suppressed(ctx); err != nil {
				return fmt.Errorf("suppress: load suppression set: %w", err)
			}
		}
		se, ok := suppressed[fp]
		if !ok {
			continue
		}
		if err := s.Forge.Comment(ctx, pr.Number, suppressComment(se)); err != nil {
			s.Log.Warn("suppress: comment failed; not closing", "pr", pr.Number, "err", err)
			continue
		}
		if err := s.Forge.Close(ctx, pr.Number); err != nil {
			s.Log.Warn("suppress: close failed", "pr", pr.Number, "err", err)
			continue
		}
		s.Log.Info("suppress: closed re-draft of a human-rejected entry",
			"pr", pr.Number, "prior_close", se.PRNumber, "reason", se.Reason)
	}
	return nil
}

// suppressComment explains the close and points at both the human decision and the
// reconsider path (Recurrence escalates past the threshold — never a reopen).
func suppressComment(se SuppressedEntry) string {
	reason := ""
	if se.Reason != "" {
		reason = fmt.Sprintf(" (%s)", se.Reason)
	}
	return fmt.Sprintf("Closed by RunLore curate: a human already rejected this entry in #%d%s. "+
		"If the incident keeps recurring, RunLore escalates via a knowledge-gap issue that links #%d — it never re-files PRs.",
		se.PRNumber, reason, se.PRNumber)
}
```

  (`isProtected` is `lifecycle.go:58`; `log/slog` needs adding to `suppression.go` imports.)
- [ ] Run `go test ./internal/curate/ -run TestSuppress -v` — expect **PASS**; then `go test ./internal/curate/` — **PASS**
- [ ] Commit: `feat(curate): Suppress pass — close re-drafts of human-rejected entries`

---

### Task 4: Shared `BuildCurateAgent` + CLI gains `--dry-run` and audit

Extract the pass assembly duplicated between the CLI and the upcoming sweeper into one builder, wire `Suppress` in, and give `lore curate` the same Guard seam (default **apply** — unchanged CLI behavior — with an opt-in `--dry-run`).

**Files:**
- `internal/app/curate.go` — refactor `RunCurate` (lines 29-114); add `BuildCurateAgent`
- `internal/app/curate_test.go` — add composition test

**Steps:**

- [ ] Write the failing test (append to `internal/app/curate_test.go`; `outcome`, `github`, `filepath` already imported — add `config`, `io`):

```go
func discardLogger() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

func TestBuildCurateAgentPassComposition(t *testing.T) {
	// Constructing a github.Client performs no I/O, so it is a safe stand-in.
	forge := github.New("https://forge.invalid", "o", "r", "main", nil)

	// No ledger: forge-only passes (Suppress, Dedup, Lifecycle).
	agent := BuildCurateAgent(&config.Config{}, forge, nil, discardLogger())
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
	agent = BuildCurateAgent(cfg, forge, ledger, discardLogger())
	if len(agent.Passes) != 7 {
		t.Fatalf("full agent: want 7 passes (suppress dedup lifecycle queue recurrence contested retirement), got %d", len(agent.Passes))
	}
}
```

- [ ] Run `go test ./internal/app/ -run TestBuildCurateAgent -v` — expect **FAIL** (`undefined: BuildCurateAgent`)
- [ ] Implement `BuildCurateAgent` in `internal/app/curate.go` and refactor `RunCurate` to use it. The builder replaces the wiring block at lines 59-103:

```go
// BuildCurateAgent assembles the grooming passes over a (typically Guard-wrapped)
// forge. ledger may be nil — the ledger-backed passes (Queue, Recurrence,
// Contested, Retirement) are then skipped. Shared by the one-shot `lore curate`
// CLI and the in-server sweeper so the two can never drift.
func BuildCurateAgent(cfg *config.Config, forge curate.GuardedForge, ledger *outcome.Ledger, log *slog.Logger) curate.Agent {
	agent := curate.Agent{Log: log, Passes: []curate.Pass{
		// Suppress runs FIRST: a re-draft of a rejected entry must not survive long
		// enough for Dedup to bless it as a cluster canonical.
		curate.Suppress{Forge: forge, Source: curate.ClosedPRSuppression{Forge: forge}, Log: log},
		curate.Dedup{Forge: forge, Log: log},
		curate.Lifecycle{Forge: forge, StaleAfter: cfg.Curate.StaleAfter.Std(), Log: log},
	}}
	if ledger == nil {
		return agent
	}
	agent.Passes = append(agent.Passes,
		curate.Queue{Forge: forge, Checker: curate.LedgerResolutionChecker{Ledger: ledger}, Log: log},
		curate.Recurrence{
			Forge:      forge,
			Ledger:     ledger,
			Threshold:  cfg.Curate.RecurrenceThreshold,
			Suppressed: curate.ClosedPRSuppression{Forge: forge},
			Log:        log,
		},
		curate.Contested{Forge: forge, Ledger: ledger, KBRepo: cfg.Forge.KBRepo, Log: log},
	)
	if cfg.Curate.Retirement.Enabled {
		agent.Passes = append(agent.Passes, curate.Retirement{
			Forge:           forge,
			Stats:           ledger,
			MinObservations: cfg.Curate.Retirement.MinObservations,
			Floor:           cfg.Curate.Retirement.Floor,
			Prior:           cfg.Curate.Retirement.Prior,
			Log:             log,
		})
	}
	return agent
}
```

  (Keep the existing explanatory comments from lines 63-93 attached to their passes — move, don't delete.)
- [ ] Refactor `RunCurate` to: add the flag `dry := fs.Bool("dry-run", false, "log and audit what the passes would do without writing to the forge")` (after line 31); after building `forge` (line 55), open the auditor and wrap:

```go
	// Same audit chain as the action executors (actions.audit_log_path); Nop when
	// unconfigured. Every forge write (or dry-run skip) below lands in it.
	aud, auditClose, aerr := BuildAuditor(cfg, log)
	if aerr != nil {
		return aerr
	}
	defer auditClose()
	guarded := curate.Guard{Inner: forge, DryRun: *dry, Audit: aud, Log: log}
```

  then open the ledger exactly as today (lines 64-68 → producing `ledger` or nil), keep both `LogLedgerStartup` calls (lines 107, 109), and end with:

```go
	log.Info("curate: grooming KB backlog", "repo", cfg.Forge.KBRepo, "dry_run", *dry)
	BuildCurateAgent(cfg, guarded, ledger, log).Run(context.Background())
	return nil
```

- [ ] Run `go test ./internal/app/ -run TestBuildCurateAgent -v` — expect **PASS**; then `go test ./internal/app/ ./internal/curate/` — **PASS**
- [ ] Update the `curate` usage line in `cmd/lore/main.go:35` to `lore curate [--config <path>] [--dry-run]                groom the KB backlog (dedup/stale/suppress…)`
- [ ] Commit: `refactor(app): shared BuildCurateAgent; lore curate gains --dry-run and audit`

---

### Task 5: `curate.Sweeper` — the interval scheduler

**Files:**
- `internal/curate/sweeper.go` — new
- `internal/curate/sweeper_test.go` — new

**Steps:**

- [ ] Write the failing test `internal/curate/sweeper_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// countPass counts Agent runs (each sweep runs every pass once).
type countPass struct{ n atomic.Int32 }

func (c *countPass) Run(context.Context) error { c.n.Add(1); return nil }

func TestSweeperRunsOnIntervalAndStopsOnCancel(t *testing.T) {
	p := &countPass{}
	s := Sweeper{Agent: Agent{Passes: []Pass{p}, Log: discardLog()}, Interval: 5 * time.Millisecond, Log: discardLog()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for p.n.Load() < 2 {
		select {
		case <-deadline:
			t.Fatalf("sweeper did not sweep twice in time, got %d", p.n.Load())
		case <-time.After(time.Millisecond):
		}
	}
	cancel()
	<-done // Run must return promptly on cancel
}

func TestSweeperNeverSweepsBeforeFirstInterval(t *testing.T) {
	// Leadership flaps re-enter startWork; an immediate first sweep would stampede
	// the forge listings on every flap. The first sweep waits one full interval.
	p := &countPass{}
	s := Sweeper{Agent: Agent{Passes: []Pass{p}, Log: discardLog()}, Interval: time.Hour, Log: discardLog()}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.Run(ctx); close(done) }()
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	if got := p.n.Load(); got != 0 {
		t.Fatalf("sweep before the first interval: got %d runs, want 0", got)
	}
}
```

  (`discardLog` comes from `guard_test.go` in the same package — Task 2.)
- [ ] Run `go test ./internal/curate/ -run TestSweeper -v` — expect **FAIL** (`undefined: Sweeper`)
- [ ] Implement `internal/curate/sweeper.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"log/slog"
	"time"
)

// DefaultSweepInterval is the fallback cadence when no interval is configured.
// config.applyDefaults ships the same value; this guard only protects direct users.
const DefaultSweepInterval = 6 * time.Hour

// Sweeper runs the grooming Agent on a fixed interval until ctx is done — the
// in-server counterpart of the `lore curate` CronJob. The first sweep happens one
// FULL interval after start, never immediately: it is launched on every leadership
// (re-)acquisition, and a flapping leader must not stampede the forge with
// back-to-back listing storms. All passes are idempotent, so an occasional
// double-run across a flap is safe, just wasteful.
type Sweeper struct {
	Agent    Agent
	Interval time.Duration // <= 0 ⇒ DefaultSweepInterval
	Log      *slog.Logger
}

// Run sweeps every Interval until ctx is cancelled.
func (s Sweeper) Run(ctx context.Context) {
	iv := s.Interval
	if iv <= 0 {
		iv = DefaultSweepInterval
	}
	t := time.NewTicker(iv)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.Log.Debug("curate sweep starting")
		s.Agent.Run(ctx)
	}
}
```

  (Ticker-loop precedent: `Reinvestigator.Poll`, `internal/investigate/reinvestigate.go:34-48` — deliberately inverted here so the first run waits.)
- [ ] Run `go test ./internal/curate/ -run TestSweeper -v` — expect **PASS**; `go test -race ./internal/curate/` — **PASS**
- [ ] Commit: `feat(curate): Sweeper — flap-proof interval scheduler for grooming passes`

---

### Task 6: `BuildSweeper` + leader-only serve wiring

**Files:**
- `internal/app/sweep.go` — new
- `internal/app/sweep_test.go` — new
- `internal/app/serve.go` — start the sweeper inside `startWork` (insert after the Matrix feedback block, line 317-322)

**Steps:**

- [ ] Write the failing test `internal/app/sweep_test.go` (RSA test-key generation style: `internal/forge/github/auth_test.go:22-25`):

```go
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"testing"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
)

// forgeConfigured returns a Config with working GitHub App credentials (a real
// throwaway key in an env var), the minimum BuildForgeTokenSource accepts.
func forgeConfigured(t *testing.T) *config.Config {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pemStr := string(pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)}))
	t.Setenv("TEST_SWEEP_GH_KEY", pemStr)
	cfg := &config.Config{}
	cfg.Forge.KBRepo = "acme/kb"
	cfg.Forge.GitHubApp = config.GitHubApp{AppID: 1, InstallationID: 2, PrivateKeyEnv: "TEST_SWEEP_GH_KEY"}
	return cfg
}

func TestBuildSweeperNilWhenOff(t *testing.T) {
	cfg := forgeConfigured(t)
	cfg.Curate.Sweeps.Mode = config.SweepOff
	if sw := BuildSweeper(cfg, nil, audit.Nop{}, discardLogger()); sw != nil {
		t.Fatal("mode: off must not build a sweeper")
	}
}

func TestBuildSweeperNilWithoutForge(t *testing.T) {
	// No GitHub App / kb_repo: sweeps silently stay off — no new required config.
	if sw := BuildSweeper(&config.Config{}, nil, audit.Nop{}, discardLogger()); sw != nil {
		t.Fatal("unconfigured forge must not build a sweeper")
	}
}

func TestBuildSweeperDefaultsToDryRunAgent(t *testing.T) {
	cfg := forgeConfigured(t) // Mode unset ⇒ dry-run
	sw := BuildSweeper(cfg, nil, audit.Nop{}, discardLogger())
	if sw == nil {
		t.Fatal("configured forge + default mode must build a sweeper")
	}
	if len(sw.Agent.Passes) != 3 {
		t.Fatalf("nil ledger sweeper: want 3 forge-only passes, got %d", len(sw.Agent.Passes))
	}
}
```

- [ ] Run `go test ./internal/app/ -run TestBuildSweeper -v` — expect **FAIL** (`undefined: BuildSweeper`)
- [ ] Implement `internal/app/sweep.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"
	"strings"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curate"
	"github.com/Smana/runlore/internal/outcome"

	github "github.com/Smana/runlore/internal/forge/github"
)

// BuildSweeper assembles the in-server grooming sweeper, or nil when sweeps are
// off (curate.sweeps.mode: off) or the KB forge is not configured. No new required
// config: with forge credentials already present, sweeps default to DRY-RUN — the
// operator flips curate.sweeps.mode: apply to let them act.
func BuildSweeper(cfg *config.Config, ledger *outcome.Ledger, aud audit.Auditor, log *slog.Logger) *curate.Sweeper {
	if !cfg.Curate.Sweeps.Enabled() {
		return nil
	}
	tok := BuildForgeTokenSource(cfg, log)
	if tok == nil || cfg.Forge.KBRepo == "" {
		return nil // no forge, nothing to groom — sweeps are strictly additive
	}
	owner, repo, ok := strings.Cut(cfg.Forge.KBRepo, "/")
	if !ok {
		log.Warn("curate sweeps disabled: forge.kb_repo must be owner/name", "kb_repo", cfg.Forge.KBRepo)
		return nil
	}
	base := cfg.Forge.BaseBranch
	if base == "" {
		base = "main"
	}
	client := github.New(cfg.Forge.GitHubAPIURL, owner, repo, base, github.TokenFunc(tok))
	guarded := curate.Guard{Inner: client, DryRun: cfg.Curate.Sweeps.DryRun(), Audit: aud, Log: log}
	// The LIVE serve ledger: the ledger-backed passes see the same episodes the
	// investigation loop records — no shared-volume mount to misconfigure (the
	// CronJob's classic footgun; see LogLedgerStartup in curate.go).
	var l *outcome.Ledger
	if ledger != nil && ledger.Enabled() {
		l = ledger
	}
	return &curate.Sweeper{
		Agent:    BuildCurateAgent(cfg, guarded, l, log),
		Interval: cfg.Curate.Sweeps.Interval.Std(),
		Log:      log,
	}
}
```

- [ ] Run `go test ./internal/app/ -run TestBuildSweeper -v` — expect **PASS**
- [ ] Wire into `RunServe`: in `internal/app/serve.go`, inside `startWork` (after the Matrix feedback block ending line 322), insert:

```go
		// In-server grooming sweeps (leader-only, like the pollers above, so one
		// replica grooms): the same passes as `lore curate`, on a timer, over the
		// live ledger. Cancelled with workCtx on leadership loss.
		if sw := BuildSweeper(cfg, ledger, aud, log); sw != nil {
			mode := config.SweepApply
			if cfg.Curate.Sweeps.DryRun() {
				mode = config.SweepDryRun
				log.Info("curate sweeps in dry-run: candidates are logged (and audited when actions.audit_log_path is set) " +
					"but nothing is written to the forge — set curate.sweeps.mode: apply to act, mode: off to silence")
			}
			log.Info("curate sweeps enabled", "mode", mode, "interval", cfg.Curate.Sweeps.Interval.Std())
			go sw.Run(workCtx)
		}
```

  (`aud` is built at line 121 and `ledger` at line 174, both before `startWork` is defined at line 290, so the closure captures them; `config` is already imported.)
- [ ] Build + full app tests: `go build ./... && go test ./internal/app/` — expect **PASS**
- [ ] Commit: `feat(app): in-server leader-only curate sweeps, dry-run by default`

---### Task 7: Helm chart — document `curate.sweeps` in values.yaml

No template changes: `config.curate.*` flows through the ConfigMap as-is. This is documentation of the new keys plus repositioning the CronJob as the alternative.

**Files:**
- `deploy/helm/runlore/values.yaml` — extend the `config.curate` block (lines 177-187) and the top-level `curate.cronjob` comment (lines 494-500)

**Steps:**

- [ ] In the `config:` section, replace the comment + block at lines 177-187 with:

```yaml
  # Phase-2 backlog groomer. `stale_after` drives the stale-close sweep; the Queue
  # (promote solved→ready-to-merge on resolution) and Recurrence (open a
  # knowledge-gap issue for repeatedly-unresolved patterns) passes also run when
  # config.outcome.ledger_path is set (they read the outcome ledger).
  curate:
    stale_after: 720h          # close unprotected KB PRs idle longer than this; 0 disables
    recurrence_threshold: 3    # open a knowledge-gap issue after this many unresolved occurrences; 0 ⇒ 3.
                               # Also the trigger for escalating a closed-unmerged (human-rejected) KB
                               # entry that keeps recurring: RunLore links the closed PR in a knowledge-gap
                               # issue rather than reopening it.
    # In-server scheduled sweeps (leader-only): the serve pod runs the grooming
    # passes on a timer whenever the KB forge (forge.kb_repo + forge.github_app) is
    # configured — no CronJob or shared ledger volume needed, and the ledger-backed
    # passes read the pod's LIVE outcome ledger. Default mode is DRY-RUN: every
    # candidate action is logged (and recorded in the actions.audit_log_path hash
    # chain when set) but nothing is written to the forge until you set mode: apply.
    sweeps:
      mode: dry-run   # dry-run (default) | apply | "off" (quote off — YAML)
      interval: 6h    # first sweep runs one full interval after startup; min 10m
```

- [ ] Update the top-level CronJob comment (line 494-496) to:

```yaml
# Phase-2 backlog groomer (lore curate) as a scheduled Job — the OUT-OF-SERVER
# alternative to config.curate.sweeps (which the serve pod runs itself, leader-only,
# dry-run by default). Prefer the in-server sweeps: the CronJob needs the outcome
# ledger on a shared volume to run its ledger-backed passes. Both paths are
# idempotent, so enabling both is safe, just redundant.
curate:
  cronjob:
    enabled: false
    schedule: "0 * * * *"   # hourly
```

- [ ] Verify the chart still renders and the ConfigMap carries the new keys:
  `helm lint deploy/helm/runlore && helm template deploy/helm/runlore | yq 'select(.kind == "ConfigMap") | .data' | grep -A2 'sweeps'`
- [ ] Run the shipped-config guard tests: `go test ./internal/config/ -run 'Shipped|Minimal'` — expect **PASS** (`shipped_configs_test.go` parses the chart's config block; `mode: dry-run` + `interval: 6h` must validate)
- [ ] Commit: `chore(chart): document curate.sweeps (in-server grooming, dry-run default)`

---

### Task 8: Docs — learning-loop, reviewing-knowledge, configuration

**Files:**
- `docs/learning-loop.md` — §5 Phase-2 intro (line 360-361) + new Suppress bullet (after the Lifecycle bullet, ~line 370)
- `docs/reviewing-knowledge.md` — §8 "the backlog groomer" (lines 233-240)
- `docs/configuration.md` — `### curate` section (lines 446-465)

**Steps:**

- [ ] `docs/learning-loop.md`: replace the Phase-2 intro line (line 360-361) with:

```markdown
**Phase-2 grooming** (`internal/curate/`) keeps the backlog healthy on a schedule. It
runs **inside the serve pod** (leader-only, every `curate.sweeps.interval`, default 6 h)
whenever the KB forge is configured — **in dry-run by default**: candidates are logged and
recorded in the action audit chain (`actions.audit_log_path`), and nothing touches the
forge until you set `curate.sweeps.mode: apply`. The opt-in `lore curate` CronJob remains
the out-of-server alternative (same passes, shared wiring — `--dry-run` there too).
```

- [ ] `docs/learning-loop.md`: after the **Lifecycle** bullet, add:

```markdown
- **Suppress** — close a PR that *re-drafts* an entry a human already rejected (closed
  without merging). The drafter's dedup only checks open PRs and merged entries, so a
  recurring permanently-benign incident would re-open a fresh PR forever. The close
  carries a back-reference to the original human decision (and its `wontfix` /
  `not-kb-worthy` label when present); a `needs-work` close is a revise-and-resubmit and
  is never suppressed. Reconsideration stays with Recurrence's knowledge-gap escalation —
  suppression never argues with a human, it just stops re-asking.
```

- [ ] `docs/reviewing-knowledge.md`: replace the first paragraph of §8 (lines 235-240) with:

```markdown
The backlog groomer keeps the PR queue tidy without you: it **suppresses** re-drafts of
entries you already rejected, **dedups** near-identical PRs, **closes stale** unreviewed
ones after a configurable age, **promotes** `solved`→`ready-to-merge` when the incident
resolved, and opens a **`knowledge-gap`** issue when an unsolved pattern recurs. It only
ever comments/labels/closes — it never merges.

It runs automatically inside the serve pod (leader-only, every 6 h by default) — **in
dry-run first**: check the logs for `curate dry-run: skipped forge write` lines (and the
audit log for `"actor":"curate"` records) to see what it *would* do, then set
`curate.sweeps.mode: apply` to let it act, or `mode: "off"` to silence it. Every
automated action, applied or skipped, lands in the same tamper-evident audit chain as
cluster actions when `actions.audit_log_path` is set. A `lore curate` CronJob remains
available for out-of-server runs. See
[getting-started](getting-started.md#step-7--the-learn-loop-kb-lifecycle--re-runs).
```

- [ ] `docs/configuration.md`: append to the `### curate` section (after the `retirement` keys, line 465):

```markdown
- `sweeps` — the **in-server** scheduled grooming loop (leader-only; the serve pod runs the same
  passes as `lore curate` on a timer, over its live outcome ledger). Strictly additive: it only
  starts when the KB forge (`forge.kb_repo` + `forge.github_app`) is configured. Keys:
  - `mode` — `dry-run` (**default**, also when empty): log + audit every candidate action, write
    nothing to the forge; `apply`: act; `off` (quote it in YAML): disable the loop. Unknown values
    fail validation — a typo must not silently demote grooming to dry-run.
  - `interval` — sweep cadence; **default `6h`**, must be `>= 10m`. The first sweep runs one full
    interval after startup (leadership flaps never trigger immediate re-sweeps).

  Every write (or dry-run skip) is appended to the `actions.audit_log_path` hash chain as
  `actor: curate` with `op` `kb.close` / `kb.comment` / `kb.relabel` / `kb.open-issue` /
  `kb.retire-pr` and decision `executed` / `dry-run` / `failed`.
```

- [ ] Full verification: `go build ./... && go test ./... && gofmt -l internal/ | (! grep .)` — expect **PASS**, no gofmt diffs
- [ ] Commit: `docs: curator automation phase 2 — in-server sweeps, suppression, dry-run posture`

---

## Acceptance criteria

- [ ] `go test ./...` passes and `go vet ./...` is clean on the branch
- [ ] With `forge.kb_repo` + `forge.github_app` configured and **no other config change**, `lore serve` logs `curate sweeps enabled mode=dry-run interval=6h0m0s` on the leader, and the first sweep happens only after one full interval
- [ ] In dry-run, a sweep against a backlog with a stale PR produces `curate dry-run: skipped forge write op=kb.close …` log lines and `{"actor":"curate","decision":"dry-run"}` audit records (when `actions.audit_log_path` is set) — and **zero forge mutations**
- [ ] With `curate.sweeps.mode: apply`, the same actions execute and are audited as `executed`; failures audit as `failed` and never abort the sweep (per-item isolation preserved)
- [ ] `curate.sweeps.mode: "off"` disables the loop; `mode: apply` (typo) fails config validation; `interval: 1m` fails validation
- [ ] A re-drafted PR whose `DupFingerprint` matches a human close labelled `wontfix`/`not-kb-worthy` (or unlabelled) is commented + closed by `Suppress`; `needs-work` closes and protected labels (`solved`, `ready-to-merge`, `accepted`, `investigating`, `knowledge-gap`) are never touched
- [ ] Only the leader sweeps: the sweeper goroutine lives inside `startWork` and stops when `workCtx` is cancelled (leadership loss / shutdown)
- [ ] `lore curate --dry-run` performs zero forge writes and reports what it would do; plain `lore curate` behavior is unchanged (apply)
- [ ] No new **required** config: a chart upgrade with existing values renders (`helm lint` + `shipped_configs` test pass) and changes nothing except dry-run log lines
- [ ] Retirement remains opt-in (`curate.retirement.enabled`, default off) and, when enabled, its retire-PR opens are audited through the same Guard
- [ ] `docs/learning-loop.md`, `docs/reviewing-knowledge.md`, `docs/configuration.md`, and `deploy/helm/runlore/values.yaml` document the sweeps, the dry-run-first posture, and the Suppress pass
