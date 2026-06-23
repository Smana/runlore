# Phase-2 curation: scheduler + lifecycle sweep Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make KB curation compound on a schedule and sweep stale PRs: surface GitHub's `updated_at` on the curated view, wire the Lifecycle pass on it, and add an opt-in `lore curate` CronJob to the chart. (Queue + Recurrence remain a documented follow-up.)

**Architecture:** `CuratedIssue` gains `UpdatedAt`; `Lifecycle` computes staleness intrinsically from it against a configured window; `runCurate` runs Dedup + Lifecycle; a new opt-in CronJob template runs `lore curate` on a schedule with the same image/config/ledger mounts as the Deployment.

**Tech Stack:** Go 1.26, standard library; Helm/Kubernetes CronJob (batch/v1).

## Global Constraints

- Go 1.26, no new dependencies.
- The CronJob is **opt-in** (`curate.cronjob.enabled`, default `false`) — zero behavior change for existing installs.
- The staleness window is app config at `config.curate.stale_after` (a `config.Duration`); `StaleAfter <= 0` disables the lifecycle sweep. A PR with an unknown (zero) `UpdatedAt` is never closed.
- Out of scope: Queue (`ResolutionChecker`) and Recurrence wiring — leave those passes in `internal/curate` untouched and update `runCurate`'s comment to state their remaining blockers.
- After each task: `go build ./... && go vet ./... && go test ./...` green and `gofmt -l .` empty.

---

### Task 1: `UpdatedAt` on the curated view

**Files:**
- Modify: `internal/providers/providers.go`
- Modify: `internal/forge/github/github.go`
- Test: `internal/forge/github/github_test.go`

**Interfaces:**
- Produces: `CuratedIssue.UpdatedAt time.Time`; the GitHub client populates it from `updated_at`.

- [ ] **Step 1: Write the failing test**

In `internal/forge/github/github_test.go`, add a test that the listing parses `updated_at`. Mirror the existing `TestListPRsByLabel` (it stands up an `httptest` server returning a JSON issue array):

```go
func TestListPRsByLabelParsesUpdatedAt(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`[
		  {"number":48,"title":"KB: X","body":"b","labels":[{"name":"runlore"}],"pull_request":{"url":"x"},"updated_at":"2026-06-01T12:00:00Z"}
		]`))
	}))
	defer srv.Close()
	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	prs, err := c.ListPRsByLabel(context.Background(), "runlore")
	if err != nil {
		t.Fatalf("ListPRsByLabel: %v", err)
	}
	if len(prs) != 1 || prs[0].UpdatedAt.IsZero() {
		t.Fatalf("updated_at not parsed: %+v", prs)
	}
	if got := prs[0].UpdatedAt.UTC().Format(time.RFC3339); got != "2026-06-01T12:00:00Z" {
		t.Fatalf("unexpected updated_at: %s", got)
	}
}
```

Ensure the test file imports `"time"` (add if missing).

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/forge/github/ -run TestListPRsByLabelParsesUpdatedAt -v`
Expected: FAIL — `CuratedIssue.UpdatedAt` undefined.

- [ ] **Step 3: Implement**

In `internal/providers/providers.go`, add to `CuratedIssue` (after `Labels`):
```go
	UpdatedAt time.Time // forge last-update time; used by the curate lifecycle sweep
```
Confirm `providers.go` imports `"time"` (it does — other types use `time.Time`).

In `internal/forge/github/github.go`, add to `rawIssue`:
```go
	UpdatedAt time.Time `json:"updated_at"`
```
and in `curated()` set it:
```go
	return providers.CuratedIssue{Number: ri.Number, Title: ri.Title, Body: ri.Body, Labels: labels, UpdatedAt: ri.UpdatedAt}
```
Confirm `github.go` imports `"time"` (add it to the import block if not already present).

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/forge/github/ -run TestListPRsByLabelParsesUpdatedAt -v`
Expected: PASS.

- [ ] **Step 5: Build + full forge/providers tests + gofmt**

