---
title: Matrix
weight: 20
integration: {kind: notifier, id: matrix}
---

**What it gives you** — findings delivered to any Matrix room, with an optional zero-ingress
👍/👎 feedback loop over reactions.

## Minimal config

```yaml
notify:
  matrix:
    homeserver: https://matrix.org
    room_id: "!yourroom:matrix.org"
    access_token_env: MATRIX_TOKEN
```

## Verify it locally

```bash
kubectl -n runlore create secret generic runlore-secrets \
  --from-literal=MATRIX_TOKEN='<matrix-access-token>'
```

Fire a test incident (see [Alertmanager]({{< relref "alertmanager.md" >}}) or `hack/demo.sh`) and
confirm delivery:

```bash
kubectl -n runlore logs deploy/runlore | grep 'msg=findings'
```

## Notes

- Matrix delivers the same content as Slack's incoming-webhook path: a **single** message per
  finding (no threading).
- **`matrix.feedback_reactions` (opt-in, default `false`)** — react 👍/👎 to a RunLore message and the
  rating lands in the outcome ledger, with the same per-user dedup and trust weighting as Slack's
  feedback buttons.
- **Nothing is exposed to enable it** — reactions arrive over the client-server `/sync` long-poll, an
  *outbound* HTTPS request authenticated by the notifier's own access token. This is the zero-ingress
  alternative to Slack's Interactivity Request URL.
- The listener runs on the leader only, skips reactions from before startup, ignores every emoji
  except 👍/👎, and only counts votes on messages **the bot itself sent** (attribution is anchored on
  `/whoami`).
- Startup fails loud unless `homeserver`/`room_id`/`access_token_env` and `outcome.ledger_path` are
  set.
- Use an **invite-only room** — any room member can vote.

## Reference

- [Configuration → `notify`]({{< relref "/docs/configuration/configuration.md#notify--where-findings-go" >}})
  for the full key reference.
- [Security model → the feedback channels]({{< relref "/docs/security/security-model.md#the-feedback-channels--exposure--trust-model" >}})
  for the exposure and vote trust model.
