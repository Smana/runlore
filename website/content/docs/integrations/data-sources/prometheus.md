---
title: Prometheus / VictoriaMetrics
weight: 301
integration: {kind: metrics, id: prometheus}
---

**What it gives you** — the `query_metrics` and `query_metrics_range` tools: PromQL against any
backend that speaks the Prometheus HTTP API, including VictoriaMetrics. Plus `alert_rule`, which
reads the firing alert's own rule expression so a threshold alert is judged against the series it
actually thresholds.

## Minimal config

```yaml
metrics:
  url: http://vmsingle.observability.svc:8429
```

Pinning the flavor instead of auto-detecting it:

```yaml
metrics:
  url: http://prometheus.observability.svc:9090
  flavor: prometheus       # or "victoriametrics" — optional, auto-detected when omitted
```

## Verify it locally

```bash
curl -s "http://vmsingle.observability.svc:8429/api/v1/query?query=up" | jq .status
```

Then fire a test incident and confirm `query_metrics` appears among the tools the model called:

```bash
kubectl -n runlore logs deploy/runlore | grep 'tool=query_metrics'
```

## Notes

- **Presence enables it** — `metrics.url` set is all it takes; an unset URL leaves the tool
  unregistered, no error.
- Both Prometheus and VictoriaMetrics speak the same Prometheus HTTP API; VictoriaMetrics **also**
  accepts MetricsQL, a PromQL superset. `metrics.flavor` unlocks MetricsQL-only query guidance for the
  model — it is auto-detected at startup (`DetectFlavor` probes `/api/v1/status/buildinfo`) and fails
  safe to generic Prometheus behaviour on an ambiguous or failed probe, so the model is never told to
  use a dialect the backend might reject. Pin `flavor:` explicitly when the backend sits behind a
  proxy that confuses the probe.
- **`alert_rule` reads the rule, so the model stops guessing which series to check.** A threshold
  alert names a metric *and* a statistic, and the two are easy to confuse: an alert on
  `aws_rds_write_latency_maximum > 0.050` is not answered by querying the `_average` series, which
  can sit near zero while the maximum spikes. The tool fetches the rule's own `expr` from
  `/api/v1/rules` so the investigation reads the right series first. It also surfaces the rule's
  `health` and `lastError` — a rule that is not evaluating looks identical to a healthy metric
  sitting at zero.
- **It degrades, never fails.** A backend with no rules endpoint, an HTTP error, an empty ruleset,
  or an alertname with no matching rule all return an "unavailable" string rather than an error, so
  a missing rules API can never abort an investigation or be misread as "this alert has no rule".
  When the name does not match, the reply lists the alertnames the backend *does* define, with the
  closest matches first. Because a string is a *successful* tool call, each degraded outcome is
  counted in `runlore_alert_rule_degraded_total` — labelled `systemic` (no rules endpoint, a failing
  read, an empty ruleset: this deployment has lost the tool entirely) or `routine` (this alertname
  has no rule here). See
  [Observability]({{< relref "/docs/operations/observability.md" >}}) for the alert recipe.
- `token_env` (bearer auth) and `headers` (e.g. `X-Scope-OrgID` for a multi-tenant backend) are
  available like every other data-source endpoint. `headers` values are **not secret-safe over plain
  HTTP** — use `https://` for a public host, or keep secrets in `token_env` only.

## Reference

- [Configuration → Other top-level keys]({{< relref "/docs/configuration/configuration.md#other-top-level-keys" >}})
  for the full `metrics` key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the provider table across every
  signal.
