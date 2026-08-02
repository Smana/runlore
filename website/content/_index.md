---
title: RunLore
layout: hextra-home
description: "The SRE agent that builds a memory your team reviews. RunLore investigates incidents like any other agent — the difference is what happens next: every verified root cause becomes a pull request in a Git repo you own. Merge it, and the next time that failure appears it is answered in seconds, from knowledge your team approved."
images:
  - images/og-card.png
---

<div class="hero-glow"></div>

{{< hextra/hero-badge >}}
  Free, open source · Apache-2.0
{{< /hextra/hero-badge >}}

{{< hextra/hero-headline >}}
  The SRE agent that builds a memory your team reviews.
{{< /hextra/hero-headline >}}

{{< hextra/hero-subtitle >}}
  It investigates incidents like any other agent. The difference is what happens
  next: every verified root cause becomes a pull request in a Git repo you own.
  You merge it, and the next time that failure appears it's answered in seconds —
  from knowledge your team approved.
{{< /hextra/hero-subtitle >}}

{{< hextra/hero-button text="Get Started" link="docs/getting-started/" >}}
{{< hextra/hero-button text="View on GitHub" link="https://github.com/Smana/runlore" style="background:transparent;border:1px solid rgba(148,163,184,0.45);color:inherit" >}}

<p class="rl-eyebrow">From alert to root cause — into memory you reviewed</p>
<div class="rl-flow"><svg viewBox="0 0 980 348" role="img" aria-label="RunLore investigates an alert into a confidence-scored root cause, drafts it as a pull request your team reviews and merges into a knowledge base you own, and answers the same failure from that memory in seconds next time."><text class="rl-src-lbl" x="480" y="16" text-anchor="middle">reads your whole stack — in parallel</text><rect class="rl-src" x="269" y="28" width="84" height="28" rx="8"/><text class="rl-sub" x="311" y="46" text-anchor="middle">metrics</text><rect class="rl-src" x="365" y="28" width="64" height="28" rx="8"/><text class="rl-sub" x="397" y="46" text-anchor="middle">logs</text><rect class="rl-src" x="441" y="28" width="82" height="28" rx="8"/><text class="rl-sub" x="482" y="46" text-anchor="middle">cluster</text><rect class="rl-src" x="535" y="28" width="88" height="28" rx="8"/><text class="rl-sub" x="579" y="46" text-anchor="middle">network</text><rect class="rl-src" x="635" y="28" width="56" height="28" rx="8"/><text class="rl-sub" x="663" y="46" text-anchor="middle">git</text><path class="rl-line" d="M311 56 L460 104"/><path class="rl-line" d="M397 56 L470 104"/><path class="rl-line" d="M482 56 L480 104"/><path class="rl-line" d="M579 56 L492 104"/><path class="rl-line" d="M663 56 L502 104"/><rect class="rl-node" x="20" y="122" width="170" height="64" rx="14"/><path class="rl-ico" d="M46 162 c0 -9 4 -14 8 -14 c4 0 8 5 8 14 l2 3 h-20 z"/><path class="rl-ico" d="M52 168 a2.4 2.4 0 0 0 4 0"/><text class="rl-title" x="74" y="150" font-size="15">Alert fires</text><text class="rl-sub" x="74" y="168">or GitOps failure</text><line class="rl-arrow" x1="192" y1="154" x2="350" y2="154"/><path class="rl-arrow-head" d="M350 154 l-10 -5 v10 z"/><rect class="rl-agent" x="356" y="104" width="248" height="100" rx="18"/><circle cx="390" cy="151" r="19" fill="#FFFFFF" stroke="#14c9a6" stroke-width="1.5"/><g transform="translate(373.6,134.7) scale(0.148)"><defs><mask id="hpowl"><rect x="0" y="0" width="220" height="220" fill="#fff"/><path d="M110,64 C104,54 92,49 81,53 C66,58 62,79 68,97 C74,114 91,124 110,130 C129,124 146,114 152,97 C158,79 154,58 139,53 C128,49 116,54 110,64 Z" fill="#000"/></mask></defs><path d="M110,166 C88,156 60,156 40,164 L40,188 C60,180 86,180 110,190 C134,180 160,180 180,188 L180,164 C160,156 132,156 110,166 Z" fill="#3b82f6"/><path d="M110,52 C101,36 87,28 72,31 C51,35 43,58 46,89 C48,126 71,160 110,170 C149,160 172,126 174,89 C177,58 169,35 148,31 C133,28 119,36 110,52 Z" fill="#101f4b" mask="url(#hpowl)"/><line x1="90" y1="86" x2="130" y2="86" stroke="#14c9a6" stroke-width="3"/><circle cx="90" cy="86" r="12" fill="#101f4b"/><circle cx="130" cy="86" r="12" fill="#101f4b"/><circle cx="90" cy="86" r="5" fill="#14c9a6"/><circle cx="130" cy="86" r="5" fill="#14c9a6"/><path d="M104,100 L116,100 L110,113 Z" fill="#14c9a6"/></g><text class="rl-title-lg" x="424" y="151">RunLore</text><text class="rl-sub" x="424" y="176">investigates</text><line class="rl-arrow" x1="604" y1="154" x2="764" y2="154"/><path class="rl-arrow-head" d="M764 154 l-10 -5 v10 z"/><rect class="rl-node" x="770" y="122" width="190" height="64" rx="14"/><path class="rl-ico" d="M788 154 l6 6 l12 -14"/><text class="rl-title" x="816" y="150" font-size="14.5">Root cause</text><text class="rl-sub" x="816" y="168">confidence → chat</text><path class="rl-arrow" d="M865 186 V252"/><path class="rl-arrow-head" d="M865 264 l-5 -10 h10 z"/><rect class="rl-node" x="762" y="264" width="198" height="62" rx="14"/><text class="rl-title" x="861" y="290" font-size="14.5" text-anchor="middle">Drafted as a PR</text><text class="rl-sub" x="861" y="310" text-anchor="middle">one per verified cause</text><path class="rl-arrow" d="M762 295 H666"/><path class="rl-arrow-head" d="M654 295 l10 -5 v10 z"/><text class="rl-cap" x="710" y="284" text-anchor="middle">you review</text><text class="rl-cap" x="710" y="313" text-anchor="middle">&amp; merge</text><rect x="330" y="252" width="316" height="86" rx="18" fill="rgba(20,201,166,0.16)" stroke="#14c9a6" stroke-width="3"/><ellipse cx="374" cy="284" rx="13" ry="5" fill="none" stroke="#14c9a6" stroke-width="2.2"/><path d="M361 284 v22 a13 5 0 0 0 26 0 v-22" fill="none" stroke="#14c9a6" stroke-width="2.2"/><text class="rl-title-lg" x="404" y="288" font-size="19">Your knowledge base</text><text class="rl-sub" x="404" y="314">plain markdown, in your own Git</text><path class="rl-loop" d="M488 252 V214"/><path class="rl-loop-head" d="M488 204 l-5 10 h10 z"/><text class="rl-loop-lbl" x="506" y="234" text-anchor="start">⚡ next time: answered in seconds</text></svg></div>

{{< hextra/feature-grid cols="2" >}}
  {{< hextra/feature-card icon="academic-cap" title="Knowledge you own"
    subtitle="Every investigation opens a pull request in a Git repo you control. A human merges it. The result is portable markdown with full provenance — exportable, greppable, and yours if you stop using RunLore tomorrow." >}}
  {{< hextra/feature-card icon="shield-check" title="Read-only by default"
    subtitle="Reads your cluster, metrics, logs and network flows. Its only writes go to Git, via reviewed PRs — and it would rather say “I don’t know” than guess." >}}
  {{< hextra/feature-card icon="cube-transparent" title="GitOps-native provenance"
    subtitle="Turns “what changed?” into an exact Git answer — the rendered-manifest diff of the revisions Flux or Argo CD reconciled. That provenance is what makes the knowledge trustworthy." >}}
  {{< hextra/feature-card icon="chip" title="Your models · one Go binary"
    subtitle="A single self-hosted Go binary running in your cluster on your own model providers. Any OpenAI-compatible endpoint, or native Anthropic. No lock-in, your data. Point it at any MCP server for tools it doesn’t ship." >}}
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
