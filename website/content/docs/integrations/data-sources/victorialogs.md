---
title: VictoriaLogs
weight: 302
integration: {kind: logs, id: victorialogs}
---

**What it gives you** — the `query_logs`, `logs_error_summary` and `discover_log_fields` tools over
LogsQL — the default logs backend RunLore shipped with.

## Minimal config

```yaml
logs:
  url: http://victorialogs.observability.svc:9428
```

## Verify it locally

```bash
curl -s "http://victorialogs.observability.svc:9428/select/logsql/query" \
  --data-urlencode 'query=*' --data-urlencode 'limit=1'
```

Then fire a test incident and confirm the logs tools appear among what the model called:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'tool=query_logs|tool=logs_error_summary|tool=discover_log_fields'
```

## Notes

- **Presence enables it** — `logs.url` set is all it takes.
- VictoriaLogs is the **auto-detect fail-safe default**: at startup RunLore probes
  `/loki/api/v1/status/buildinfo` (an endpoint VictoriaLogs does not serve); anything other than a
  Loki-shaped 200 response — unreachable backend, non-200, a 200 that isn't buildinfo JSON — resolves
  to VictoriaLogs. Pin `logs.provider: victorialogs` explicitly if the backend sits behind a proxy
  that confuses the probe, so a startup network hiccup can't misclassify it.
- The default field convention assumes the shipped VictoriaLogs + vector kubernetes-metadata layout:
  `container_field: kubernetes.container_name`, `namespace_field: kubernetes.pod_namespace`,
  `pod_field: kubernetes.pod_name`, `level_field: log.level`, `unpack_pipe: unpack_json`. Override any
  of them under `logs.fields` if your collector labels differently.
- See [Grafana Loki]({{< relref "loki.md" >}}) for the second supported backend and its parity
  caveats.

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `logs` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
