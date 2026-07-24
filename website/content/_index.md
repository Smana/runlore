---
title: RunLore
layout: hextra-home
---

{{< hextra/hero-badge >}}
  Free, open source · Apache-2.0
{{< /hextra/hero-badge >}}

{{< hextra/hero-headline >}}
  An open-source SRE agent that investigates incidents
{{< /hextra/hero-headline >}}

{{< hextra/hero-subtitle >}}
  …and remembers what it learns. Read-only by default — it reads your cluster,
  metrics, logs and network flows, and its only writes go to Git via reviewed PRs.
  Runs on your models, as a single Go binary in your cluster.
{{< /hextra/hero-subtitle >}}

{{< hextra/hero-button text="Get Started" link="docs/getting-started" >}}

{{< hextra/feature-grid cols="2" >}}
  {{< hextra/feature-card title="Learns your platform"
    subtitle="Every investigation opens a PR in a Git repo you own. A human merges it, building a knowledge base of your incidents — the same pattern next time gets an instant answer." >}}
  {{< hextra/feature-card title="Read-only by default"
    subtitle="Reads your cluster, metrics, logs and network flows. Its only writes go to Git, via reviewed PRs — and would rather say “I don’t know” than guess." >}}
  {{< hextra/feature-card title="GitOps-native"
    subtitle="Turns “what changed?” into an exact Git answer — the rendered-manifest diff of the revisions Flux or Argo CD reconciled." >}}
  {{< hextra/feature-card title="Your models · one Go binary"
    subtitle="A single self-hosted Go binary running in your cluster on your own model providers. Portable, no lock-in, your data." >}}
{{< /hextra/feature-grid >}}
