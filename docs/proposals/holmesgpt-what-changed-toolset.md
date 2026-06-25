# Proposal: a GitOps "what-changed" toolset for HolmesGPT

> **Status:** Draft (POS-4 strategic spike) · **Author:** RunLore · **Date:** 2026-06-25
> **Audience:** internal decision first, then a HolmesGPT upstream discussion.
> **TL;DR:** Contribute RunLore's GitOps revision-diff ("what changed") capability to
> HolmesGPT as a toolset — first via an MCP server (zero upstream friction), then as a
> native toolset PR. It fills HolmesGPT's clearest RCA gap and converts our biggest
> competitive threat into a distribution channel.

## 1. Why this, why now

HolmesGPT is the strongest open-source investigation agent (CNCF Sandbox; 60+
toolsets; runbooks; a first-class MCP client; read-only by default). Two facts make a
contribution strategically obvious:

1. **Change/deploy correlation is the #1 RCA signal** (consistently across the
   literature and our own landscape survey), yet HolmesGPT's toolsets are
   Prometheus/Loki/Kubernetes-centric — it has **no Flux/Argo CD revision-diff**. It
   can see *that* a workload is unhealthy, but not the *exact rendered-manifest change*
   between the two revisions the GitOps engine actually reconciled.
2. **It is our #1 ranked threat.** If HolmesGPT adds change-diff RCA, it erases the
   sharper of RunLore's two differentiators. The what-changed Git diff is, candidly, a
   *copyable feature* (Komodor and Anyshift already ship change-aware RCA
   commercially) — not a durable moat. RunLore's durable wedge is the **open,
   reviewable, outcome-weighted knowledge catalog** and the honesty/eval posture, not
   the diff itself.

**Conclusion:** contributing the diff upstream is net-positive. We give away a feature
we cannot defend anyway, in exchange for (a) being the canonical GitOps lens for the
CNCF-blessed agent, (b) upstream credibility and reach, and (c) keeping the *learning*
layer — which HolmesGPT deliberately does not do (stateless, human-authored runbooks)
— as RunLore's distinct space. Alignment over competition.

## 2. What the toolset provides

The toolset exposes RunLore's what-changed spine as read-only tools the HolmesGPT
ReAct loop can call. Proposed tools (names indicative):

| Tool | Input | Output (text for the LLM) |
|---|---|---|
| `gitops_what_changed` | `namespace`, optional `workload` | The change **timeline** for the selector: each Flux Kustomization/HelmRelease or Argo CD Application revision with `from..to`, engine, type, and the **path-scoped unified diff** of the rendered manifests between the two revisions the engine reconciled. |
| `gitops_revision_diff` | `source repo`, `from_rev`, `to_rev`, `path` | The raw path-scoped unified diff between two specific revisions (for drill-down). |

Key property: the diff is between the **exact deployed revisions** (resolved from
Flux/Argo status), path-scoped to the failing workload — not a generic `git log`. This
is the high-signal, low-noise artifact HolmesGPT's reasoning currently lacks.

It is **read-only** (clones + diffs; never writes), authenticated for private repos via
a GitHub App installation token, and bounded (shallow/cached clones, per-call timeout).

## 3. Integration approach — two paths, sequence them

### Path A — MCP server (primary; ship first)

HolmesGPT is a **first-class MCP client**. Expose the what-changed engine as an MCP
server — e.g. a `lore mcp` subcommand (or a small standalone `runlore-whatchanged-mcp`
binary reusing `internal/whatchanged` + the GitOps providers). HolmesGPT consumes it by
adding an MCP server entry to its config; **no upstream PR is required**.

- **Pros:** fastest to a working demo + eval; RunLore owns the release cadence; works
  with any MCP-capable agent (kagent, Claude Desktop, etc.), not just HolmesGPT; clean
  separation (HolmesGPT reasons, RunLore serves data).
- **Cons:** an extra process to run; not "in the box" with HolmesGPT.

### Path B — native HolmesGPT toolset (deeper; upstream once proven)

Contribute a toolset to the HolmesGPT repo (under its toolsets plugin dir), wrapping a
stable `lore what-changed --namespace … [--workload …]` CLI (text output) or a thin
Python shim over the MCP server.

- **Pros:** ships with HolmesGPT; CNCF discoverability; "RunLore" named in the
  ecosystem; documented alongside the official toolsets.
- **Cons:** upstream review + maintenance alignment; coupling to HolmesGPT's toolset
  schema and release process.

**Recommendation:** A → B. Prove value and iterate on the tool contract via the MCP
server; once the interface is stable and we have eval evidence, open the upstream
toolset PR with that evidence in hand.

## 4. Design notes / technical fit

- **Reuse, don't rebuild:** `internal/whatchanged` (go-git path-scoped differ),
  `internal/providers/gitops/{flux,argocd}` (revision history + change detection), and
  the GitHub App token source already exist and are tested. The MCP server is a thin
  adapter over them.
- **Output contract:** unified diff + revision metadata as plain text, scoped to the
  selector — the same content the `what_changed` tool already renders for RunLore's own
  loop, so there's a single code path.
- **Scoping:** HolmesGPT passes the failing workload (namespace, name); the toolset
  scopes the timeline + diff to it (and its GitOps source path), keeping output small.
- **Boundaries / non-goals:** read-only; no remediation; **no knowledge write-back in
  v1** — the catalog/learning loop stays RunLore's layer (a later, separate bridge could
  let HolmesGPT *read* RunLore's OKF catalog, or RunLore *capture* HolmesGPT findings,
  but that is out of scope here and should not gate this contribution).
- **Licensing:** both projects are Apache-2.0 — compatible.

## 5. Phased plan

1. **MCP MVP** — `lore mcp` exposes `gitops_what_changed`; demo against an induced Flux
   failure (reuse the e2e scenario). *(~days; all building blocks exist.)*
2. **HolmesGPT eval** — wire the MCP server into HolmesGPT; measure RCA lift on
   change-caused incidents vs. HolmesGPT without it. This evidence is the upstream pitch.
3. **Upstream** — open a HolmesGPT discussion/issue with the eval results; submit the
   native toolset PR (Path B) + docs.
4. **(Optional, later)** — explore an OKF read bridge or a HolmesGPT→RunLore capture
   path; revisit only if the toolset lands and there's appetite.

## 6. Open questions (verify before upstreaming)

- **Toolset manifest schema** — confirm HolmesGPT's current custom-toolset format
  (tool definition fields, parameter schema, output conventions) against its docs; the
  field names above are indicative, not verified against the latest release.
- **Invocation contract** — how HolmesGPT decides to call the toolset and what selector
  context it passes (does it reliably provide namespace+workload?).
- **Argo vs Flux parity** — RunLore's deep GitOps inspection is Flux-first; confirm the
  Argo CD path returns equivalent revision/diff fidelity for the toolset's promises.
- **Maintenance ownership** — who maintains the native toolset upstream; CI alignment.
- **Positioning guardrail** — keep the contribution scoped to *data* (the diff). The
  open-catalog learning loop is deliberately **not** part of this; that's the line that
  keeps RunLore distinct rather than absorbed.

## 7. Recommendation

Proceed with **Path A (MCP server) as a spike**, build the eval evidence, and decide on
the upstream PR from data. Low cost (the engine exists), high strategic option value,
and it hedges the top threat regardless of whether the upstream PR is ultimately
accepted — the MCP server is independently useful to any MCP-capable agent.
