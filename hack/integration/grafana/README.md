# Grafana source — local verification

Runs a real Grafana with a provisioned alert rule and contact point wired to a `lore serve`
running on the host, end to end: fire the rule, confirm an investigation starts with the right
namespace/workload; let it resolve, confirm a resolution is recorded rather than a second
investigation.

No cluster, no Prometheus — the rule queries Grafana's built-in **TestData** datasource with the
`predictable_pulse` scenario, which alternates firing/resolved on its own (20s high, 20s low), so
no manual "make it fire" step is needed.

## Run it

1. Start `lore serve` on the host, keyless (no LLM configured — this only proves ingestion,
   trigger-policy admission and the investigation *starting*, same scope as `hack/demo-trigger-policy.sh`):

   ```bash
   export GRAFANA_WEBHOOK_TOKEN=demo-token
   go build -o /tmp/lore ./cmd/lore
   /tmp/lore serve --config hack/integration/grafana/runlore.config.yaml --addr :8080
   ```

2. In another terminal, start Grafana:

   ```bash
   cd hack/integration/grafana
   GRAFANA_WEBHOOK_TOKEN=demo-token docker compose up
   ```

3. Watch the `lore serve` logs. Within ~20s of Grafana starting, the `HighCPU` rule's TestData
   query crosses its threshold and Grafana POSTs a `firing` alert to `/webhook/grafana`. Confirm:
   - the log line shows an admitted/investigating request
   - `namespace=payments`, `workload=api-0` (from `labels.namespace` / `labels.pod`)
   - the title is `HighCPU` (Grafana's auto-injected `alertname` label = the rule title)

4. ~20s later the TestData value drops back below threshold and Grafana POSTs a `resolved` alert
   for the same series. Confirm the log records a **resolution** (outcome ledger), not a second
   investigation.

5. `docker compose down` to stop Grafana; `Ctrl-C` the `lore serve` process.

## Verify by hand instead of waiting on the pulse

Grafana UI at `http://localhost:3000` (anonymous Admin access is enabled for this recipe) →
**Alerting → Alert rules → RunLore Demo → HighCPU** shows the rule's current state and evaluation
history. To control it deterministically rather than waiting on the pulse, edit the rule's query
(scenario `csv_content` with a fixed value above/below 50) and save — Grafana re-evaluates on the
next `interval` (10s).

## Notes

- `authorization_credentials: ${GRAFANA_WEBHOOK_TOKEN}` in `provisioning/alerting/contactpoints.yml`
  is expanded by Grafana from the container's own `GRAFANA_WEBHOOK_TOKEN` env var (set in
  `docker-compose.yml`) — keep it equal to the value exported for `lore serve` in step 1.
- `extra_hosts: host.docker.internal:host-gateway` in `docker-compose.yml` makes the container
  reach the host's `lore serve`; this is Linux Docker's equivalent of Docker Desktop's built-in
  `host.docker.internal`.
- This recipe was authored but has **not been run end to end** — the sandbox this was written in
  has no Docker daemon available (docker CLI present, `dockerd` not running, no passwordless sudo
  to start it). Treat it as a reviewed-on-paper recipe, not a verified one, until someone with a
  working Docker runs it.
