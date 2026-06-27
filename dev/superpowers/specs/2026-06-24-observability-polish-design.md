# Observability Polish (R20) — Design Spec

- **Date:** 2026-06-24
- **Status:** Draft (awaiting review)
- **Type:** Observability (Grafana panels + SLO-aligned histogram buckets)
- **Author:** RunLore maintainers

## 1. Problem

Item R20 collects two independent observability gaps. Both verified against the
current code before any change.

### (a) Three emitted metrics have no Grafana panel

`internal/telemetry/metrics.go` constructs and the code records three instruments
whose series never appear in `deploy/observability/grafana/runlore.json`
(`grep` of the dashboard returns zero hits for each):

| Instrument (Go field) | Prometheus series | Type | Labels |
|---|---|---|---|
| `ToolOutputTruncatedBytes` | `runlore_tool_output_truncated_bytes_total` | counter | — |
| `CoalesceBatchSize` | `runlore_coalesce_batch_size` (`_bucket`/`_sum`/`_count`) | histogram | — |
| `CurationDedupScore` | `runlore_curation_dedup_score` (`_bucket`/…) | histogram | — |

All three are real, exported series (verified at `metrics.go:72,76,79`). They are
documented in `docs/observability.md` but invisible on the dashboard, so an
operator cannot see truncation pressure, batch sizing, or curation-dedup score
distribution without ad-hoc PromQL.

### (b) Latency histograms use OTel SDK default buckets, not SLO-aligned ones

`Setup()` (`internal/telemetry/setup.go`) wires the OTel→Prometheus exporter with
`sdkmetric.NewMeterProvider(sdkmetric.WithReader(exporter))` and **no views**. With
no explicit-bucket view, every `Float64Histogram` inherits the OTel SDK default
boundaries:

```
0, 5, 10, 25, 50, 75, 100, 250, 500, 750, 1000, 2500, 5000, 7500, 10000
```

These are tuned for **millisecond**-scale values. RunLore's latency histograms are
in **seconds**:

- `runlore_tool_call_duration_seconds` (panel 33 already does `histogram_quantile(0.95, …)`)
- `runlore_model_request_duration_seconds` (panel 36, p95)
- `runlore_investigation_duration_seconds` (panel 22, p50/p95/p99)
- `runlore_incident_resolution_seconds` (panel 52, p50/p95)

A tool call at 0.2s and one at 4.9s both land in the first `[0,5]` bucket. Existing
`histogram_quantile` panels therefore interpolate inside one bucket and return
near-meaningless numbers below 5s — i.e. for the common case. This is a real defect
in panels that already exist, not a cosmetic one.

The non-latency histograms (`coalesce_batch_size`, `recall_score`,
`curation_dedup_score`, `investigation_tokens_estimated`) are *count*/*score*/*token*
distributions, not latencies, and are rendered as heatmaps (raw `_bucket` by `le`),
not via `histogram_quantile`. SLO-aligned latency buckets do not apply to them, but
the SDK defaults are still a poor fit (e.g. token estimates of thousands collapse
into the top open bucket). We give the seconds-scale latency histograms explicit
SLO buckets and leave the rest on defaults for this change to stay minimal — except
`investigation_tokens_estimated`, which is large-valued enough that the default
top bucket (10000) is a borderline reasonable ceiling, so it is left untouched.

## 2. Goals

1. Add three dashboard panels (one per un-paneled metric), matching existing panel
   structure, datasource template variable (`${datasource}`), and `pluginVersion`.
2. Give the four seconds-scale latency histograms explicit, SLO-aligned bucket
   boundaries via an OTel explicit-bucket-histogram view, so the existing
   percentile panels return meaningful values.
3. Keep `runlore.json` valid (`python3 -m json.tool`) and every new `expr`
   referencing a series that exists in `metrics.go`.
4. Keep `docs/observability.md` accurate.

## Non-goals

- No new metrics, no renames (the contract test pins series names).
- No change to alert rules.
- No re-bucketing of non-latency histograms.

## 3. Design

### (a) Panels

Append to existing rows (no new rows needed; keep gridPos consistent):

- **`runlore_tool_output_truncated_bytes_total`** — timeseries, rate of bytes
  elided by output truncation. Lives in the "Tools & model" row.
  `expr: sum(rate(runlore_tool_output_truncated_bytes_total[$__rate_interval]))`,
  unit `Bps`.
- **`runlore_coalesce_batch_size`** — heatmap of batch size distribution, in the
  "Alert intake" row (alongside intake rate).
  `expr: sum(rate(runlore_coalesce_batch_size_bucket[$__rate_interval])) by (le)`,
  format `heatmap`. Modeled on panel 42.
- **`runlore_curation_dedup_score`** — heatmap of BM25 top-hit score at the dedup
  decision, in the "Learning loop" row (mirrors the recall-score heatmap).
  `expr: sum(rate(runlore_curation_dedup_score_bucket[$__rate_interval])) by (le)`.

Existing rows are re-laid-out only as needed so the new panels tile cleanly. New
panel ids use a free range (e.g. 12, 13, 55) and gridPos is recomputed for the
affected rows.

### (b) SLO-aligned latency buckets

In `Setup()`, add explicit-bucket views before constructing the provider:

```go
latencyBuckets := []float64{0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 30, 60, 120, 300}
view := func(instrument string) sdkmetric.View {
    return sdkmetric.NewView(
        sdkmetric.Instrument{Name: instrument},
        sdkmetric.Stream{Aggregation: sdkmetric.AggregationExplicitBucketHistogram{
            Boundaries: latencyBuckets,
        }},
    )
}
mp := sdkmetric.NewMeterProvider(
    sdkmetric.WithReader(exporter),
    sdkmetric.WithView(
        view("runlore_tool_call_duration_seconds"),
        view("runlore_model_request_duration_seconds"),
        view("runlore_investigation_duration_seconds"),
        view("runlore_incident_resolution_seconds"),
    ),
)
```

**Boundary rationale** (seconds): `50ms…300ms` resolves fast tool calls; `0.5…2.5s`
the typical tool/model range; `5…10s` slow calls; `30…300s` long investigations and
incident resolution (which can run minutes). One shared ladder keeps it simple and
covers all four; `incident_resolution_seconds` can exceed 300s but the `+Inf`
bucket captures the tail and the p50/p95 panel reads correctly up to 5 min.

## 4. Validation

- `python3 -m json.tool deploy/observability/grafana/runlore.json` → exit 0.
- Each new `expr` references a series present in `metrics.go`.
- `go build ./... && go vet ./... && go test ./internal/telemetry/...` green.
- `gofmt -l .` clean; `golangci-lint run ./...` (+`--enable gosec`) clean.
- A new telemetry test asserts the bucket boundaries reach `/metrics`
  (e.g. a `le="2.5"` bucket exists for `runlore_tool_call_duration_seconds`,
  which it cannot under the defaults).
- `docs/observability.md` updated to note SLO buckets on the latency histograms.

## 5. Risks / deviations

- View names must match the **exported Prometheus** series name
  (`runlore_*`), which is the instrument name OTel sees. Confirmed via the
  contract test that these are the names at `/metrics`.
- If a future histogram is added it inherits defaults unless added to the view list
  — acceptable; documented inline.
