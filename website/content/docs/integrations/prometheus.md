---
title: Prometheus / VictoriaMetrics
weight: 10
integration: {kind: metrics, id: prometheus}
---

**What it gives you** — the `query_metrics` and `query_metrics_range` tools: PromQL against any
backend that speaks the Prometheus HTTP API, including VictoriaMetrics.

## Minimal config

```yaml
metrics:
  url: http://vmsingle.observability.svc:8429
```

Pinning the flavor instead of auto-detecting it:

```yaml
metrics:
  url: http://prometheus.observability.svc:9090
  flavor: prometheus       # or "victoriametrics" — optional, auto-detected when omitted
```

## Verify it locally

```bash
curl -s "http://vmsingle.observability.svc:8429/api/v1/query?query=up" | jq .status
```

Then fire a test incident and confirm `query_metrics` appears among the tools the model called:

```bash
kubectl -n runlore logs deploy/runlore | grep 'tool=query_metrics'
```

## Notes

- **Presence enables it** — `metrics.url` set is all it takes; an unset URL leaves the tool
  unregistered, no error.
- Both Prometheus and VictoriaMetrics speak the same Prometheus HTTP API; VictoriaMetrics **also**
  accepts MetricsQL, a PromQL superset. `metrics.flavor` unlocks MetricsQL-only query guidance for the
  model — it is auto-detected at startup (`DetectFlavor` probes `/api/v1/status/buildinfo`) and fails
  safe to generic Prometheus behaviour on an ambiguous or failed probe, so the model is never told to
  use a dialect the backend might reject. Pin `flavor:` explicitly when the backend sits behind a
  proxy that confuses the probe.
- `token_env` (bearer auth) and `headers` (e.g. `X-Scope-OrgID` for a multi-tenant backend) are
  available like every other data-source endpoint. `headers` values are **not secret-safe over plain
  HTTP** — use `https://` for a public host, or keep secrets in `token_env` only.

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `metrics` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
