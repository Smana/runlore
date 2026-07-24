---
title: RunLore
layout: hextra-home
---

<div class="hero-glow"></div>

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

{{< hextra/hero-button text="Get Started" link="docs/getting-started/" >}}
{{< hextra/hero-button text="View on GitHub" link="https://github.com/Smana/runlore" style="background:transparent;border:1px solid rgba(148,163,184,0.45);color:inherit" >}}

<p class="rl-eyebrow">How it works</p>
<div class="rl-flow"><svg viewBox="0 0 920 200" role="img" aria-label="How RunLore works: Investigate, Open a PR, Instant recall — and it learns your platform"><rect class="rl-chip" x="8" y="36" width="240" height="60" rx="14"/><circle class="rl-ico" cx="36" cy="62" r="7"/><line class="rl-ico" x1="41" y1="67" x2="47" y2="73"/><text class="rl-label" x="62" y="72">Investigate</text><rect class="rl-chip" x="340" y="36" width="240" height="60" rx="14"/><circle class="rl-ico" cx="364" cy="52" r="4.5"/><circle class="rl-ico" cx="364" cy="80" r="4.5"/><path class="rl-ico" d="M364 56 v20"/><circle class="rl-ico" cx="382" cy="66" r="4.5"/><path class="rl-ico" d="M364 66 q0 -10 14 -12"/><text class="rl-label" x="398" y="72">Open a PR</text><rect class="rl-chip" x="672" y="36" width="240" height="60" rx="14"/><path class="rl-ico-fill" d="M700 44 l-14 24 h9 l-5 20 16 -26 h-9 z"/><text class="rl-label" x="722" y="72">Instant recall</text><line class="rl-arrow" x1="256" y1="66" x2="332" y2="66"/><path class="rl-arrow-head" d="M332 66 l-9 -5 v10 z"/><line class="rl-arrow" x1="588" y1="66" x2="664" y2="66"/><path class="rl-arrow-head" d="M664 66 l-9 -5 v10 z"/><path class="rl-loop" d="M792 96 V150 Q792 164 778 164 H142 Q128 164 128 150 V104"/><path class="rl-loop-head" d="M128 98 l-5 9 h10 z"/><text class="rl-loop-label" x="460" y="187" text-anchor="middle">↺ learns your platform</text></svg></div>

{{< hextra/feature-grid cols="2" >}}
  {{< hextra/feature-card icon="academic-cap" title="Learns your platform"
    subtitle="Every investigation opens a PR in a Git repo you own. A human merges it, building a knowledge base of your incidents — the same pattern next time gets an instant answer." >}}
  {{< hextra/feature-card icon="shield-check" title="Read-only by default"
    subtitle="Reads your cluster, metrics, logs and network flows. Its only writes go to Git, via reviewed PRs — and would rather say “I don’t know” than guess." >}}
  {{< hextra/feature-card icon="cube-transparent" title="GitOps-native"
    subtitle="Turns “what changed?” into an exact Git answer — the rendered-manifest diff of the revisions Flux or Argo CD reconciled." >}}
  {{< hextra/feature-card icon="chip" title="Your models · one Go binary"
    subtitle="A single self-hosted Go binary running in your cluster on your own model providers. Portable, no lock-in, your data." >}}
{{< /hextra/feature-grid >}}
