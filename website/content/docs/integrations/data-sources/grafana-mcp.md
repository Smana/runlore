---
title: Grafana (via MCP)
weight: 313
integration: {kind: mcp, id: grafana-mcp}
---

**What it gives you** — RunLore reads your metrics, logs, cluster and Git. It does not read what
your team has already written down *about* them in Grafana: the deploy annotation from four minutes
ago, the incident someone already declared, who is on call to act on the fix. This wires that in,
through the [MCP client]({{< relref "/docs/integrations/data-sources/mcp.md" >}}) with
[`grafana/mcp-grafana`](https://github.com/grafana/mcp-grafana) — no Go code, no fork.

## What it actually changes

Take the Harbor incident from [`hack/demo.sh`](https://github.com/Smana/runlore#-try-it-in-one-minute--no-cluster-no-keys).
Without Grafana wired in, RunLore reaches the cause from raw signal — metrics, logs, and the Flux
revision diff:

> Database migration deadlock preventing `harbor-db` from starting. Chart bump to 1.15.0 enabled
> schema migrations. Confidence 90%.

With Grafana wired in, the same investigation also sees what your organisation recorded *around*
that failure (illustrative):

> …plus: an annotation marks a `harbor` deploy at 14:02, three minutes before the alert fired.
> Grafana Incident #482 is already open against this service. The platform rota is primary on-call.

The root cause didn't change. What changed is the **evidence** behind it and what you do next — and
two of those facts are ones RunLore structurally cannot get from a cluster:

- **An annotation is a change that never passed through GitOps.** RunLore's *what changed?* spine is
  a Git revision diff, which is exact but blind to anything applied outside it — a manual dashboard
  edit, a feature flag, a maintenance window. Annotations are where teams already record those.
- **A declared incident means someone is already on it.** Knowing that turns "action required" into
  "join the thread", which is a different next step for whoever reads the card.

That is the test worth applying to any MCP server before you wire it: *does it tell RunLore
something the cluster cannot?* If the answer is no, it costs turns and adds nothing.

## The general pattern

This is the worked example for a broader claim: **any vendor RunLore doesn't natively speak, an MCP
server closes.** If you run Datadog, Splunk or New Relic instead, the shape below is identical —
only the image and the tool names change.

> [!NOTE]
> Looking for Grafana as a *trigger* rather than a data source? That is
> [Grafana Alerting]({{< relref "/docs/integrations/triggers/grafana.md" >}}) — a first-class
> webhook source, no MCP involved. The two are independent and compose.

## Don't duplicate what's already native

`mcp-grafana` exposes `query_prometheus` and `query_loki_logs`. **Leave both out of the allowlist.**

RunLore already reads [Prometheus/VictoriaMetrics]({{< relref
"/docs/integrations/data-sources/prometheus.md" >}}) and [Loki]({{< relref
"/docs/integrations/data-sources/loki.md" >}}) through native providers that the investigation loop
and the eval suite are built around. Registering a second path to the same data gives the model two
tools for one job — it pays extra turns choosing between them, and the evidence trail gets harder to
read for no added signal.

Wire the native providers for metrics and logs. Use MCP for what has no native equivalent:

| What it answers | Grafana tools |
|---|---|
| *Did something change that GitOps can't see?* — deploy markers, maintenance windows, feature flags | `get_annotations` |
| *Is someone already working this?* — avoids a second opinion on a declared incident | `list_incidents`, `get_incident` |
| *Who acts on this?* — turns a suggested next step into a routed one | `list_oncall_schedules`, `get_current_oncall_users` |
| *What is normal for this service?* — the panels your team already trusts, rather than a threshold RunLore guessed | `search_dashboards`, `get_dashboard_by_uid`, `get_dashboard_summary` |
| *Has Grafana already analysed this?* — Sift's own findings, as corroboration or contradiction | `get_sift_investigation`, `get_sift_analysis`, `find_error_pattern_logs` |
| *What is actually wired here?* — when not every datasource is reachable from the cluster | `list_datasources`, `get_datasource_by_uid` |

## Deploy the MCP server

RunLore's MCP client speaks **streamable HTTP only** — there is no stdio client transport, because
RunLore runs in-cluster and remote servers are network services. So `mcp-grafana` runs as a
Deployment with `-t streamable-http`, not as a subprocess.

<!-- docsguard:ignore Kubernetes manifests for the MCP server, not a runlore.yaml -->
```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: mcp-grafana
  namespace: runlore
spec:
  replicas: 1
  selector:
    matchLabels: {app: mcp-grafana}
  template:
    metadata:
      labels: {app: mcp-grafana}
    spec:
      containers:
        - name: mcp-grafana
          image: grafana/mcp-grafana:latest   # pin a digest in production
          # `command` REPLACES the image entrypoint on purpose. The published
          # entrypoint bakes in `--transport sse --address 0.0.0.0:8000`, and a
          # Kubernetes `args:` replaces CMD, not ENTRYPOINT — so passing the
          # transport as args alone appends it and you end up starting with both
          # `--transport sse` and `--transport streamable-http`. Replacing the
          # command means the flags below are the whole command line, which also
          # means `--address` has to be repeated here.
          #
          # --disable-write drops every mutating tool server-side. RunLore's own
          # allowlist below already prevents them being called; this makes it
          # true even if that allowlist is later widened by mistake.
          command: ["/app/mcp-grafana"]
          args:
            - --transport=streamable-http
            - --address=0.0.0.0:8000
            - --disable-write
          ports:
            - {name: http, containerPort: 8000}
          env:
            - name: GRAFANA_URL
              value: https://your-org.grafana.net
            - name: GRAFANA_SERVICE_ACCOUNT_TOKEN
              valueFrom:
                secretKeyRef: {name: mcp-grafana, key: token}
          readinessProbe:
            httpGet: {path: /healthz, port: http}
          securityContext:
            allowPrivilegeEscalation: false
            readOnlyRootFilesystem: true
            runAsNonRoot: true
            # runAsUser is REQUIRED alongside runAsNonRoot here. The image declares
            # `USER mcp-grafana` — a name, not a uid — and the kubelet cannot prove a
            # named user is non-root, so it refuses to start the container with
            # "container has runAsNonRoot and image has non-numeric user". 1000 is the
            # uid that name resolves to in the published image.
            runAsUser: 1000
            capabilities: {drop: ["ALL"]}
---
apiVersion: v1
kind: Service
metadata:
  name: mcp-grafana
  namespace: runlore
spec:
  selector: {app: mcp-grafana}
  ports:
    - {name: http, port: 8000, targetPort: http}
```

Give the Grafana **service account** the narrowest role that covers the tools you allowlist — Viewer
is enough for dashboards, datasources and annotations. Incident, OnCall and Sift tools need their
own scopes; grant only the ones you actually list.

## Wire it into RunLore

```yaml
mcp:
  require_allowlist: true          # refuse startup unless every server declares an allowlist
  servers:
    - name: grafana                # tools register as grafana__<tool>
      url: http://mcp-grafana.runlore.svc.cluster.local:8000/mcp
      tools:
        - search_dashboards
        - get_dashboard_by_uid
        - get_dashboard_summary
        - list_incidents
        - list_oncall_schedules
        - get_current_oncall_users
        - get_annotations
        - list_datasources
```

`require_allowlist: true` is deliberate. It is deny-by-default across *every* MCP server, so adding a
second one later cannot silently register its full toolset. The allowlist is enforced at discovery —
an unlisted tool is never registered, so the model cannot call it even if it tries.

No `token_env` here: the server is reached over in-cluster `http://`, and it holds the Grafana
credential itself rather than passing yours through.

If you do put a token on it, note where the guard actually bites. RunLore rejects a token over
plaintext `http://` **to a public host** only — an in-cluster address is treated as private and
allowed, which is why the `…svc.cluster.local` URL above needs no TLS. Private means a loopback or
[RFC 1918](https://datatracker.ietf.org/doc/html/rfc1918) address, `localhost`, a single-label
service name, or a host ending in `.svc`, `.cluster.local`, `.local` or `.internal`. Expose
`mcp-grafana` on anything outside that set and you must terminate TLS and use `https://` before
adding `token_env` — startup fails otherwise.

## If you run the chart's NetworkPolicy, open port 8000

This is the step most likely to make the recipe look like it silently did nothing.
`networkPolicy.enabled` renders `policyTypes: [Ingress, Egress]`, and **neither egress mode lets
this through on its own**: the permissive default opens only 443 and 6443, and `strict: true`
denies by default and allows only what you declare. `mcp-grafana` listens on **8000**, so it is
blocked either way, in the same namespace or not.

Add it via `extraEgress`, which is appended verbatim in both modes:

<!-- docsguard:ignore Helm chart values, not a runlore.yaml -->
```yaml
networkPolicy:
  egress:
    extraEgress:
      - to:
          - podSelector:
              matchLabels:
                app: mcp-grafana
        ports:
          - protocol: TCP
            port: 8000
```

Add a `namespaceSelector` alongside the `podSelector` if you put `mcp-grafana` in a different
namespace from RunLore. And remember `mcp-grafana` needs its *own* egress to reach Grafana — a
policy selecting it, not RunLore, if your cluster defaults to deny.

## Verify it

Discovery happens once at startup, per server:

```bash
kubectl -n runlore logs deploy/runlore | grep -E 'mcp server|grafana__'
```

You should see the server registered and exactly the tools you allowlisted — no more. Then fire a
test incident and confirm the model actually reached for one:

```bash
kubectl -n runlore logs deploy/runlore | grep 'tool=grafana__'
```

If the server is unreachable, RunLore logs it at Warn and **starts anyway** with the tools it does
have. That is deliberate — a broken MCP server degrades an investigation, it never blocks one — but
it does mean a silent typo in `url` looks like "the model just didn't use it". Check the startup log
before concluding the tools aren't helping.

## Notes

- **External tools are read-only to RunLore.** Remote tools are marked external and
  untrusted-output in the system prompt; they inform an investigation and can never perform an
  action, because the action gate only knows RunLore's built-in operations. Note what that does
  *not* say: the gate constrains RunLore's write path, not the remote server's. A remote tool with
  server-side side effects is prevented only by the allowlist — which is why `--disable-write` on
  the server is worth setting as well.
- **Output is untrusted data.** Tool results go through the same redaction path as every other
  tool, and are never followed as instructions.
- **Namespacing.** Every tool registers as `grafana__<tool>`; built-in names always win a collision.
- **No retries.** Remote calls are not assumed idempotent, so a failed call is not retried.
- **Not the same as a first-party integration.** This is a real gap-closer, not parity with a
  maintained native provider — see the [HolmesGPT comparison]({{< relref "/compare/holmesgpt.md"
  >}}), which says so plainly.
