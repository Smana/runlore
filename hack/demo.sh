#!/usr/bin/env bash
# Demo: run `lore serve` and fire mocked Alertmanager alerts through the trigger policy.
#
# The Alertmanager webhook JSON is the mock "event". examples/alertmanager-webhook.json
# exercises every policy path: match, dedup (repeated fingerprint), wrong-severity,
# wrong-environment, ignore-list, and a resolved alert (dropped before the decision).
#
# Usage: hack/demo.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
ADDR=":18080"
BIN="$(mktemp -d)/lore"
CFG="$(mktemp)"
LOG="$(mktemp)"

go build -o "$BIN" "$ROOT/cmd/lore"

cat > "$CFG" <<'EOF'
triggers:
  incidents:
    enabled: true
    match:
      severity: [critical]
      environment: [prod]
    ignore:
      alertnames: [Watchdog]
    dedup: { window: 30m }
EOF

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
