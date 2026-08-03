Commons snapshot — memory-pressure failure class (treatment arm)
================================================================

A VERBATIM snapshot of three playbooks from the knowledge commons,
https://github.com/Smana/runlore-kb-commons/tree/main/playbooks, taken 2026-08-03:

  node-memory-pressure-eviction.md  the playbook that fits the incident
  container-oomkilled.md            the adjacent class the incident's evidence also
                                    shows — both playbooks name the other under
                                    "# Not covered"
  node-disk-pressure-eviction.md    same eviction MECHANISM, different cause: a
                                    plausible mis-retrieval the corpus must not reward

`examples/eval/node-eviction-with-commons.yaml` loads this directory as its
`commons_dir`; `examples/eval/node-eviction-no-commons.yaml` loads an empty one. The
two cases are otherwise identical, so whatever they score differently is what the
commons was worth on this incident.

THIS IS A VENDORED FORK, AND THE REPO'S POLICY IS THAT IT SHOULDN'T EXIST
------------------------------------------------------------------------

`.github/workflows/ci.yaml` says the commons is deliberately NOT copied into this repo,
because "a copy here would be a fork that drifts, not a second safety net". That is
still the rule for anything the agent SERVES. These three files are a deliberate,
narrow exception: an A/B eval has to pin its treatment corpus or the number it prints
is not reproducible, and a run whose corpus silently changed underneath it measures the
upstream repo's edit history rather than the commons. The exception is recorded in
ci.yaml next to the policy it bends, so it reads as a decision rather than an oversight.

What that costs: nothing here is validated by `lore validate-kb`, and nothing detects
drift from upstream. Treat these files as a dated snapshot, not as the commons.

Refresh (and update the date above in the same commit):

  gh api repos/Smana/runlore-kb-commons/contents/playbooks/<file>.md \
    --jq '.content' | base64 -d

KEEP THE ENTRIES VERBATIM
-------------------------

Three of their properties are load-bearing, and editing them would quietly change what
the pair measures:

- `type: Playbook`, no `resource:` — commons entries describe a failure CLASS, not one
  cluster's workload. Resource-lessness is what makes them structurally incapable of
  firing instant recall for a workload-carrying alert.
- A `# Not covered` section — each entry states its own boundary. It is the whole reason
  the corpus can hold two adjacent, easily-confused failure classes without the wrong
  one being cited.
- They came from upstream unedited — the moment a snapshot is tuned to the case, the
  eval measures the tuning rather than the shared corpus real deployments actually get.

`TestShippedCommonsCorpusIsGenericAndInert` pins the first two.

This note is `.txt`, not `.md`, so the loader's structural definition of an entry
(`IsEntryFile`: a non-hidden `.md`) excludes it outright. As `README.md` it would have
been excluded only by `reservedBundleFiles`, a maintained list of filenames — correct
today, but not something the corpus's contents should depend on.
