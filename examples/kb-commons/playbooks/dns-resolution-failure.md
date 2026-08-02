---
type: Playbook
title: In-cluster DNS resolution failures via CoreDNS
description: Workloads fail to resolve service names - CoreDNSErrorsHigh, CoreDNSLatencyHigh, CoreDNSDown or CoreDNSForwardErrorsHigh fire - and applications log no such host or i/o timeout against cluster and external names alike.
tags: [dns, coredns, kube-dns, resolution, ndots, resolv.conf, forward, upstream, CoreDNSDown, CoreDNSErrorsHigh, CoreDNSLatencyHigh, CoreDNSForwardErrorsHigh, CoreDNSForwardLatencyHigh, KubePodNotReady, TargetDown]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

Applications log `no such host`, `SERVFAIL`, `Temporary failure in name resolution`, or
`i/o timeout` on DNS. The CoreDNS mixin alerts fire — `CoreDNSDown`, `CoreDNSErrorsHigh`,
`CoreDNSLatencyHigh`, `CoreDNSForwardErrorsHigh`, `CoreDNSForwardLatencyHigh`.

DNS failures rarely look like DNS failures at the workload: they present as readiness
failures, connection timeouts to dependencies, and 5xx across unrelated services at once.
**Breadth is the diagnostic signal** — many unrelated workloads degrading together points at
DNS or the network long before it points at any one application.

# Investigate

1. Reproduce from inside a pod, and test both a cluster name and an external one — the split
   tells you whether it is CoreDNS itself or its upstream:
   ```
   kubectl -n <ns> exec <pod> -- nslookup kubernetes.default.svc.cluster.local
   kubectl -n <ns> exec <pod> -- nslookup example.com
   ```
2. CoreDNS pod health: `kubectl -n kube-system get pods -l k8s-app=kube-dns -o wide`.
   Restarts, OOMKills, and pods concentrated on one node are all meaningful.
3. `kubectl -n kube-system logs -l k8s-app=kube-dns --tail=200` — look for
   `plugin/errors`, `i/o timeout` on forwards, and `Loop ... detected` (a forward loop makes
   CoreDNS crash-loop on startup).
4. Confirm the Service is backed: `kubectl -n kube-system get endpoints kube-dns`. Empty
   endpoints means every lookup in the cluster fails regardless of CoreDNS's own health.
5. `kubectl -n kube-system get configmap coredns -o yaml` — read the Corefile, especially
   the `forward . <upstream>` line and any `rewrite`/`hosts` stanzas. Check whether it was
   changed recently.
6. Check the client's `/etc/resolv.conf`
   (`kubectl -n <ns> exec <pod> -- cat /etc/resolv.conf`) — `ndots:5` is the default and
   means every unqualified external name is tried against the search domains first,
   multiplying query volume and latency.
7. Rule out a policy denial: a NetworkPolicy in the workload's namespace that omits UDP/TCP
   port 53 egress to kube-system blocks DNS for that namespace only.

# Common causes

- CoreDNS under-replicated or CPU-throttled for the cluster's query rate; latency rises
  before errors do.
- Upstream resolver unreachable or slow, so external names fail while cluster names work.
- A Corefile change — wrong forward target, a `hosts` or `rewrite` stanza, or a
  configuration that creates a resolution loop.
- NetworkPolicy denying egress to port 53 in a namespace that recently adopted a default
  deny policy. Only that namespace breaks, which makes it look application-specific.
- Node-local DNS cache (when deployed) unhealthy on specific nodes — failures follow node
  placement, not workload.
- `ndots:5` plus a hot external hostname producing several failed lookups per real one,
  saturating CoreDNS.
- CoreDNS pods OOMKilled under query load, so the failure is intermittent and correlates
  with traffic.

# Resolution

- **Restore capacity first** if CoreDNS is saturated or crash-looping: scale replicas up,
  raise its memory limit, spread it across nodes. Reversible and fast.
- **Revert a recent Corefile change** in Git if the timeline points there; a loop or bad
  forward target is a config bug, not a capacity problem.
- Add the missing port-53 egress rule when a NetworkPolicy is the cause. Every default-deny
  namespace needs an explicit DNS egress rule.
- For `ndots` amplification, set `dnsConfig.options` `ndots: 1` on the noisy workload, or
  fully qualify hot external names with a trailing dot.
- Fix an unhealthy upstream resolver or node-local cache; those are infrastructure
  incidents, and the cluster is only reporting them.

# Not covered

- **External DNS records and public zones** — records created by external-dns, registrar
  delegation, and propagation. A missing public record is not an in-cluster DNS failure.
- **Service discovery when the Service has no endpoints.** The name resolves fine and the
  connection fails; that is the no-endpoints playbook.
- **NetworkPolicy denial generally** — this entry covers only the port-53 case.
- **Service mesh DNS interception** and mesh-specific resolution paths.
- **Node-level `/etc/resolv.conf` and systemd-resolved** configuration on the host.
- Sizing CoreDNS for a specific cluster; the numbers depend on query rate.