Run: `go build ./... && go test ./internal/forge/github/ ./internal/providers/ && gofmt -l internal/forge/github/ internal/providers/`
Expected: PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/providers/providers.go internal/forge/github/github.go internal/forge/github/github_test.go
git commit -m "feat(forge): surface updated_at on CuratedIssue for the curate lifecycle sweep"
```

---

### Task 2: Lifecycle computes staleness from `UpdatedAt`

**Files:**
- Modify: `internal/curate/lifecycle.go`
- Test: `internal/curate/lifecycle_test.go`

**Interfaces:**
- Produces: `Lifecycle{ Forge; StaleAfter time.Duration; Now func() time.Time; Log }`. Closes an unprotected PR only when `StaleAfter > 0`, `pr.UpdatedAt` is non-zero, and `now.Sub(pr.UpdatedAt) > StaleAfter`.

- [ ] **Step 1: Rewrite the tests**

Replace the contents of `internal/curate/lifecycle_test.go`'s stale test with age-based cases (read the file first to reuse its `fakeForge`/logger style; `fakeForge` from `dedup_test.go` records `closed`/`commented`):

```go
func lifecycleNow() time.Time { return time.Date(2026, 6, 23, 0, 0, 0, 0, time.UTC) }

func TestLifecycleClosesOnlyAgedUnprotected(t *testing.T) {
	now := lifecycleNow()
	f := &fakeForge{prs: []providers.CuratedIssue{
		{Number: 1, Labels: []string{"runlore"}, UpdatedAt: now.Add(-40 * 24 * time.Hour)},  // aged → close
		{Number: 2, Labels: []string{"runlore"}, UpdatedAt: now.Add(-2 * time.Hour)},          // fresh → keep
		{Number: 3, Labels: []string{"runlore", "accepted"}, UpdatedAt: now.Add(-40 * 24 * time.Hour)}, // aged but protected → keep
	}}
	l := Lifecycle{Forge: f, StaleAfter: 30 * 24 * time.Hour, Now: func() time.Time { return now }, Log: testLogger()}
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.closed) != 1 || f.closed[0] != 1 {
		t.Fatalf("only the aged unprotected PR #1 should close, got %v", f.closed)
	}
}

func TestLifecycleZeroStaleAfterDisables(t *testing.T) {
	now := lifecycleNow()
	f := &fakeForge{prs: []providers.CuratedIssue{{Number: 1, Labels: []string{"runlore"}, UpdatedAt: now.Add(-365 * 24 * time.Hour)}}}
	l := Lifecycle{Forge: f, StaleAfter: 0, Now: func() time.Time { return now }, Log: testLogger()}
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.closed) != 0 {
		t.Fatalf("StaleAfter==0 must close nothing, got %v", f.closed)
	}
}

func TestLifecycleUnknownAgeNotClosed(t *testing.T) {
	now := lifecycleNow()
	f := &fakeForge{prs: []providers.CuratedIssue{{Number: 1, Labels: []string{"runlore"}}}} // zero UpdatedAt
	l := Lifecycle{Forge: f, StaleAfter: 24 * time.Hour, Now: func() time.Time { return now }, Log: testLogger()}
	if err := l.Run(context.Background()); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(f.closed) != 0 {
		t.Fatalf("a PR with unknown age must never close, got %v", f.closed)
	}
}
```

Use the same logger helper the file already uses (likely an inline `slog.New(slog.NewTextHandler(io.Discard, nil))` — if there's no `testLogger()` in the package tests, define one or inline it). Match the existing `fakeForge` field names (`closed`).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/curate/ -run TestLifecycle -v`
Expected: FAIL — `Lifecycle` has no `StaleAfter`/`Now` fields (compile error).

- [ ] **Step 3: Rewrite `Lifecycle`**

Replace the `Lifecycle` struct + `Run` in `internal/curate/lifecycle.go` (keep `protectedLabels`/`isProtected` and the comment-before-close safety):

