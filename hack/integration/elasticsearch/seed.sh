#!/usr/bin/env bash
# Seeds an Elasticsearch or OpenSearch instance (they speak an identical
# _search/_bulk wire format) with ECS-shaped log documents for the
# hack/integration/elasticsearch/ verification recipe: a handful of baseline
# info/warn lines across two namespaces, plus a CrashLoopBackOff-style burst
# of near-identical error lines from one pod — the shape logs_error_summary's
# histogram + top-messages should surface as "baseline -> spike".
#
# The index template below maps `message` as `text` ONLY (no `keyword`
# sub-field) — matching the REAL Elastic Common Schema convention (Filebeat's
# ECS template omits a keyword multi-field for `message` on purpose, since log
# lines are high-cardinality/long). Elasticsearch's default DYNAMIC mapping
# would otherwise auto-add a `message.keyword` sub-field and mask the exact
# parity gap this recipe exists to demonstrate: logs_error_summary's top
# messages must fall back to client-side aggregation against this cluster.
#
# Usage:
#   ./seed.sh http://localhost:9200   # Elasticsearch (docker-compose default)
#   ./seed.sh http://localhost:9201   # OpenSearch (docker-compose default)
set -euo pipefail

BASE_URL="${1:-http://localhost:9200}"
INDEX="logs-demo"

echo "waiting for ${BASE_URL} to be healthy..."
for _ in $(seq 1 60); do
  if curl -sf "${BASE_URL}/_cluster/health" >/dev/null 2>&1; then
    break
  fi
  sleep 2
done
curl -sf "${BASE_URL}/_cluster/health?pretty"

echo "putting index template (message: text-only, matching the real ECS convention)..."
curl -sf -X PUT "${BASE_URL}/_index_template/logs-demo-template" \
  -H 'Content-Type: application/json' -d '{
  "index_patterns": ["logs-*"],
  "template": {
    "mappings": {
      "properties": {
        "@timestamp": {"type": "date"},
        "message": {"type": "text"},
        "log": {"properties": {"level": {"type": "keyword"}}},
        "kubernetes": {
          "properties": {
            "namespace": {"type": "keyword"},
            "pod": {"properties": {"name": {"type": "keyword"}}},
            "container": {"properties": {"name": {"type": "keyword"}}}
          }
        }
      }
    }
  }
}' >/dev/null
echo

echo "deleting any previous ${INDEX} index (idempotent re-seed)..."
curl -s -X DELETE "${BASE_URL}/${INDEX}" >/dev/null || true

# now() minus an offset in whole seconds, RFC3339 UTC — bash+date, no deps.
ts() { date -u -d "-$1 seconds" +"%Y-%m-%dT%H:%M:%S.000Z" 2>/dev/null || date -u -v-"$1"S +"%Y-%m-%dT%H:%M:%S.000Z"; }

BULK_FILE="$(mktemp)"
trap 'rm -f "$BULK_FILE"' EXIT

add_doc() {
  # add_doc <seconds-ago> <namespace> <pod> <container> <level> <message>
  # Messages below are plain ASCII with no quotes/backslashes, so a bare
  # printf substitution is safe — no JSON-escaping dependency needed.
  local secs="$1" ns="$2" pod="$3" container="$4" level="$5" msg="$6"
  cat >>"$BULK_FILE" <<EOF
{"index":{"_index":"${INDEX}"}}
{"@timestamp":"$(ts "$secs")","message":"${msg}","log":{"level":"${level}"},"kubernetes":{"namespace":"${ns}","pod":{"name":"${pod}"},"container":{"name":"${container}"}}}
EOF
}

# Baseline traffic: a healthy checkout service, mostly info, occasional warn.
add_doc 3600 checkout web-0 web info  "request completed in 42ms"
add_doc 3300 checkout web-0 web info  "request completed in 51ms"
add_doc 3000 checkout web-1 web warn  "slow downstream call: 890ms"
add_doc 2700 checkout web-0 web info  "request completed in 38ms"
add_doc 2400 checkout worker-0 worker info "processed batch of 25 orders"

# A quiet baseline of errors from payments too, BEFORE the burst — this is the
# "3/5m baseline" logs_error_summary's histogram should show, contrasted with
# the spike below.
add_doc 2100 payments api-0 api error "retrying database connection (attempt 1/5)"
add_doc 1980 payments api-0 api error "retrying database connection (attempt 1/5)"
add_doc 1860 payments api-0 api error "retrying database connection (attempt 1/5)"

# The CrashLoopBackOff-style burst: payments/api-0 spinning against a
# postgres it can't reach, ~20 near-identical lines within a couple of
# minutes — the dominant message logs_error_summary's top-messages (or its
# client-side fallback, since `message` is text-only here) should surface.
for i in $(seq 1 20); do
  secs=$((300 - i * 8))
  add_doc "$secs" payments api-0 api error "connection refused to postgres.payments.svc:5432 (attempt ${i}/20)"
done
add_doc 40 payments api-0 api error "Back-off restarting failed container api in pod api-0_payments(a1b2c3)"
add_doc 20 payments api-0 api warn  "container api restarted (restartCount=7)"

echo "bulk-indexing $(grep -c '"index"' "$BULK_FILE") documents into ${INDEX}..."
curl -sf -X POST "${BASE_URL}/_bulk" -H 'Content-Type: application/x-ndjson' --data-binary "@${BULK_FILE}" \
  | grep -o '"errors":[a-z]*'

curl -sf -X POST "${BASE_URL}/${INDEX}/_refresh" >/dev/null

echo
echo "seeded ${BASE_URL}/${INDEX} — sanity check:"
curl -sf -X POST "${BASE_URL}/${INDEX}/_search" -H 'Content-Type: application/json' \
  -d '{"size":0,"query":{"match_all":{}}}' | grep -o '"value":[0-9]*' | head -1
