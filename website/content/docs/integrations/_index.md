---
title: Integrations
weight: 15
---

Everything RunLore plugs into: what can **trigger** an investigation, which **LLMs** run it, the
**data sources** it reads for signal, where **notifications** land, and the Git **forge** it writes
findings back to. Each page below is short by design — the minimal config to wire the integration,
how to verify it locally, and the gotchas that would otherwise cost you a debugging session.

**Presence is enablement.** A key under `sources.<name>` — or the equivalent notifier / data-source
block — is what turns that integration on; there is no separate `enabled: true` flag to hunt for.
Leave a data source out and RunLore simply runs without the tool it would have unlocked: no source,
notifier, or data source is required, and none is assumed. See
[Configuration]({{< relref "/docs/configuration/configuration.md" >}}) for the exhaustive key
reference every page here deep-links into.

## Triggers

What starts an investigation.

{{< hextra/feature-grid cols="4" >}}
  {{< hextra/feature-card link="triggers/alertmanager/" icon="bell" title="Alertmanager"
    subtitle="Prometheus/VMAlert webhook — the primary trigger." >}}
  {{< hextra/feature-card link="triggers/gitops/" icon="cube-transparent" title="GitOps failures"
    subtitle="React to Flux or Argo CD Ready=False, not just alerts." >}}
  {{< hextra/feature-card link="triggers/pagerduty/" icon="shield-exclamation" title="PagerDuty"
    subtitle="Trigger investigations from PagerDuty incidents." >}}
  {{< hextra/feature-card link="triggers/grafana/" icon="bell" title="Grafana Alerting"
    subtitle="Grafana's own alert webhook, as a first-class source." >}}
  {{< hextra/feature-card link="triggers/custom-webhook/" icon="puzzle" title="Custom webhook"
    subtitle="Map any vendor's alert JSON with dot-path field extraction — no code." >}}
{{< /hextra/feature-grid >}}

## LLM providers

What runs the investigation loop.

{{< hextra/feature-grid cols="4" >}}
  {{< hextra/feature-card link="llm/openai-compatible/" icon="chip" title="OpenAI-compatible"
    subtitle="vLLM, Ollama, OpenAI, OpenRouter — any endpoint that supports forced tool_choice." >}}
  {{< hextra/feature-card link="llm/anthropic/" icon="chip" title="Anthropic"
    subtitle="Native Claude, keyed or in-cluster via a compatible gateway." >}}
  {{< hextra/feature-card link="llm/gemini/" icon="chip" title="Gemini"
    subtitle="Google's models over the OpenAI-compatible endpoint — version caveats apply." >}}
  {{< hextra/feature-card link="llm/local-keyless/" icon="terminal" title="Local / keyless"
    subtitle="In-cluster vLLM or Ollama — no API key, no egress." >}}
{{< /hextra/feature-grid >}}

## Data sources

What the agent reads for signal — every one is optional; an unset data source just disables the tool
it would have unlocked.

{{< hextra/feature-grid cols="3" >}}
  {{< hextra/feature-card link="data-sources/prometheus/" icon="database" title="Prometheus / VictoriaMetrics"
    subtitle="PromQL metrics — the query_metrics tools." >}}
  {{< hextra/feature-card link="data-sources/victorialogs/" icon="database" title="VictoriaLogs"
    subtitle="LogsQL — the default logs backend." >}}
  {{< hextra/feature-card link="data-sources/loki/" icon="database" title="Grafana Loki"
    subtitle="LogQL, auto-detected at startup." >}}
  {{< hextra/feature-card link="data-sources/elasticsearch/" icon="database" title="Elasticsearch"
    subtitle="ECS-shaped logs over the _search API." >}}
  {{< hextra/feature-card link="data-sources/opensearch/" icon="database" title="OpenSearch"
    subtitle="The AWS-managed fork, same query path." >}}
  {{< hextra/feature-card link="data-sources/kubernetes/" icon="cube" title="Kubernetes"
    subtitle="Pod status, events, controller and pod logs — client-go, in-cluster." >}}
  {{< hextra/feature-card link="data-sources/hubble/" icon="globe" title="Cilium Hubble"
    subtitle="eBPF flow visibility with rich drop reasons." >}}
  {{< hextra/feature-card link="data-sources/aws-vpc-flow-logs/" icon="cloud" title="AWS VPC Flow Logs"
    subtitle="Network drops on any AWS VPC, Cilium or not." >}}
  {{< hextra/feature-card link="data-sources/gcp-firewall-logs/" icon="cloud" title="GCP Firewall Logs"
    subtitle="DENIED connections on any GCP VPC, including GKE." >}}
  {{< hextra/feature-card link="data-sources/aws-cloud/" icon="cloud-download" title="AWS cloud control plane"
    subtitle="CloudTrail + EC2/ASG/EKS — what changed outside GitOps." >}}
  {{< hextra/feature-card link="data-sources/gcp-cloud/" icon="cloud-download" title="GCP cloud control plane"
    subtitle="Cloud Audit Logs + GKE/MIG/Compute. Not yet verified on a live cluster." >}}
  {{< hextra/feature-card link="data-sources/source-repos/" icon="link" title="Source repos"
    subtitle="Turns a manifest bump into the actual code diff behind it." >}}
  {{< hextra/feature-card link="data-sources/mcp/" icon="puzzle" title="MCP"
    subtitle="Extend the agent's own toolbox with remote MCP tools — no Go required." >}}
  {{< hextra/feature-card link="data-sources/grafana-mcp/" icon="chart-square-bar" title="Grafana (via MCP)"
    subtitle="Deploy annotations GitOps can't see, and whether an incident is already open — a worked MCP example." >}}
{{< /hextra/feature-grid >}}

## Notifications

Where findings are delivered.

{{< hextra/feature-grid cols="4" >}}
  {{< hextra/feature-card link="notifications/slack/" icon="chat" title="Slack"
    subtitle="Incoming webhook or bot token, with optional Approve/Reject buttons." >}}
  {{< hextra/feature-card link="notifications/matrix/" icon="chat-alt" title="Matrix"
    subtitle="Deliver to any Matrix room via an access token." >}}
  {{< hextra/feature-card link="notifications/webhook/" icon="link" title="Webhook"
    subtitle="Generic outgoing webhook — plug in anything that takes JSON." >}}
  {{< hextra/feature-card link="notifications/templated/" icon="chat-alt-2" title="Templated"
    subtitle="Named instances with your own Go-template payload, e.g. Microsoft Teams." >}}
{{< /hextra/feature-grid >}}

## Forge

Where the Learn loop writes findings back to.

{{< hextra/feature-grid cols="4" >}}
  {{< hextra/feature-card link="forge/github/" icon="external-link" title="GitHub"
    subtitle="A scoped GitHub App drafts PRs and issues against your KB repo." >}}
  {{< hextra/feature-card link="forge/gitlab/" icon="external-link" title="GitLab"
    subtitle="A project or group access token drafts merge requests — self-hosted or gitlab.com." >}}
{{< /hextra/feature-grid >}}
