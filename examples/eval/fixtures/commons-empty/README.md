# Empty commons corpus (control arm)

This directory is the **control** half of the paired commons eval — the corpus
`examples/eval/node-eviction-no-commons.yaml` loads as its knowledge commons.

It is deliberately empty of knowledge. `README.md` is a *reserved bundle file*
(`internal/catalog/load.go`, `reservedBundleFiles`), so the catalog loader skips it and
indexes nothing at all. That is the point: `kb_search` is offered to the model and
returns `no matching catalog entries`, which is exactly the state of a fresh RunLore
deployment before it has curated anything.

Keeping the directory present — rather than simply omitting `commons_dir` from the
baseline case — is what makes the pair a controlled comparison. Both halves get the
same tools, the same recall gates and the same recorded evidence; the only variable
between them is whether the commons corpus has anything in it.

**Do not add `.md` entries here.** Any real entry would be indexed, and the baseline
would silently stop being a baseline.
