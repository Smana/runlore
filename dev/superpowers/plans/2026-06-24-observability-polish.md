# Observability Polish (R20) — Implementation Plan

Spec: `dev/superpowers/specs/2026-06-24-observability-polish-design.md`

Two independent, incrementally-committed changes. Validate after each.

## Commit 1 — SLO-aligned latency histogram buckets (test-first)

1. **Test first** — extend `internal/telemetry/setup_test.go` (or a new test) to:
   - call `Setup`, `NewMetrics`, record a `ToolCallDuration` sample,
   - scrape `/metrics`, assert a non-default boundary appears, e.g.
     `runlore_tool_call_duration_seconds_bucket{...le="2.5"}` — impossible under
     the SDK defaults `{5,10,25,…}`, so the test fails before the change.
2. Run the test → red.
3. Edit `internal/telemetry/setup.go`: add the `latencyBuckets` ladder and four
   `sdkmetric.NewView(...)` registrations via `sdkmetric.WithView(...)`.
4. Run the test → green.
5. `go build ./... && go vet ./... && go test ./internal/telemetry/...`,
   `gofmt -l .`, `golangci-lint run ./internal/telemetry/... --enable gosec`.
6. Update `docs/observability.md`: note latency histograms carry explicit SLO
   buckets and list the boundaries.
7. **Commit**: `feat(telemetry): SLO-aligned buckets for latency histograms`.

## Commit 2 — Dashboard panels for the three un-paneled metrics

1. Edit `deploy/observability/grafana/runlore.json`:
   - timeseries panel for `runlore_tool_output_truncated_bytes_total` (Tools & model row),
   - heatmap for `runlore_coalesce_batch_size` (Alert intake row),
   - heatmap for `runlore_curation_dedup_score` (Learning loop row).
   - Recompute gridPos for affected rows; assign new unique panel ids.
2. `python3 -m json.tool deploy/observability/grafana/runlore.json` → exit 0.
3. Re-grep the dashboard: each of the three series now present; each new `expr`
   references a series in `metrics.go`.
4. **Commit**: `feat(observability): panels for truncation, batch size, dedup score`.

## Final validation

- Full `go build/vet/test ./...`, `gofmt -l .`, `golangci-lint run ./... --enable gosec`.
- `python3 -m json.tool` on the dashboard.
