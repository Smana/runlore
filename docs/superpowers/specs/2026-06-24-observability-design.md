# Design — Observability: structured logging, metrics, dashboard & alerts

Date: 2026-06-24 · Status: approved (autonomous) · Worktree: `feat/observability`

## Problem

- **Logging**: `slog.NewTextHandler` was hardcoded at every site (6×) with `nil` options
  → no JSON output, no level control, no central constructor. In-cluster logs couldn't be
  shipped as structured records.
- **Metrics**: a good OTel→Prometheus set existed, but lacked per-tool / per-model /
  per-investigation-duration / completion-result / build-info / leadership signals needed
  for a useful dashboard and meaningful alerts.
- **No Grafana dashboard and no alert rules** shipped at all.

## Changes

### Logging (`internal/logging`)
- `New(w, format, level)` builds a text or JSON slog handler at a parsed level.
- `FromConfig(w, format, level)` applies `RUNLORE_LOG_FORMAT` / `RUNLORE_LOG_LEVEL` overrides.
- `config.Logging{Format, Level}` (`logging:` block). All 5 live logger sites in `cmd/lore`
  route through `FromConfig`; the `io.Discard` eval logger stays silent by design.
- Helm defaults in-cluster to JSON/info.

### Metrics (`internal/telemetry`)
New instruments (names are the dashboard/alert contract — covered by a test):
`runlore_build_info{version}`, `runlore_leader`, `runlore_investigation_duration_seconds{result}`,
`runlore_investigations_completed_total{result}`, `runlore_tool_calls_total{tool,result}`,
`runlore_tool_call_duration_seconds{tool}`, `runlore_model_requests_total{provider,result}`,
`runlore_model_request_duration_seconds{provider}`, `runlore_curations_total{kind,result}`.
- Gauges via `RegisterRuntimeGauges(version, isLeader)` (observable; wired in `serve` after
  leader election). `NewMetrics()` signature is unchanged (many call sites/tests depend on it).
- Wired at the natural chokepoints: the loop's model call + `runTool` + each `Investigate`
  exit (deferred recorder, result-tagged), and the curator's PR/coalesce paths.

### Dashboard & alerts (`deploy/observability/`, portable — any environment)
- `grafana/runlore.json` — one `datasource` template var (Prometheus type → works for
  Prometheus *and* VictoriaMetrics), `$__rate_interval`, no hardcoded UIDs.
- `alerts/prometheusrule.yaml` + `alerts/vmrule.yaml` — identical 12-rule set (liveness, HA,
  pipeline-stall, error rates, latency, cost), shipped for both operators.
- `docs/observability.md` documents logging, the metric reference, dashboard import, alerts.

## Testing
`logging` unit tests (format/level/env-override); a telemetry test that scrapes `/metrics`
and asserts every new contract series name is exposed (rename ⇒ test breaks).
build / vet / gofmt / test / golangci-lint all clean.
