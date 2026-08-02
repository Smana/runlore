---
type: Playbook
title: cert-manager Certificate not ready with a stuck ACME order or challenge
description: CertManagerCertNotReady fires because an ACME Order stays pending and its Challenge never validates, so no Secret is issued and the Ingress serves a default or expired certificate.
tags: [certificates, cert-manager, acme, letsencrypt, order, challenge, issuer, clusterissuer, http01, dns01, ingress, tls, CertManagerCertNotReady, CertManagerHittingRateLimits, CertManagerAbsent, CertManagerCertExpirySoon]
timestamp: "2026-08-02"
status: active
last_validated: "2026-08-02"
---

# Symptom

`CertManagerCertNotReady` fires (cert-manager mixin). `kubectl get certificate -A` shows
`READY=False`. Clients see a browser TLS warning or a wrong-certificate error because the
Ingress falls back to a default certificate, and the target Secret either does not exist or
still holds the previous one.

cert-manager's object chain is `Certificate → CertificateRequest → Order → Challenge`.
**The useful error is always at the deepest object that exists**, not on the Certificate.

# Investigate

1. Walk the chain down, in this order:
   ```
   kubectl -n <ns> describe certificate <name>
   kubectl -n <ns> describe certificaterequest    # the one owned by that certificate
   kubectl -n <ns> describe order
   kubectl -n <ns> describe challenge
   ```
   Stop at the deepest object present and read its status message — that is the diagnosis.
2. If there is **no Order**, the failure is before ACME: a missing/misnamed Issuer or
   ClusterIssuer, or an issuer that is itself not Ready
   (`kubectl describe clusterissuer <name>`).
3. For an `HTTP01` challenge: cert-manager creates a temporary solver pod and Ingress at
   `/.well-known/acme-challenge/<token>`. Verify the path is reachable **from the public
   internet** — not from inside the cluster. Common blockers: the Ingress forces an HTTPS
   redirect on that path, an auth middleware protects it, or the hostname does not resolve
   publicly to the ingress.
4. For a `DNS01` challenge: check the `_acme-challenge.<domain>` TXT record actually exists
   in the authoritative zone, and that the solver's credentials have permission to write it.
   Propagation delay is normal; hours of `pending` is not.
5. `kubectl -n cert-manager logs -l app.kubernetes.io/name=cert-manager --tail=200` — the
   controller logs the ACME server's response verbatim, including rate-limit and CAA errors.
6. Check `CertManagerHittingRateLimits`. A reconcile loop that recreates Orders will exhaust
   the ACME provider's issuance limits, and then **nothing** will succeed for that domain
   for a long window regardless of what you fix.

# Common causes

- Public DNS for the requested name does not point at the ingress (or does not exist yet),
  so `HTTP01` validation cannot reach the solver.
- An Ingress-level HTTPS redirect, IP allow-list, or auth annotation intercepts the
  `/.well-known/acme-challenge/` path.
- Wrong or missing `issuerRef` — a namespaced `Issuer` referenced as a `ClusterIssuer`, or a
  name typo. No Order is created at all.
- `DNS01` solver credentials lacking write access to the zone, or the wrong zone/provider
  configured.
- ACME rate limits already exhausted by repeated failed attempts.
- A CAA record on the domain forbidding the chosen CA.
- Requesting a certificate for a domain the cluster does not actually own.

# Resolution

- Fix what the deepest object reported — publish the DNS record, exempt the ACME path from
  the redirect/auth, correct the `issuerRef`, or grant the DNS credentials.
- **Use the staging ACME endpoint while iterating.** Staging has far looser rate limits, and
  a debugging loop against production issuance is how a fixable problem becomes a
  multi-hour one.
- Delete the failed `Order` (or the `CertificateRequest`) to force a fresh attempt **after**
  fixing the cause. Deleting before fixing just consumes rate limit.
- If rate-limited, stop retrying and wait out the window. Nothing else works, and continued
  attempts extend it.

# Not covered

- **Certificates that issued fine but are approaching expiry without renewing** —
  `CertManagerCertExpirySoon` on a Ready certificate is a different situation.
- **cert-manager itself being down** (`CertManagerAbsent`), which stops all issuance.
- **Non-ACME issuers** — Vault, Venafi, CA, and SelfSigned issuers have entirely different
  failure modes and no Order/Challenge objects.
- **DNS zone administration and registrar configuration.**
- **Ingress controller TLS termination and SNI**, and how a specific controller picks a
  default certificate.
- **Choosing between HTTP01 and DNS01** for a given topology, and wildcard-certificate
  requirements.