```go
// Lifecycle closes stale, unprotected KB artifacts — those with no forge activity
// within StaleAfter. A PR whose age is unknown (zero UpdatedAt) is never closed.
type Lifecycle struct {
	Forge      Forge
	StaleAfter time.Duration    // 0 disables the sweep
	Now        func() time.Time // injectable clock; nil ⇒ time.Now
	Log        *slog.Logger
}

// Run closes stale, unprotected artifacts with a comment.
func (l Lifecycle) Run(ctx context.Context) error {
	if l.StaleAfter <= 0 {
		return nil
	}
	now := time.Now
	if l.Now != nil {
		now = l.Now
	}
	prs, err := l.Forge.ListPRsByLabel(ctx, "runlore")
	if err != nil {
		return err
	}
	for _, pr := range prs {
		if isProtected(pr.Labels) || pr.UpdatedAt.IsZero() || now().Sub(pr.UpdatedAt) <= l.StaleAfter {
			continue
		}
		// Comment first; if the back-ref comment fails, do NOT close (preserve the
		// "why" for whoever reopens it) — mirrors Dedup.
		if err := l.Forge.Comment(ctx, pr.Number, "Closed as stale by RunLore curate (no progress in the staleness window). Reopen if still relevant."); err != nil {
			l.Log.Warn("stale: comment failed; not closing", "pr", pr.Number, "err", err)
			continue
		}
		if err := l.Forge.Close(ctx, pr.Number); err != nil {
			l.Log.Warn("stale close failed", "pr", pr.Number, "err", err)
			continue
		}
		l.Log.Info("closed stale artifact", "pr", pr.Number)
	}
	return nil
}
```

Ensure `lifecycle.go` imports `"time"`.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/curate/ -run TestLifecycle -v`
Expected: PASS.

- [ ] **Step 5: Full curate package + gofmt**

Run: `go test ./internal/curate/ && gofmt -l internal/curate/`
Expected: PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/curate/lifecycle.go internal/curate/lifecycle_test.go
git commit -m "feat(curate): lifecycle closes stale PRs by UpdatedAt age"
```

---

### Task 3: Config knob + wire Dedup+Lifecycle in `runCurate`

**Files:**
- Modify: `internal/config/config.go`
- Modify: `cmd/lore/main.go`

**Interfaces:**
- Consumes: `Lifecycle` (Task 2), `Duration.Std()`.
- Produces: `config.Config.Curate config.Curate` with `StaleAfter Duration`.

- [ ] **Step 1: Add the config field**

In `internal/config/config.go`, add a `Curate` field to the top-level `Config` struct (alongside `Model`, `Cloud`, etc.):
```go
	Curate Curate `yaml:"curate"`
```
and the type (near the `Forge` type):
```go
// Curate configures the Phase-2 backlog groomer (lore curate).
type Curate struct {
	StaleAfter Duration `yaml:"stale_after"` // close unprotected KB PRs idle longer than this; 0 disables (default 720h)
}
```

- [ ] **Step 2: Wire the passes in `runCurate`**

In `cmd/lore/main.go`'s `runCurate`, replace the agent construction (currently only `curate.Dedup`) and its preceding admitting comment. The new comment + construction:

```go
	// runCurate grooms the KB backlog (Phase-2 curation agent). It runs the
	// backlog-dedup pass (collapses duplicate open PRs across history) and the
	// lifecycle sweep (closes stale, unprotected PRs by forge age). The
	// resolution-gated decision-ready queue and the recurrence→gap-issue pass are
	// implemented + tested in internal/curate but still need wiring: Queue needs a
	// ResolutionChecker (alert/ledger join), Recurrence needs an idempotent
	// ledger-backed driver over Episodes() — follow-up.
```

(Move/replace the existing comment block above `runCurate` accordingly.)

Inside the function, after building `forge`, set a default staleness window and build the passes:
```go
	staleAfter := cfg.Curate.StaleAfter.Std()
	if staleAfter == 0 {
		staleAfter = 720 * time.Hour // 30 days
	}
	agent := curate.Agent{Log: log, Passes: []curate.Pass{
		curate.Dedup{Forge: forge, Log: log},
		curate.Lifecycle{Forge: forge, StaleAfter: staleAfter, Log: log},
	}}
```

