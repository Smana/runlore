# Elasticsearch / OpenSearch logs providers — local verification

Starts a single-node Elasticsearch 8.x AND a single-node OpenSearch 2.x, seeds both with the
same ECS-shaped log documents (including a CrashLoopBackOff-style error burst), then exercises
the three logs tools (`query_logs`, `logs_error_summary`, `discover_log_fields`) against each —
first at the raw HTTP/DSL level (no LLM needed, proves the wire contract `internal/logs/
elasticsearch` encodes is correct against a REAL cluster), then optionally end to end through
`lore investigate` (needs a model configured).

**Status: run against real backends** — Elasticsearch 8.15.0 and OpenSearch 2.17.0, the versions
pinned in `docker-compose.yml`. All three tools returned identical results on both distributions.
`internal/logs/elasticsearch/live_verify_test.go` is that harness, build-tagged `liveverify` so it
never runs in CI:

```bash
ES_URL=http://localhost:9200 OS_URL=http://localhost:9201 \
  go test -tags liveverify ./internal/logs/elasticsearch/ -run TestLive -v
```

The one thing the live run settled that no mock could: the text-field aggregation rejection is
**not worded identically** on the two distributions — Elasticsearch prefixes it with
`Fielddata is disabled on [message] in [logs-demo].`, OpenSearch does not. See
`isTextFieldAggError`'s comment in `elasticsearch.go` for what the matcher therefore keys on.

## Host prerequisite (Linux)

Both images fail their bootstrap check and restart-loop unless the kernel mmap limit is raised.
This is the single most common reason "it just doesn't start":

```bash
sudo sysctl -w vm.max_map_count=262144                                # this boot only
echo 'vm.max_map_count=262144' | sudo tee /etc/sysctl.d/99-opensearch.conf   # persist
```

The symptom is `max virtual memory areas vm.max_map_count [65530] is too low` in
`docker compose logs`, not a compose error — `docker compose ps` just shows a container restarting.

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

   Each run prints the cluster health, confirms the index template applied, bulk-indexes 30
   documents (a "logs-demo" index matching the `logs-*` pattern), and prints a total hit count —
   expect `"value":30`. The script deletes the index first, so a re-seed always lands on 30 too.

   That 30 is 5 baseline `checkout` lines + 3 baseline `payments` errors + the 20-line
   CrashLoopBackOff burst + 1 `Back-off restarting` error + 1 `container restarted` warn.

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

   Expect exactly 24 hits — 3 baseline `payments` errors + the 20-line burst + the 1
   `Back-off restarting failed container` error; the `container restarted` line is a `warn` and is
   correctly excluded. Newest first. (24 is what both real clusters returned.)

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
   # illegal_argument_exception, proving the fallback in TopMessages actually
   # triggers against a real cluster rather than only in the mocked test.
   # NOTE the wording differs by distribution: Elasticsearch opens with
   # "Fielddata is disabled on [message] in [logs-demo]." and OpenSearch does
   # not. Both share "Text fields are not optimised …" and "set fielddata=true",
   # which is what isTextFieldAggError keys on — do not match the ES-only
   # sentence.
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
- For OpenSearch that means BOTH `DISABLE_SECURITY_PLUGIN=true` and
  `DISABLE_INSTALL_DEMO_CONFIG=true`. Setting only the first leaves the entrypoint running
  `install_demo_configuration.sh` on every start (demo TLS certs, and on 2.12+ a hard failure when
  no `OPENSEARCH_INITIAL_ADMIN_PASSWORD` is set). With the demo config disabled, no admin password
  is needed at all.
- `seed.sh`'s index template maps `message` as `text` with NO `keyword` sub-field, deliberately
  reproducing the real Elastic Common Schema convention (Filebeat's ECS template omits a keyword
  multi-field for `message`). Without this explicit template, Elasticsearch's default DYNAMIC
  mapping would auto-add `message.keyword` and mask the exact parity gap step 4 is meant to prove.
- Port `9201` (not the default `9200`) is used for OpenSearch on the host side purely to let both
  containers run side by side in one compose file; the containers themselves both listen on `9200`
  internally.
