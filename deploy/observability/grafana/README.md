# RunLore — Grafana dashboard

`runlore.json` is a production-grade, **portable** Grafana dashboard for the RunLore SRE agent.

## What it shows

Panels are grouped into collapsible rows:

| Row | Panels |
| --- | --- |
| **🧠 Learning loop — is it working?** | Recall fire-rate; recall precision; recall resolve-rate; recall token savings; invalid KB entries |
| **🔎 Retrieve — instant recall** | Recall outcome by verify result; rejections by reason; recall vs fresh answer mix; BM25 recall-score heatmap |
| **🧪 Capture & trust — outcome ledger** | Outcomes opened + incidents resolved; incident resolution time p50/p95; recall outcome by result (pie) |
| **📝 Curate — knowledge growth** | Curation rate by kind & result; curation dedup-score heatmap |
| **💸 Cost & efficiency** | Tokens per investigation, full loop vs recall short-circuit (the gap is the saving); per-investigation cost p50/p95 (USD); model cache hit ratio |
| **🔬 Investigations** | Started/throttled/dropped rates; duration p50/p95/p99; completion rate by result |
| **🛠 Tools & model** | Tool call rate by tool; tool error ratio; tool latency p95 by tool; model request rate by provider; model error ratio; model latency p95 by provider; output truncation rate |
| **📥 Alert intake** | Received / coalesced / suppressed alert rates; coalesced batch-size heatmap |
| **🖥 Fleet** | Agent version (`runlore_build_info`), Active leaders (`sum(runlore_leader)`, green at exactly 1), Replicas reporting (`count(runlore_build_info)`) |

Units follow conventions: rates use `ops`/`reqps`, durations use `s`, ratios use `percentunit`, token counts use `short`.

## Portability — works on Prometheus **and** VictoriaMetrics

The dashboard has a single templating variable, `datasource`, of type `datasource` with query `prometheus`. VictoriaMetrics registers in Grafana as a Prometheus-type datasource, so the same variable selects either backend.

Every panel and every query target references `{"type": "prometheus", "uid": "${datasource}"}` — there are **no hardcoded datasource UIDs**. All `rate()` queries use `$__rate_interval`, so the dashboard adapts to the dashboard's resolution and the scrape interval automatically. The panel models target Grafana 10/11 (`schemaVersion: 39`).

Pick your Prometheus or VictoriaMetrics datasource in the top-left `Data source` dropdown after import — nothing else needs editing.

## How to import

### Grafana UI

1. Grafana → **Dashboards** → **New** → **Import**.
2. **Upload JSON file** and select `runlore.json` (or paste its contents).
3. When prompted, choose your Prometheus / VictoriaMetrics datasource for the `datasource` variable.
4. Import.

### Provisioning (file-based)

Place `runlore.json` where a dashboard provider points, e.g.:

```yaml
# /etc/grafana/provisioning/dashboards/runlore.yaml
apiVersion: 1
providers:
  - name: runlore
    orgId: 1
    folder: RunLore
    type: file
    disableDeletion: false
    editable: true
    options:
      path: /var/lib/grafana/dashboards/runlore
```

Copy `runlore.json` into that `path` and restart / reload Grafana. The `datasource` variable defaults to the configured default Prometheus-type datasource.

## Requirements

- The RunLore agent's `/metrics` endpoint must be scraped by your Prometheus or VictoriaMetrics instance (the agent exposes the `runlore_*` series this dashboard queries).
- Grafana 10 or 11.
