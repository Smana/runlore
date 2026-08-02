---
title: Eval scorecard
---

# RunLore nightly eval scorecard

*No nightly scorecard has been published yet.*

The nightly replay eval publishes a per-scenario scorecard — pass/fail, recall
outcomes, confidence calibration, model, date and cost — on every run, red or green.
When a run has published, this page shows it.

Reproduce it yourself:

```
lore eval -config eval/ci.runlore.yaml -cases examples/eval -n 5 -fail-under 0.7
```

See [`.github/workflows/eval.yaml`](https://github.com/Smana/runlore/blob/main/.github/workflows/eval.yaml).
