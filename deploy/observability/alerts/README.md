# RunLore alerting rules

Portable alerting rules for the RunLore SRE agent. RunLore is
metrics-backend-agnostic, so the same alert logic ships in two operator-native
formats. The two files have **identical** rule logic (same `expr`, `for`,
`severity`, annotations) — only `apiVersion`/`kind` differ. If you edit one,
edit the other.

## Which file for which operator

| File                 | Apply when you run...                                  | Kind                                                |
| -------------------- | ------------------------------------------------------ | --------------------------------------------------- |
| `prometheusrule.yaml`| Prometheus Operator / kube-prometheus-stack            | `monitoring.coreos.com/v1` `PrometheusRule`         |
| `vmrule.yaml`        | VictoriaMetrics Operator (VMAlert)                     | `operator.victoriametrics.com/v1beta1` `VMRule`     |

Apply only the one matching your stack:

```sh
# Prometheus Operator / kube-prometheus-stack
kubectl apply -f prometheusrule.yaml

# VictoriaMetrics Operator
kubectl apply -f vmrule.yaml
```

## How the operator discovers the rule

Both operators discover rule objects via a label selector, so the object must
carry labels the operator is watching for.

- **Prometheus Operator (kube-prometheus-stack):** the `Prometheus` CR has a
  `ruleSelector`. kube-prometheus-stack's default selector usually requires an
  extra label such as `release: <helm-release-name>` (e.g.
  `release: kube-prometheus-stack`). If the rule is not picked up, inspect the
  selector and add the matching label to `prometheusrule.yaml`:

  ```sh
  kubectl get prometheus -A -o jsonpath='{.items[*].spec.ruleSelector}'
  ```

  A commented `release:` label is left in `prometheusrule.yaml` for this.

- **VictoriaMetrics Operator:** the `VMAlert` CR has `ruleSelector` /
  `ruleNamespaceSelector`. Many deployments select all `VMRule` in watched
  namespaces, so no extra label is typically needed. Verify with:

  ```sh
  kubectl get vmalert -A -o jsonpath='{.items[*].spec.ruleSelector}'
  ```

Both files already carry `app.kubernetes.io/name: runlore` and
`role: alert-rules` for selector matching.

## Thresholds are starting points

Every threshold and `for:` duration in these rules is a **starting point**.
Tune them to your alert volume, model/provider latency profile, token budget,
and SLOs before relying on them in production. Each rule has a YAML comment
explaining its intent and what to tune.

## Alert inventory and metric dependencies

For the rules to evaluate, RunLore's `/metrics` endpoint must be scraped and the
listed series must be present.

| Alert                                    | Severity | Depends on metric(s)                                                            |
| ---------------------------------------- | -------- | ------------------------------------------------------------------------------ |
| RunloreAgentDown                         | critical | `runlore_build_info`                                                           |
| RunloreNoActiveLeader                    | critical | `runlore_leader`                                                               |
| RunloreMultipleLeaders                   | warning  | `runlore_leader`                                                               |
| RunloreInvestigationsDropped             | warning  | `runlore_investigations_dropped_total`                                        |
| RunloreInvestigationThrottlingSustained  | warning  | `runlore_investigations_throttled_total`                                      |
| RunlorePipelineStalled                   | critical | `runlore_alerts_received_total`, `runlore_investigations_started_total`        |
| RunloreInvestigationErrors               | warning  | `runlore_investigations_completed_total{result="error"}`                      |
| RunloreToolErrorRateHigh                 | warning  | `runlore_tool_calls_total{tool,result}`                                       |
| RunloreModelErrorRateHigh                | critical | `runlore_model_requests_total{provider,result}`                               |
| RunloreModelLatencyHigh                  | warning  | `runlore_model_request_duration_seconds_bucket`                               |
| RunloreSlowResolution                    | info     | `runlore_incident_resolution_seconds_bucket`                                  |
| RunloreInvestigationCostHigh             | warning  | `runlore_investigation_tokens_estimated_bucket`                               |

All metrics are exposed on RunLore's Prometheus `/metrics` endpoint. Histograms
additionally expose `_sum` and `_count` series alongside the `_bucket` series
used by the quantile rules.

## Runbooks

Each rule's `runbook_url` annotation points at
`https://github.com/Smana/runlore/blob/main/docs/observability.md#<anchor>`
(placeholders — the per-alert anchors live in that doc).
