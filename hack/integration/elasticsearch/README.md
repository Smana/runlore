# Elasticsearch / OpenSearch logs providers — local verification

Starts a single-node Elasticsearch 8.x AND a single-node OpenSearch 2.x, seeds both with the
same ECS-shaped log documents (including a CrashLoopBackOff-style error burst), then exercises
the three logs tools (`query_logs`, `logs_error_summary`, `discover_log_fields`) against each —
first at the raw HTTP/DSL level (no LLM needed, proves the wire contract `internal/logs/
elasticsearch` encodes is correct against a REAL cluster), then optionally end to end through
`lore investigate` (needs a model configured).

**This recipe was authored but has NOT been run end to end** — the sandbox this was written in has
no Docker daemon available (`docker info` fails). Treat it as a reviewed-on-paper recipe, not a
verified one, until someone with a working Docker runs it. The client's behavior IS verified —
thoroughly, via `go test ./internal/logs/elasticsearch/...`, `./internal/logs/...`,
`./internal/investigate/...`, and `./internal/app/...` against mocked ES/OpenSearch responses — but
a mock can only be as correct as its author's understanding of the real wire format, which is
exactly the assumption this recipe exists to close.

## Run it

1. Start both backends:

   ```bash
   cd hack/integration/elasticsearch
   docker compose up -d
   docker compose ps   # wait for both healthchecks to report healthy
   ```

2. Seed both with the same ECS-shaped documents:

   ```bash
   ./seed.sh http://localhost:9200   # Elasticsearch
   ./seed.sh http://localhost:9201   # OpenSearch
   ```

   Each run prints the cluster health, confirms the index template applied, bulk-indexes 28
   documents (a "logs-demo" index matching the `logs-*` pattern), and prints a total hit count —
   expect `"value":28` (or a slightly different total on a re-seed against a non-empty index; the
   script deletes the index first, so a fresh run always lands on 28).

3. Detection — confirm `internal/logs.Detect` classifies each correctly:

   ```bash
   curl -s http://localhost:9200/ | grep -o '"number":"[^"]*"'        # Elasticsearch: version.number only
   curl -s http://localhost:9201/ | grep -o '"distribution":"[^"]*"'  # OpenSearch: version.distribution: opensearch
   ```

4. Exercise the three tools' underlying requests directly — these are EXACTLY the requests
   `internal/logs/elasticsearch.Client` issues (compare against `elasticsearch.go`'s `searchBody`/
   `Hits`/`TopMessages`/`FieldNames`); run each against BOTH `:9200` and `:9201` and confirm
   the responses are structurally identical (the whole premise this task rests on):

   **`query_logs`** — `_search` with `query_string` + a `range` filter, newest-first, size-capped:

   ```bash
   curl -s -X POST "http://localhost:9200/logs-*/_search" -H 'Content-Type: application/json' -d '{
     "query": {"bool": {"must": [{"query_string": {"query": "kubernetes.namespace:\"payments\" AND log.level:\"error\""}}]}},
     "sort": [{"@timestamp": {"order": "desc"}}],
     "size": 1000
   }' | python3 -m json.tool | head -40
   ```

   Expect ~23 hits (3 baseline + 20 burst lines), newest first.

   **`logs_error_summary`** — `date_histogram` split by `log.level`, PLUS the top-messages
   fallback in action (the seeded index maps `message` as `text`-only, matching the real ECS
   convention — see `seed.sh`'s comment):

   ```bash
   # date_histogram + terms sub-agg (the volume-over-time half)
   curl -s -X POST "http://localhost:9200/logs-*/_search" -H 'Content-Type: application/json' -d '{
     "size": 0,
     "query": {"bool": {"must": [{"query_string": {"query": "kubernetes.namespace:\"payments\""}}]}},
     "aggs": {"by_time": {"date_histogram": {"field": "@timestamp", "fixed_interval": "300s"},
       "aggs": {"by_level": {"terms": {"field": "log.level", "size": 20}}}}}
   }' | python3 -m json.tool

   # top-messages terms agg on `message` — THIS MUST 400 with an
   # illegal_argument_exception naming "Fielddata is disabled on text fields",
   # proving the fallback in TopMessages actually triggers against a real
   # cluster rather than only in the mocked test.
   curl -s -X POST "http://localhost:9200/logs-*/_search" -H 'Content-Type: application/json' -d '{
     "size": 0,
     "query": {"bool": {"must": [{"query_string": {"query": "kubernetes.namespace:\"payments\""}}]}},
     "aggs": {"top_messages": {"terms": {"field": "message", "size": 10}}}
   }' | python3 -m json.tool
   ```

   **`discover_log_fields`** — `_field_caps`, present on both distributions:

   ```bash
   curl -s "http://localhost:9200/logs-*/_field_caps?fields=*" | python3 -m json.tool
   ```

   Confirm `kubernetes.namespace`/`kubernetes.pod.name`/`kubernetes.container.name`/`log.level` show
   up as `keyword` (aggregatable), and `message` shows up as `text` (searchable, NOT aggregatable) —
   the exact mapping shape the fallback logic above depends on.

5. (Optional, needs a model configured) End-to-end through RunLore itself:

   ```bash
   go build -o /tmp/lore ./cmd/lore
   /tmp/lore investigate --config hack/integration/elasticsearch/runlore.elasticsearch.yaml \
     --alert CrashLoopBackOff --namespace payments --message "payments/api-0 crash-looping"
   /tmp/lore investigate --config hack/integration/elasticsearch/runlore.opensearch.yaml \
     --alert CrashLoopBackOff --namespace payments --message "payments/api-0 crash-looping"
   ```

   Confirm the findings cite the seeded `connection refused to postgres.payments.svc:5432` burst and
   the `Back-off restarting failed container` line, on BOTH runs.

6. `docker compose down -v` to stop and remove both containers.

## Notes

- `docker-compose.yml` disables both distributions' security plugins (plain HTTP, no auth) purely
  for a quick throwaway local cluster — a real deployment always uses TLS + `token_env`, see the
  docs page. RunLore's client never disables TLS verification; this recipe simply doesn't use TLS.
- `seed.sh`'s index template maps `message` as `text` with NO `keyword` sub-field, deliberately
  reproducing the real Elastic Common Schema convention (Filebeat's ECS template omits a keyword
  multi-field for `message`). Without this explicit template, Elasticsearch's default DYNAMIC
  mapping would auto-add `message.keyword` and mask the exact parity gap step 4 is meant to prove.
- Port `9201` (not the default `9200`) is used for OpenSearch on the host side purely to let both
  containers run side by side in one compose file; the containers themselves both listen on `9200`
  internally.
