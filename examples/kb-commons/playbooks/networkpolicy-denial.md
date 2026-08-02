---
type: Playbook
title: Traffic silently dropped by a NetworkPolicy
description: Connections between pods or to external endpoints hang and time out with no rejection, because a NetworkPolicy denies them; readiness probes fail and dependent services report timeouts rather than connection refused.
tags: [network, networkpolicy, cni, cilium, calico, egress, ingress, deny, timeout, connectivity, KubePodNotReady, TargetDown, KubeDeploymentReplicasMismatch, default-deny]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

A workload cannot reach a dependency. The tell is the **failure mode**: connections **hang
and time out** rather than being refused. `connection refused` means something answered and
said no (nothing listening, or a proxy rejecting); `i/o timeout` / `context deadline
exceeded` with a resolvable name means packets are being dropped — a policy, a firewall, or
a missing route.

There is no upstream alert for a policy denial. It surfaces as `KubePodNotReady` (readiness
probes that call the dependency), `TargetDown` (Prometheus cannot scrape the target), or
application 5xx. A CNI that exports drop metrics is the exception — if yours does, that is
the fastest signal available.

The strongest circumstantial evidence: it broke when **nothing in the application changed**,
and it broke for a whole namespace at once.

# Investigate

1. List policies that select the source and destination pods:
   `kubectl -n <ns> get networkpolicy` and
   `kubectl -n <ns> describe networkpolicy <name>`.
   Remember the semantics: **a pod selected by any policy is default-deny for that
   direction**. Adding one ingress policy to a namespace silently denies all other ingress
   to the selected pods.
2. Check both ends. Egress must be allowed on the client **and** ingress allowed on the
   server. A one-sided rule fails exactly like no rule.
3. Test connectivity directly, bypassing the application:
   `kubectl -n <ns> exec <client-pod> -- nc -zv <service>.<ns>.svc.cluster.local <port>`
   (or `wget -T3 -qO-`). Compare with the same test from a pod in a namespace with no
   policies — if that works, the policy is confirmed.
4. Verify DNS separately. A default-deny egress policy without a port-53 rule breaks name
   resolution first, and every symptom after that is misleading.
5. Use the CNI's own tooling when available — most CNIs can report which policy verdict
   applied to a flow. That converts guesswork into a definite answer.
6. Check the label selectors actually match:
   `kubectl -n <ns> get pods --show-labels` against the policy's `podSelector`. A renamed
   label leaves a policy selecting nothing (allowing everything) or selecting the wrong pods
   (denying everything).
7. For cross-namespace traffic, confirm the `namespaceSelector` matches the source
   namespace's **labels**, not its name — unless the cluster sets the
   `kubernetes.io/metadata.name` label, which most modern versions do.

# Common causes

- A namespace adopted a default-deny policy and the allow rules for existing dependencies
  were never written. Everything breaks at once, including metrics scraping.
- Egress allowed to the application dependency but not to DNS (port 53, UDP and TCP).
- Missing ingress rule for the monitoring namespace, so scrapes fail and `TargetDown` fires
  while the application is perfectly healthy.
- `namespaceSelector` matching a label the source namespace does not carry.
- A pod label rename that silently changes which pods a policy selects.
- Policy allows the Service port but the traffic actually lands on a different
  `targetPort` — policies match pod ports, not service ports.
- A CNI that does not enforce NetworkPolicy at all, so policies exist but nothing is denied
  — the opposite failure, and worth ruling out before trusting any policy.

# Resolution

- Add the missing allow rule in Git and reconcile. Be specific: name the port and the
  selector rather than widening to allow-all.
- Every default-deny namespace needs, at minimum: DNS egress to kube-system on port 53
  (UDP+TCP), ingress from the monitoring namespace on the metrics port, and explicit rules
  for each dependency.
- **Do not delete the policy to unblock traffic.** Deleting it removes a security control
  that was added deliberately, and "temporarily" deleted policies are rarely restored. Add
  the narrow rule instead.
- If probes are failing, confirm the kubelet's probe path is not policy-affected — node-to-
  pod probe traffic is treated differently by different CNIs.

# Not covered

- **CNI-specific policy CRDs** (CiliumNetworkPolicy, Calico GlobalNetworkPolicy, mesh
  authorization policies). Those add layers that plain `kubectl get networkpolicy` does not
  show — check for them, but their semantics are outside this entry.
- **Whether your CNI enforces NetworkPolicy at all**, and how to verify that.
- **Cloud firewalls, security groups, and node-level rules** outside the cluster.
- **DNS failures generally** — only the port-53 policy case is covered here.
- **Service mesh mTLS and authorization**, which drop traffic for entirely different reasons
  and report them differently.
- **Designing a namespace's policy set** from scratch.
