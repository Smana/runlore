---
type: Playbook
title: TLS certificate approaching expiry without renewing
description: CertManagerCertExpirySoon fires on a certificate that is still Ready, meaning renewal is not happening in time; the Secret keeps the old material and TLS will fail cluster-wide at expiry unless renewal is unblocked.
tags: [certificates, cert-manager, tls, expiry, renewal, renewBefore, secret, ingress, webhook, CertManagerCertExpirySoon, CertManagerCertNotReady, CertManagerAbsent, KubeClientCertificateExpiration, KubeletClientCertificateExpiration]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`CertManagerCertExpirySoon` fires (cert-manager mixin, from
`certmanager_certificate_expiration_timestamp_seconds`) while the `Certificate` still reports
`READY=True`. Nothing is broken yet — which is exactly why this gets ignored until it becomes
an outage at a predictable moment.

The equivalent for the cluster's own PKI is `KubeletClientCertificateExpiration` /
`KubeClientCertificateExpiration`; those have a different remediation path (see below).

# Investigate

1. `kubectl -n <ns> describe certificate <name>` — read `Not After` and `Renewal Time`.
   If `Renewal Time` is in the past and the Secret has not changed, renewal is **attempted
   and failing**, not merely late.
2. Check for a renewal attempt in flight:
   `kubectl -n <ns> get certificaterequest,order,challenge`. A failing renewal produces the
   same object chain as a first issuance — and if it is failing, the ACME/order playbook
   applies.
3. Confirm which certificate is actually served, rather than trusting the object:
   `openssl s_client -connect <host>:443 -servername <host> </dev/null 2>/dev/null | openssl x509 -noout -dates`.
   A stale certificate served while the Secret is fresh means the consumer never reloaded.
4. Compare the Secret's material with the Certificate's status
   (`kubectl -n <ns> get secret <tls-secret> -o jsonpath='{.metadata.creationTimestamp}'` and
   its annotations). A Secret older than the last renewal means the write is not landing.
5. `kubectl -n cert-manager logs -l app.kubernetes.io/name=cert-manager --tail=200` and check
   `CertManagerAbsent` — a controller that is not running renews nothing, silently.
6. For manually managed certificates, there is no controller at all. Establish who owns the
   renewal before assuming automation exists.

# Common causes

- cert-manager is not running, is crash-looping, or lost RBAC to write the Secret. Every
  certificate in the cluster stops renewing at once, and the alerts arrive weeks apart as
  each one approaches its own expiry.
- Renewal is being attempted and failing for the same reasons a first issuance fails —
  DNS moved, the ACME path is blocked, credentials rotated.
- `renewBefore` too short relative to how long issuance takes, leaving no room for retries.
- The Secret is renewed correctly but the consumer never reloads it: an ingress controller,
  application, or webhook server that reads the certificate once at startup.
- The certificate is not managed by cert-manager at all — imported manually, or issued by an
  external process that stopped running.
- A webhook or API server client certificate approaching expiry, which is cluster PKI and
  not cert-manager's concern.

# Resolution

- Restore the controller first if cert-manager is down; that unblocks every certificate at
  once and is the highest-leverage action available.
- If renewal is failing on the ACME chain, resolve that failure — expiry is just the clock
  running out on it.
- Force a renewal once the cause is fixed (`cmctl renew <cert>` where available, or delete
  the `CertificateRequest` to trigger a new one). Verify the Secret's material actually
  changed rather than trusting the Ready condition.
- If the Secret is fresh but the served certificate is stale, restart or reload the consumer
  — and treat "does not reload certificates" as the real defect to fix.
- Raise `renewBefore` so there is slack for retries; renewing at the last moment turns any
  transient issuance failure into an outage.

# Not covered

- **Issuance that never succeeded.** A `Certificate` that was never Ready is the stuck
  order/challenge playbook, not this one.
- **Kubernetes control-plane and kubelet certificate rotation** — cluster PKI, a completely
  different lifecycle and toolchain, even though the alerts look adjacent.
- **Certificates outside the cluster** (load balancers, CDNs, managed cloud certificates)
  that no in-cluster controller can see.
- **Manually managed certificate material**, including where it came from and who renews it.
- **Application code that caches TLS material** — this entry can identify the symptom, not
  fix the reload path.
- **Choosing certificate lifetimes and `renewBefore` values** for a given CA's policy.
