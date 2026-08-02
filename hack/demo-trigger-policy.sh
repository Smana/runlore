#!/usr/bin/env bash
# Trigger-policy demo: run `lore serve` and fire mocked Alertmanager alerts through
# the trigger policy, showing which alerts become incidents (match, dedup,
# wrong-severity, wrong-environment, ignore-list, resolved).
#
# This shows the FILTER, not the investigation. For the investigation — a real root
# cause, keyless — run hack/demo.sh.
#
# Usage: hack/demo-trigger-policy.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ADDR=":18080"
BIN="$(mktemp -d)/lore"
CFG="$ROOT/hack/demo.config.yaml"   # committed + parse-tested (internal/config)
LOG="$(mktemp)"

go build -o "$BIN" "$ROOT/cmd/lore"

"$BIN" serve --config "$CFG" --addr "$ADDR" > "$LOG" 2>&1 &
SRV=$!
trap 'kill "$SRV" 2>/dev/null || true' EXIT

# Wait for the server, then fire the mock alerts.
curl -s --retry-connrefused --retry 10 --retry-delay 1 -o /dev/null "http://localhost${ADDR}/healthz"
curl -s -o /dev/null -w 'webhook HTTP %{http_code}\n' \
  -XPOST "http://localhost${ADDR}/webhook/alertmanager" \
  --data @"$ROOT/examples/alertmanager-webhook.json"

echo "=== trigger-policy decisions ==="
grep "msg=incident" "$LOG"
