---
title: GitOps failures
weight: 102
integration: {kind: source, id: gitops}
---

**What it gives you** — investigations triggered by Flux `Kustomization`/`HelmRelease` or Argo CD
`Application` going `Ready=False`, not just alerts.

## Minimal config

```yaml
gitops:
  engine: flux          # or "argocd"
sources:
  gitops:
    enabled: true        # also react to Flux/Argo CD Ready=False
triggers:
  gitops_failures:
    debounce: 60s          # require a failure to persist this long before investigating
```

## Verify it locally

Break a `Kustomization` (or `Application`) on a real or local (k3d/kind) cluster and watch it get
picked up:

```bash
flux suspend kustomization <name>   # or edit it to reference a bad image/values

kubectl -n runlore logs deploy/runlore | grep -E 'watching gitops failures|msg=incident'
```

Expected: a `watching gitops failures` line at startup, then `msg=incident … investigate=true` once
the resource sits `Ready=False` past the debounce window.

## Notes

- **Presence + `enabled: true` both gate it** — `sources.gitops` must be present *and*
  `enabled: true`; a missing key or `enabled: false` disables the watcher entirely.
- Cascade failures (a child resource failing only because its parent already did) are dropped before
  they ever reach the trigger policy — only the root failure investigates.
- `gitops_failures.debounce` (default **60s**) requires the failure to persist before investigating;
  `0s` fires immediately on every `Ready=False`. This is separate from `incidents.debounce`, which
  governs the Alertmanager path.
- Any conformant Flux or Argo CD install works — EKS, GKE, AKS, or local k3d/kind; RunLore only needs
  the engine running, selected via `config.gitops.engine`.

## Reference

- [Configuration → `sources`]({{< relref "/docs/configuration/configuration.md#sources--what-wakes-runlore" >}})
  and [`triggers`]({{< relref "/docs/configuration/configuration.md#triggers--which-incidents-to-investigate" >}})
  for the full key reference.
- [Data sources]({{< relref "/docs/concepts/data-sources.md" >}}) — the `GitOpsProvider` interface
  behind the `what_changed` tool.
