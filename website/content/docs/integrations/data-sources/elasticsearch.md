---
title: Elasticsearch
weight: 304
integration: {kind: logs, id: elasticsearch}
---

**What it gives you** — the same `query_logs`, `logs_error_summary` and `discover_log_fields` tools,
speaking Lucene `query_string` over the classic `_search` DSL instead of LogsQL/LogQL, auto-detected
at startup. See [OpenSearch]({{< relref "opensearch.md" >}}) for the same client against an
OpenSearch cluster — the two speak an identical `_search`/`_field_caps` wire format.

## Minimal config

```yaml
logs:
  url: https://elasticsearch.observability.svc:9200
  provider: elasticsearch      # optional — auto-detected when omitted
  index: logs-*                # index pattern
```

Authenticated:

```yaml
logs:
  url: https://elasticsearch.observability.svc:9200
  provider: elasticsearch
  index: logs-*
  token_env: ES_TOKEN                     # bearer token, by env-var indirection
  # headers: { Authorization: "ApiKey <base64 id:key>" }  # ES API key instead of a bearer token
```

A `headers.Authorization` entry is applied AFTER `token_env`, so it wins — use it for an
Elasticsearch [API key](https://www.elastic.co/guide/en/elasticsearch/reference/current/security-api-create-api-key.html)
(`Authorization: ApiKey <base64>`) rather than a bearer token.

## Verify it locally

```bash
curl -sk -X POST "https://elasticsearch.observability.svc:9200/logs-*/_search" \
  -H 'Content-Type: application/json' -d '{"size":1,"query":{"match_all":{}}}'
```

Then fire a test incident and confirm the logs tools appear among what the model called:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'tool=query_logs|tool=logs_error_summary|tool=discover_log_fields'
```

## Notes

- **Presence enables it** — `logs.url` set is all it takes; `provider: elasticsearch` is optional (the
  startup probe hits `GET /`, which both Elasticsearch and VictoriaLogs answer, but only
  Elasticsearch/OpenSearch return a `version.number` in the response body — VictoriaLogs' root page is
  HTML). Pin it explicitly when the backend is unreachable at startup or sits behind a proxy that
  confuses the probe — an unreachable/ambiguous probe fails safe to VictoriaLogs, not to Elasticsearch.
- **TLS verification is always on** — there is no `insecure_skip_verify` escape hatch. Point `logs.url`
  at a certificate the runtime trusts (a cluster CA bundle, or a properly-issued cert behind an
  ingress) rather than disabling verification.
- The ECS (Elastic Common Schema) field convention is assumed by default: `container_field:
  kubernetes.container.name`, `namespace_field: kubernetes.namespace`, `pod_field:
  kubernetes.pod.name`, `level_field: log.level`, `timestamp_field: @timestamp`, `message_field:
  message`. Override any of them under `logs.fields` if your index template labels differently.
  `unpack_pipe` is ignored — Elasticsearch has no parser-pipe concept (documents are already
  structured).

**Parity notes — read before relying on Elasticsearch for these three tools:**

- **`logs_error_summary`'s top messages fall back to client-side aggregation when the message field
  is `text`-only** — and by default, it is. ECS ships `message` as a plain `text` field with no
  `keyword` sub-field, so Elasticsearch/OpenSearch reject a `terms` aggregation on it
  (`illegal_argument_exception: Fielddata is disabled on text fields`). RunLore tries the server-side
  aggregation first (cheap and corpus-wide when it succeeds) and, on exactly that rejection, falls
  back to aggregating over the capped, newest-first `query_logs` sample instead — counts and
  first→last spans are then **per-sample, not corpus-wide**, the same caveat Loki's client-side
  fallback carries. If your index template adds an aggregatable multi-field (commonly
  `message.keyword`), point `logs.fields.message_field` at it to get the corpus-wide, server-side
  path instead. RunLore learns the rejection once per process and goes straight to the fallback
  afterwards, so the default configuration does not repeat a request the cluster will always refuse.
  (OpenSearch's wording for the same rejection differs slightly — RunLore matches the part both
  distributions emit, verified against real clusters of each.)
- **Partial results are flagged, never silently reported as complete.** Elasticsearch answers a
  search some shards could not serve with HTTP 200 and `_shards.failed > 0` — typically when a
  rolling `logs-*` pattern spans a mapping change, so a `terms` aggregation succeeds on the newer
  indices and is rejected on the older ones. `query_logs` appends an explicit "partial results"
  line, `logs_error_summary`'s top messages switch to the client-side path (which is unaffected),
  and its histogram reports the partiality as an error rather than showing a chart that understates
  the spike.
- **`discover_log_fields` lists the INDEX'S MAPPED fields, not fields present only in matching
  documents.** It uses `_field_caps`, a mapping-introspection endpoint with no query/window scope —
  broader than VictoriaLogs'/Loki's discovery (which is scoped to what a query actually matched), and
  it reports no per-field hit count (every result shows no `×N` frequency), unlike VictoriaLogs'
  `field_names`.
- The error-volume histogram (`date_histogram` + a `terms` sub-aggregation on `level_field`) IS
  corpus-wide and server-side — `log.level` ships as a `keyword`-typed field under ECS, so it
  aggregates without the `message` field's restriction.

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `logs` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
- [VictoriaLogs]({{< relref "victorialogs.md" >}}) — the fail-safe default this auto-detect falls
  back to.
- [OpenSearch]({{< relref "opensearch.md" >}}) — the same client, same DSL, same parity notes.