Note: a default of 720h means the sweep is ON by default in `lore curate`; that is intended (the CronJob is still opt-in at the chart level). If you prefer the sweep strictly opt-in, the spec's `StaleAfter <= 0 disables` already supports it — but the default here is 720h. Keep 720h. Confirm `cmd/lore/main.go` imports `"time"` (it does).

- [ ] **Step 3: Build + vet + full suite + gofmt**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l .`
Expected: all PASS; gofmt prints nothing.

- [ ] **Step 4: Manual smoke — config parses**

Run: `go vet ./... && printf 'forge:\n  kb_repo: o/r\ncurate:\n  stale_after: 240h\n' > /tmp/curate-smoke.yaml && go run ./cmd/lore curate -config /tmp/curate-smoke.yaml 2>&1 | head -3 || true`
Expected: it parses the config and then fails on the missing GitHub App (`curate requires a configured GitHub App`) — NOT a yaml/flag/`stale_after` parse error.

- [ ] **Step 5: Commit**

```bash
git add internal/config/config.go cmd/lore/main.go
git commit -m "feat(curate): wire Dedup+Lifecycle in lore curate; add curate.stale_after"
```

---

### Task 4: Opt-in `lore curate` CronJob in the chart

**Files:**
- Create: `deploy/helm/runlore/templates/cronjob.yaml`
- Modify: `deploy/helm/runlore/values.yaml`

- [ ] **Step 1: Add values**

Read `deploy/helm/runlore/values.yaml` and `deploy/helm/runlore/templates/deployment.yaml` first to mirror image, `podSecurityContext`/`securityContext`, `env`/`envFrom`, `serviceAccountName`, and the `config`/`catalog`/`tmp` volume wiring.

Add a top-level deploy block to `values.yaml`:
```yaml
# Phase-2 backlog groomer (lore curate) as a scheduled Job. Opt-in: writes to the
# KB forge (closes duplicate/stale PRs), so it stays disabled until you set the
# GitHub App credentials and enable it.
curate:
  cronjob:
    enabled: false
    schedule: "0 * * * *"   # hourly
```

Add the staleness window under the existing `config:` block (rendered into the ConfigMap verbatim):
```yaml
config:
  # ... existing keys ...
  curate:
    stale_after: 720h   # close unprotected KB PRs idle longer than this; 0 disables
```

(Place `config.curate.stale_after` so it nests under `config:` — it flows into `runlore.yaml` automatically.)

- [ ] **Step 2: Create the CronJob template**

`deploy/helm/runlore/templates/cronjob.yaml` — mirror the Deployment's pod spec but run `curate`. Use the chart's existing helpers (`runlore.fullname`, `runlore.labels`, `runlore.selectorLabels`, `runlore.serviceAccountName` if present — check `_helpers.tpl`):

```yaml
{{- if .Values.curate.cronjob.enabled }}
apiVersion: batch/v1
kind: CronJob
metadata:
  name: {{ include "runlore.fullname" . }}-curate
  labels:
    {{- include "runlore.labels" . | nindent 4 }}
