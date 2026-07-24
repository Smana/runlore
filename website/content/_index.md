---
title: RunLore
layout: hextra-home
---

<div class="hero-glow"></div>

{{< hextra/hero-badge >}}
  Free, open source · Apache-2.0
{{< /hextra/hero-badge >}}

{{< hextra/hero-headline >}}
  The SRE agent that speeds up investigations
{{< /hextra/hero-headline >}}

{{< hextra/hero-subtitle >}}
  …and learns from your context. RunLore reads your cluster, metrics, logs and
  network flows in parallel, reasons over them, and posts a confidence-scored root
  cause to chat — its only writes go to Git via reviewed PRs. Read-only by default,
  runs on your models, one Go binary.
{{< /hextra/hero-subtitle >}}

{{< hextra/hero-button text="Get Started" link="docs/getting-started/" >}}
{{< hextra/hero-button text="View on GitHub" link="https://github.com/Smana/runlore" style="background:transparent;border:1px solid rgba(148,163,184,0.45);color:inherit" >}}

<p class="rl-eyebrow">From alert to root cause — fast</p>
<div class="rl-flow"><svg viewBox="0 0 980 288" role="img" aria-label="RunLore reads your whole stack in parallel to turn an alert into a confidence-scored root cause in minutes, and instantly recalls known incidents in seconds."><text class="rl-src-lbl" x="480" y="16" text-anchor="middle">reads your whole stack — in parallel</text><rect class="rl-src" x="269" y="28" width="84" height="28" rx="8"/><text class="rl-sub" x="311" y="46" text-anchor="middle">metrics</text><rect class="rl-src" x="365" y="28" width="64" height="28" rx="8"/><text class="rl-sub" x="397" y="46" text-anchor="middle">logs</text><rect class="rl-src" x="441" y="28" width="82" height="28" rx="8"/><text class="rl-sub" x="482" y="46" text-anchor="middle">cluster</text><rect class="rl-src" x="535" y="28" width="88" height="28" rx="8"/><text class="rl-sub" x="579" y="46" text-anchor="middle">network</text><rect class="rl-src" x="635" y="28" width="56" height="28" rx="8"/><text class="rl-sub" x="663" y="46" text-anchor="middle">git</text><path class="rl-line" d="M311 56 L460 104"/><path class="rl-line" d="M397 56 L470 104"/><path class="rl-line" d="M482 56 L480 104"/><path class="rl-line" d="M579 56 L492 104"/><path class="rl-line" d="M663 56 L502 104"/><rect class="rl-node" x="20" y="122" width="170" height="64" rx="14"/><path class="rl-ico" d="M46 162 c0 -9 4 -14 8 -14 c4 0 8 5 8 14 l2 3 h-20 z"/><path class="rl-ico" d="M52 168 a2.4 2.4 0 0 0 4 0"/><text class="rl-title" x="74" y="150" font-size="15">Alert fires</text><text class="rl-sub" x="74" y="168">or GitOps failure</text><line class="rl-arrow" x1="192" y1="154" x2="350" y2="154"/><path class="rl-arrow-head" d="M350 154 l-10 -5 v10 z"/><rect class="rl-agent" x="356" y="104" width="248" height="100" rx="18"/><circle cx="390" cy="151" r="19" fill="#FFFFFF" stroke="#14c9a6" stroke-width="1.5"/><g transform="translate(373.6,134.7) scale(0.148)"><defs><mask id="hpowl"><rect x="0" y="0" width="220" height="220" fill="#fff"/><path d="M110,64 C104,54 92,49 81,53 C66,58 62,79 68,97 C74,114 91,124 110,130 C129,124 146,114 152,97 C158,79 154,58 139,53 C128,49 116,54 110,64 Z" fill="#000"/></mask></defs><path d="M110,166 C88,156 60,156 40,164 L40,188 C60,180 86,180 110,190 C134,180 160,180 180,188 L180,164 C160,156 132,156 110,166 Z" fill="#3b82f6"/><path d="M110,52 C101,36 87,28 72,31 C51,35 43,58 46,89 C48,126 71,160 110,170 C149,160 172,126 174,89 C177,58 169,35 148,31 C133,28 119,36 110,52 Z" fill="#101f4b" mask="url(#hpowl)"/><line x1="90" y1="86" x2="130" y2="86" stroke="#14c9a6" stroke-width="3"/><circle cx="90" cy="86" r="12" fill="#101f4b"/><circle cx="130" cy="86" r="12" fill="#101f4b"/><circle cx="90" cy="86" r="5" fill="#14c9a6"/><circle cx="130" cy="86" r="5" fill="#14c9a6"/><path d="M104,100 L116,100 L110,113 Z" fill="#14c9a6"/></g><text class="rl-title-lg" x="424" y="151">RunLore</text><text class="rl-sub" x="424" y="176">investigates</text><text class="rl-hint" x="686" y="140" text-anchor="middle">in minutes</text><line class="rl-arrow" x1="604" y1="154" x2="764" y2="154"/><path class="rl-arrow-head" d="M764 154 l-10 -5 v10 z"/><rect class="rl-node" x="770" y="122" width="190" height="64" rx="14"/><path class="rl-ico" d="M788 154 l6 6 l12 -14"/><text class="rl-title" x="816" y="150" font-size="14.5">Root cause</text><text class="rl-sub" x="816" y="168">confidence → chat</text><path class="rl-loop" d="M865 186 V234 Q865 246 853 246 H494 Q480 246 480 234 V210"/><path class="rl-loop-head" d="M480 203 l-5 9 h10 z"/><text class="rl-loop-lbl" x="665" y="268" text-anchor="middle">⚡ seen before → instant recall, in seconds</text></svg></div>

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

<h2 class="rl-section">Browse the docs</h2>

{{< hextra/feature-grid cols="3" >}}
  {{< hextra/feature-card link="docs/getting-started/" icon="book-open" title="Getting Started" subtitle="Deploy RunLore into a cluster and watch it react." >}}
  {{< hextra/feature-card link="docs/concepts/" icon="light-bulb" title="Concepts" subtitle="The design, the learning loop, and the data sources." >}}
  {{< hextra/feature-card link="docs/configuration/" icon="cog" title="Configuration" subtitle="Configure the agent, its sources, and MCP." >}}
  {{< hextra/feature-card link="docs/operations/" icon="server" title="Operations" subtitle="Run, observe, troubleshoot, and upgrade." >}}
  {{< hextra/feature-card link="docs/security/" icon="shield-check" title="Security" subtitle="Read-only default, the action gate, the trust model." >}}
  {{< hextra/feature-card link="docs/reference/" icon="bookmark" title="Reference" subtitle="Tools, benchmarks, and worked examples." >}}
{{< /hextra/feature-grid >}}
