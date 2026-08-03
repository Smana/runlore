# Commons snapshot — memory-pressure failure class (treatment arm)

A **verbatim** snapshot of three playbooks from the knowledge commons,
<https://github.com/Smana/runlore-kb-commons/tree/main/playbooks>, taken 2026-08-03:

| file | why it is here |
| --- | --- |
| `node-memory-pressure-eviction.md` | the playbook that fits the incident |
| `container-oomkilled.md` | the adjacent class the incident's evidence also shows — both playbooks name the other in `# Not covered` |
| `node-disk-pressure-eviction.md` | same eviction *mechanism*, different cause: a plausible mis-retrieval the corpus must not reward |

`examples/eval/node-eviction-with-commons.yaml` loads this directory as its
`commons_dir`; `examples/eval/node-eviction-no-commons.yaml` loads an empty one. The
two cases are otherwise identical, so whatever they score differently is what the
commons was worth on this incident.

Keep the entries **verbatim**. Three of their properties are load-bearing and editing
them would quietly change what the pair measures:

- **`type: Playbook`, no `resource:`** — commons entries describe a failure *class*, not
  one cluster's workload. Resource-lessness is what makes them structurally incapable
  of firing instant recall for a workload-carrying alert.
- **A `# Not covered` section** — each entry states its own boundary. It is the whole
  reason the corpus can hold two adjacent, easily-confused failure classes without the
  wrong one being cited.
- **They came from upstream unedited** — the moment a snapshot is tuned to the case, the
  eval measures the tuning rather than the shared corpus real deployments actually get.

`README.md` is a reserved bundle file (`internal/catalog/load.go`), so this note is
never indexed as a knowledge entry.

Refresh with:

```bash
gh api repos/Smana/runlore-kb-commons/contents/playbooks/<file>.md --jq '.content' | base64 -d
```