spec:
  schedule: {{ .Values.curate.cronjob.schedule | quote }}
  concurrencyPolicy: Forbid
  successfulJobsHistoryLimit: 3
  failedJobsHistoryLimit: 3
  jobTemplate:
    spec:
      backoffLimit: 2
      template:
        metadata:
          labels:
            {{- include "runlore.selectorLabels" . | nindent 12 }}
        spec:
          restartPolicy: Never
          serviceAccountName: {{ include "runlore.serviceAccountName" . }}
          securityContext:
            {{- toYaml .Values.podSecurityContext | nindent 12 }}
          containers:
            - name: curate
              image: "{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}"
              imagePullPolicy: {{ .Values.image.pullPolicy }}
              args:
                - curate
                - --config
                - /etc/runlore/runlore.yaml
              {{- with .Values.envFrom }}
              envFrom:
                {{- toYaml . | nindent 16 }}
              {{- end }}
              {{- with .Values.env }}
              env:
                {{- toYaml . | nindent 16 }}
              {{- end }}
              securityContext:
                {{- toYaml .Values.securityContext | nindent 16 }}
              volumeMounts:
                - name: config
                  mountPath: /etc/runlore
                  readOnly: true
                - name: tmp
                  mountPath: /tmp
                {{- if .Values.catalog.configMap }}
                - name: catalog
                  mountPath: {{ .Values.catalog.mountPath }}
                  readOnly: true
                {{- else if or .Values.catalog.gitSync .Values.persistence.enabled }}
                - name: catalog
                  mountPath: {{ .Values.catalog.mountPath }}
                {{- end }}
          volumes:
            - name: config
              configMap:
                name: {{ include "runlore.fullname" . }}-config
            - name: tmp
              emptyDir: {}
            {{- if .Values.catalog.configMap }}
            - name: catalog
              configMap:
                name: {{ .Values.catalog.configMap }}
            {{- else if or .Values.catalog.gitSync .Values.persistence.enabled }}
            - name: catalog
              {{- if .Values.persistence.enabled }}
              persistentVolumeClaim:
                claimName: {{ .Values.persistence.existingClaim | default (printf "%s-data" (include "runlore.fullname" .)) }}
              {{- else }}
              emptyDir: {}
              {{- end }}
            {{- end }}
{{- end }}
```

IMPORTANT: verify the exact helper names against `deploy/helm/runlore/templates/_helpers.tpl` and the Deployment (`serviceAccountName` rendering, `podSecurityContext` vs `securityContext` values keys). Match the Deployment's volume block EXACTLY (copy it) so the curate Job sees the same config + catalog/ledger.

- [ ] **Step 3: Render smoke — disabled by default, present when enabled**

Run: `helm template t deploy/helm/runlore | grep -c "kind: CronJob" || true`
Expected: `0` (disabled by default).

Run: `helm template t deploy/helm/runlore --set curate.cronjob.enabled=true | grep -A2 "kind: CronJob"`
Expected: renders a `CronJob` named `t-runlore-curate`.

Run: `helm template t deploy/helm/runlore --set curate.cronjob.enabled=true | python3 -c "import sys,yaml; list(yaml.safe_load_all(sys.stdin)); print('helm yaml ok')"`
Expected: `helm yaml ok` (all rendered docs are valid YAML).

Also confirm the curate args render: `helm template t deploy/helm/runlore --set curate.cronjob.enabled=true | grep -A6 "name: curate" | grep -q curate && echo args-ok`.

- [ ] **Step 4: Commit**

```bash
git add deploy/helm/runlore/templates/cronjob.yaml deploy/helm/runlore/values.yaml
git commit -m "feat(chart): opt-in lore curate CronJob (scheduled backlog groom)"
```

---

## Self-Review

**Spec coverage:**
- `UpdatedAt` on CuratedIssue + GitHub parse → Task 1. ✅
- Lifecycle intrinsic staleness (StaleAfter/Now, unknown-age guard) → Task 2. ✅
- `config.curate.stale_after` + runCurate wires Dedup+Lifecycle + updated comment → Task 3. ✅
- Opt-in CronJob + values → Task 4. ✅
- Queue/Recurrence explicitly deferred (passes untouched; comment states blockers) → Tasks 3-4 leave them alone. ✅

**Placeholder scan:** all Go code is complete; the CronJob template is complete but flagged to verify helper names against `_helpers.tpl`/Deployment (real templating dependency, not a placeholder). ✅

**Type consistency:** `CuratedIssue.UpdatedAt` (Task 1) consumed by `Lifecycle` (Task 2); `config.Curate.StaleAfter` (Task 3) → `.Std()` → `Lifecycle.StaleAfter`; `curate.Lifecycle`/`curate.Dedup`/`curate.Agent`/`curate.Pass` are existing types. ✅
