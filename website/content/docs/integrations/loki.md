---
title: Grafana Loki
weight: 30
integration: {kind: logs, id: loki}
---

**What it gives you** — the same `query_logs`, `logs_error_summary` and `discover_log_fields` tools,
speaking LogQL instead of LogsQL, auto-detected at startup.

## Minimal config

```yaml
logs:
  url: http://loki-gateway.observability.svc:80
  provider: loki          # optional — auto-detected when omitted
```

Authenticated / multi-tenant:

```yaml
logs:
  url: http://loki-gateway.observability.svc:80
  provider: loki
  token_env: LOKI_TOKEN                 # bearer token, by env-var indirection
  headers: { X-Scope-OrgID: my-tenant } # multi-tenant Loki
```

## Verify it locally

```bash
curl -s "http://loki-gateway.observability.svc:80/loki/api/v1/status/buildinfo"
```

A 200 with a `version` field is what RunLore's own startup probe looks for. Then fire a test incident
and confirm the logs tools appear among what the model called:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'tool=query_logs|tool=logs_error_summary|tool=discover_log_fields'
```

## Notes

- **Presence enables it** — `logs.url` set is all it takes; `provider: loki` is optional (the startup
  probe hits `/loki/api/v1/status/buildinfo`, which only Loki answers, and identifies it that way).
  Pin it explicitly when the backend is unreachable at startup or sits behind a proxy that confuses
  the probe — an unreachable/ambiguous probe fails safe to VictoriaLogs, not to Loki.
- Loki's default field convention differs from VictoriaLogs': `container_field: container`,
  `namespace_field: namespace`, `pod_field: pod`, `level_field: detected_level`, and **no default
  `unpack_pipe`** (`detected_level` is structured metadata, filterable without a parser stage).
  Override any of them under `logs.fields`.

**Parity notes — read before relying on Loki for these three tools:**

- `logs_error_summary`'s histogram uses a LogQL metric query
  (`sum by (detected_level) (count_over_time(…))`), but its **top messages are aggregated
  client-side** over the capped, newest-first query sample — counts are **per-sample, not
  corpus-wide**, unlike the VictoriaLogs path.
- `discover_log_fields` merges stream labels (`/loki/api/v1/labels`) with detected body fields
  (`/loki/api/v1/detected_fields`, **Loki ≥ 3.0 only** — older Loki degrades to labels-only field
  discovery).
- **Loki 2.x has no `detected_level`.** Set
  `logs: { fields: { level_field: level, unpack_pipe: logfmt } }` (or `unpack_pipe: json`, matching
  your collector) to get level filtering working on Loki 2.x.

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `logs` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
- [VictoriaLogs]({{< relref "victorialogs.md" >}}) — the fail-safe default this auto-detect falls
  back to.
