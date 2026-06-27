# Helm probe timing — startupProbe + configurable probes (R21)

Date: 2026-06-24
Status: design
Scope: `deploy/helm/runlore/templates/deployment.yaml`, `deploy/helm/runlore/values.yaml`

## Finding (R21)

> The readiness probe's `initialDelaySeconds` is tight (~2s) given catalog-index
> warmup + leader-lease gating, risking probe flapping during startup / leader
> handoff.

## Challenge — what the code actually does

Readiness (`/readyz`) is computed by `readyFunc(leader.Load, cat)` in
`cmd/lore/main.go`:

```go
func readyFunc(leader func() bool, cat *catalog.Catalog) func() bool {
    return func() bool {
        if cat != nil && !cat.Ready() { return false }
        return leader()
    }
}
```

- `leader()` — true only once this replica holds the leader Lease. With the
  default `replicaCount: 2` + leader election, a freshly-started replica that is
  NOT the leader stays `503` indefinitely by design (standby). The leader-handoff
  window (Lease `LeaseDuration: 15s`, `RenewDeadline: 10s`, `RetryPeriod: 2s`)
  means a new leader can take up to ~15s to be elected after the old one dies.
- `cat.Ready()` — for a **git-sync** catalog (`catalog.NewEmpty()`), this stays
  `false` until the FIRST successful clone+bleve-index completes
  (`catalog.go:Ready()` reads an `atomic.Bool` set on first `Reload`). A network
  clone + index of a real KB repo can take many seconds. For a static
  (`configMap` / local-dir) catalog, `Ready()` is true at load. For no catalog,
  readiness is pure leadership.

Liveness (`/healthz`) always returns 200 once the HTTP server is listening. The
git-sync syncer runs in a **goroutine** (`go syncer.Run(...)`), so `buildCatalog`
returns immediately and `ListenAndServe` starts within ~1s of process start.
=> Liveness is NOT at risk from a slow catalog clone. `livenessProbe` is sane.

## Verdict

Timing IS tight, but the failure mode in the finding is overstated:

- **No restart risk.** A failing readiness probe only pulls the pod out of
  Service rotation; it never restarts the pod. Liveness comes up fast and is
  decoupled from warmth. So a slow warmup cannot crash-loop the pod.
- **Real symptom = readiness flapping / noisy "Unhealthy" events.** With
  `readinessProbe.initialDelaySeconds: 2`, `periodSeconds: 5` and the default
  `failureThreshold: 3`, every git-sync warmup and every leader handoff produces
  a run of `Unhealthy (readiness)` events on the pod until the gate clears. On a
  standby replica (never the leader) that is *permanent* readiness-probe failure
  noise for the life of the pod.

So: not already mitigated (no `startupProbe` exists), the finding's direction is
right, its severity ("flapping", implying restarts) is wrong.

## Decision

Add a **`startupProbe`** on `/healthz` (preferred fix) plus make all three probes
configurable via values, keeping the tight readiness cadence for fast post-startup
leader-handoff detection. Rationale:

- A `startupProbe` is the idiomatic Kubernetes answer to "slow start, then probe
  normally". While it runs, the readiness and liveness probes are **suppressed**,
  so no premature `Unhealthy` readiness events during the initial HTTP-server
  bring-up window. Once it passes once, readiness/liveness take over.
- The startupProbe targets `/healthz` (process-alive), NOT `/readyz`. Gating
  startup on `/readyz` would deadlock a standby replica (never leader ⇒ never
  ready ⇒ startupProbe never passes ⇒ kubelet kills it). `/healthz` flips green
  as soon as the server listens, which is exactly "the process finished booting".
- Liveness stays on `/healthz` and is NOT loosened — a genuine process hang still
  trips it on the normal cadence. We only add a small `failureThreshold` so the
  budget is explicit, without hiding hangs.
- Readiness keeps a tight cadence (`periodSeconds: 5`) so that AFTER startup, a
  leader handoff is reflected in Service endpoints within a few seconds. Its
  `initialDelaySeconds` is dropped to 0 (the startupProbe already covers the cold
  window) but defended by `failureThreshold` for the warmup-after-startup tail
  (git-sync index finishing just after the server starts listening).

### Probe defaults (rendered)

```yaml
startupProbe:            # generous warmup budget: 30 * 2s = 60s before liveness/readiness engage
  httpGet: { path: /healthz, port: http }
  periodSeconds: 2
  failureThreshold: 30
  timeoutSeconds: 2
livenessProbe:          # unchanged cadence; failureThreshold explicit, does not hide hangs
  httpGet: { path: /healthz, port: http }
  periodSeconds: 10
  failureThreshold: 3
  timeoutSeconds: 2
readinessProbe:         # tight: fast leader-handoff / warmth detection post-startup
  httpGet: { path: /readyz, port: http }
  periodSeconds: 5
  failureThreshold: 3
  timeoutSeconds: 2
```

`initialDelaySeconds` is no longer needed on liveness/readiness because the
startupProbe owns the cold-start window; kubelet does not run liveness/readiness
until the startupProbe succeeds. Keeping them at 0 (omitted) is correct and
avoids double-counting the warmup budget.

## Values surface

A `probes:` block in `values.yaml`, each probe fully overridable, the startupProbe
toggleable:

```yaml
probes:
  startup:
    enabled: true
    periodSeconds: 2
    failureThreshold: 30
    timeoutSeconds: 2
  liveness:
    periodSeconds: 10
    failureThreshold: 3
    timeoutSeconds: 2
  readiness:
    periodSeconds: 5
    failureThreshold: 3
    timeoutSeconds: 2
```

Template wires these with `default` fallbacks so an upgrade with a stale values
file still renders sane probes (no nil holes).

## Validation

- `helm lint deploy/helm/runlore` clean.
- `helm template` across: defaults; `catalog.gitSync=true`; static
  `catalog.configMap`; `probes.startup.enabled=false`; a custom
  `probes.startup.failureThreshold`. Each render is valid YAML (piped through
  `yq`) and shows the expected probe block.

## Non-goals

- No change to `readyFunc` / the Go readiness model.
- Liveness is NOT loosened to hide hangs.
- No change to `updateStrategy: Recreate` (orthogonal leader-election concern).
