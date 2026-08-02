---
title: OpenSearch
weight: 50
integration: {kind: logs, id: opensearch}
---

**What it gives you** — the same `query_logs`, `logs_error_summary` and `discover_log_fields` tools as
[Elasticsearch]({{< relref "elasticsearch.md" >}}), speaking the identical Lucene `query_string` over
the classic `_search` DSL: OpenSearch forked from Elasticsearch 7.10 and kept the same wire format, so
RunLore ships ONE client for both — this page only covers what differs (auth conventions and
detection), not a second implementation.

## Minimal config

```yaml
logs:
  url: https://opensearch.observability.svc:9200
  provider: opensearch      # optional — auto-detected when omitted
  index: logs-*             # index pattern
```

Authenticated:

```yaml
logs:
  url: https://opensearch.observability.svc:9200
  provider: opensearch
  index: logs-*
  token_env: OPENSEARCH_TOKEN   # bearer token, by env-var indirection
```

## Verify it locally

```bash
curl -sk -X POST "https://opensearch.observability.svc:9200/logs-*/_search" \
  -H 'Content-Type: application/json' -d '{"size":1,"query":{"match_all":{}}}'
```

Then fire a test incident and confirm the logs tools appear among what the model called:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'tool=query_logs|tool=logs_error_summary|tool=discover_log_fields'
```

## Notes

- **Presence enables it** — `logs.url` set is all it takes; `provider: opensearch` is optional. The
  startup probe hits `GET /`: OpenSearch's response carries `version.distribution: opensearch`, the
  field OpenSearch itself adds so a client can tell it apart from Elasticsearch at the same endpoint
  shape — its absence (a plain `version.number`) means Elasticsearch instead. Pin the provider
  explicitly when the backend is unreachable at startup or sits behind a proxy that confuses the
  probe — an unreachable/ambiguous probe fails safe to VictoriaLogs, not to OpenSearch.
- **TLS verification is always on** — there is no `insecure_skip_verify` escape hatch.
- Same ECS field convention as Elasticsearch: `container_field: kubernetes.container.name`,
  `namespace_field: kubernetes.namespace`, `pod_field: kubernetes.pod.name`, `level_field:
  log.level`, `timestamp_field: @timestamp`, `message_field: message`, all overridable under
  `logs.fields`.
- OpenSearch's own auth plugins (Basic auth, or its own API-key flow) map onto the same knobs:
  `token_env` sends a bearer token; for HTTP Basic auth against OpenSearch's security plugin, set
  `headers: { Authorization: "Basic <base64 user:pass>" }` instead (applied after `token_env`, so it
  wins).

**Parity notes — identical to Elasticsearch, read before relying on OpenSearch for these three
tools:**

- **`logs_error_summary`'s top messages fall back to client-side aggregation when the message field
  is `text`-only** — the default for ECS's `message` field on OpenSearch too: it rejects the same
  `terms` aggregation with the same `illegal_argument_exception`. The rejection *message* is
  slightly different (OpenSearch omits the `Fielddata is disabled on [field] in [index].` sentence
  Elasticsearch opens with); RunLore matches on the part both emit, verified against real clusters
  of each. Point `logs.fields.message_field` at an aggregatable multi-field (e.g. `message.keyword`)
  to get the corpus-wide, server-side path instead of the per-sample fallback.
- **`discover_log_fields` lists the INDEX'S MAPPED fields** via `_field_caps` (present on OpenSearch
  2.x), not fields scoped to a query's matches, and reports no per-field hit count.
- **Partial results are flagged, never silently reported as complete.** OpenSearch answers a search
  some shards could not serve with HTTP 200 and `_shards.failed > 0` — typically when a rolling
  `logs-*` pattern spans a mapping change. `query_logs` appends an explicit "partial results" line,
  `logs_error_summary`'s top messages switch to the client-side path, and its histogram reports the
  partiality as an error rather than showing a short bar chart.
- The error-volume histogram is corpus-wide and server-side (`log.level` is `keyword`-typed under
  ECS).

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `logs` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
- [VictoriaLogs]({{< relref "victorialogs.md" >}}) — the fail-safe default this auto-detect falls
  back to.
- [Elasticsearch]({{< relref "elasticsearch.md" >}}) — the same client, same DSL, full parity detail.
