Empty commons corpus (control arm)
==================================

This directory is the *control* half of the paired commons eval — the corpus
`examples/eval/node-eviction-no-commons.yaml` loads as its knowledge commons.

It is deliberately empty of knowledge, and this note is `.txt` rather than `.md` on
purpose. The catalog loader's definition of an entry is structural — a non-hidden
`.md` file (`internal/catalog/load.go`, `IsEntryFile`) — so a `.txt` file can never be
indexed by any future change. A `README.md` here would have been excluded only by
`reservedBundleFiles`, a maintained list of filenames: correct today, but the emptiness
of the baseline is load-bearing enough that it should not depend on a list staying
correct. Worse, this note's own prose (commons, knowledge, kb_search, corpus) is a
strong lexical match for exactly the query the control arm runs, so if it ever were
indexed the baseline would start answering the incident it exists not to answer.

Emptiness is what makes the arm a baseline: `kb_search` is offered to the model and
returns `no matching catalog entries`, which is exactly the state of a fresh RunLore
deployment before it has curated anything.

Keeping the directory present — rather than simply omitting `commons_dir` from the
baseline case — is what makes the pair a controlled comparison. Both halves get the
same tools, the same recall gates and the same recorded evidence; the only variable
between them is whether the commons corpus has anything in it.

Do not add `.md` files here. Any real entry would be indexed, and the baseline would
silently stop being a baseline. `TestShippedCommonsCorpusIsGenericAndInert` fails if
one appears.
