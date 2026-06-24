# KB-Entry Validation — Design Spec

- **Date:** 2026-06-24
- **Status:** Draft (awaiting review)
- **Type:** Feature (validation + CI + process)
- **Author:** RunLore maintainers

## 1. Problem

RunLore's learning loop opens KB pull requests against the catalog repo
(`Smana/runlore-kb`). Today the only automated gate on a new entry is
`curator.meetsBar` — a **finding-quality** gate (`internal/curator/curator.go`):

```
Verified (survived adversarial review)
&& Confidence >= min_confidence (default 0.75)
&& len(RootCauses) > 0
&& top.Summary != "" && len(top.Evidence) > 0
&& (top.ChangeRef != "" || top.SuggestedAction != "")   // provenance
```

This gates the *investigation*, not the *entry*. Three gaps surface in
production:

1. **No structural validation of the rendered entry.** `catalog.Load`
   (`internal/catalog/load.go`) parses YAML frontmatter and **silently skips**
   any file that fails to parse, and **silently indexes** a malformed-but-parseable
   one (e.g. empty `title`, missing `## Cause`, bad `resource`). A broken entry
   that merges contributes nothing to recall, with only a `WARN` no one watches.

2. **No durability / generalizability check.** `meetsBar` passes *confident +
   verified* findings, but **confident+verified ≠ KB-worthy**. Evidence from the
   live catalog:
   - **Closed (rejected) by humans:** PRs #20/#22/#27/#29 — all
     `Kustomization DependencyNotReady` / `GitRepository not found`, opened during
     a cluster's **bootstrap convergence** (transient, self-resolving noise).
   - **Merged (accepted):** #48 `HarborRegistryDown`, #38 Harbor-HelmRelease —
     durable, generalizable incidents with a clear cause + fix.
   The distinguishing dimension — *is this a durable incident or transient/
   environmental/bootstrap noise?* — is invisible to `meetsBar`.

3. **No cause-explains-symptom check.** PR #51
   (`AlertmanagerClusterFailedToSendAlerts`, confidence 0.9) is well-formed but
   its root causes describe broad observability-stack breakage (Kyverno HostPath
   denials, Cilium IP exhaustion, capacity) without pinning why Alertmanager
   failed to **send**. A reviewer should be prompted to check that the top Cause
   actually explains the Symptom.

The accept/reject history is an **implicit** human process. This spec makes it an
**explicit, partially-automated** process: a structural hard-gate that blocks
malformed entries, an LLM advisory that surfaces the two judgment failure modes,
and a documented reviewer workflow.

## 2. Goals / Non-goals

**Goals**
- A deterministic **structural validator** that is the merge gate for every KB
  entry (RunLore- and human-authored).
- An **LLM-assisted semantic advisory** (cause-explains-symptom + durability)
  posted to the PR — assist, not gate.
- A **GitHub Action** on `runlore-kb` wiring both into the PR review.
- A **load-time strict-warn** in `catalog.Load` so malformed merged entries are
  loud + counted, not silent.
- A **documented validation process** (criteria + workflow).
- Shared validation logic in one package, reused by all consumers (DRY).

**Non-goals**
- Changing `meetsBar` or the curation flow (the validator is additive).
- Auto-merging or auto-closing PRs (the human stays the decision-maker).
- Semantic checks as a hard gate (LLM judgment is advisory only — non-determinism
  must not block merges or break CI when no model key is present).
- Rewriting the OKF schema or the recall/index path.

## 3. Validation criteria

### 3.1 Structural (deterministic — HARD GATE)

Applied to every `*.md` under the catalog root except reserved `index.md` /
`log.md`. Each violation is an `Issue{Severity, Field, Message}`; any
`Severity=Error` fails the gate.

**Frontmatter (all types):**
- `type` present and ∈ vocabulary `{Incident, Runbook, Concept}`.
- `title` non-empty, single line, ≤ 120 chars.
- `description` non-empty.
- `resource` non-empty, no internal whitespace, shaped `namespace/name` or a
  single bare token for cluster-scoped resources (regex, see §8 D2).
- `tags` non-empty list.
- `fingerprint` (required for `type: Incident`) = 64-char lowercase hex.

**Body (type-aware):**
- `type: Incident` requires headings `## Symptom`, `## Cause`, `## Resolution`,
  each with non-empty content; `## Investigate` recommended (warn if absent).
- `type: Runbook` / `Concept` require a non-empty body; section rules are
  intentionally relaxed in v1 (documented as extensible).
- No heading may be present-but-empty.

These rules mirror exactly what `curator.draftKBEntry` emits and what
`catalog.parseEntry` consumes, so a RunLore-authored entry that passes `meetsBar`
also passes the structural gate by construction (regression-tested).

### 3.2 Semantic (LLM-assisted — ADVISORY ONLY)

Runs only when a model is configured; otherwise skipped cleanly. Returns an
`Advisory` with two verdicts, each `{ok bool, rationale string}`:
- **cause_explains_symptom:** does the top Cause plausibly explain the Symptom?
- **durable:** is this a durable, generalizable incident, or transient /
  environmental / bootstrap-convergence noise unlikely to recur or teach?

The advisory is **never** a gate: it is rendered into the PR comment to focus the
human review. A `not ok` verdict is a prompt to a reviewer, not a failure.

## 4. Architecture

### 4.1 Shared core — `internal/kbvalidate` (new package)

```
type Severity int   // Error | Warning
type Issue struct { Severity Severity; Field, Message string }

// Deterministic. No I/O, no model. Pure(entry) -> issues.
func ValidateStructural(e catalog.Entry) []Issue

// LLM-assisted advisory. Reuses internal/model. Returns zero-value Advisory
// (and Skipped=true) when model is nil so callers degrade cleanly.
type Verdict  struct { OK bool; Rationale string }
type Advisory struct { CauseExplainsSymptom, Durable Verdict; Skipped bool }
func ReviewSemantic(ctx, e catalog.Entry, m model.Client) (Advisory, error)

func HasErrors(issues []Issue) bool
```

`ValidateStructural` operates on the already-parsed `catalog.Entry` so it shares
the parser with `Load` (no second frontmatter parser). A small parse-tolerant
path is added so a file that fails `parseEntry` yields a structural `Error`
(rather than a skip) when validated directly.

### 4.2 Consumers

1. **CLI — `lore validate-kb [flags] <paths|dir>`** (`cmd/lore`):
   - Structural always; exits non-zero if any `Error`.
   - `--semantic` runs the advisory (requires a configured model; warns + skips if
     absent).
   - `--format=text` (human) | `github` (GH annotations + a comment-body file).
   - `--changed-only` (with a base ref) to validate just a PR's changed entries.

2. **GitHub Action — `.github/workflows/validate-kb.yml` in `runlore-kb`:**
   - Trigger: `pull_request` on `**.md`.
   - Runs `ghcr.io/smana/runlore:<pinned>` → `lore validate-kb --changed-only
     --semantic --format=github`.
   - Structural `Error` → non-zero exit → **required check fails → merge blocked**.
   - Advisory → upsert a single PR comment (marker-delimited, idempotent).
   - Gemini key from a repo secret; semantic degrades to skipped without it
     (structural gate still runs — zero-config baseline).

3. **Load-time strict-warn — `catalog.Load`:**
   - For each entry, run `ValidateStructural`; on `Error`, log loud
     (`level=WARN`, path + issues) and increment a new
     `catalog_invalid_entries_total` metric. **Behaviour preserved:** the entry is
     still indexed/skipped exactly as today (one bad entry never empties the
     catalog) — only visibility changes. No new hard failure at runtime.

### 4.3 Data flow

```
RunLore investigation → meetsBar → draftKBEntry → OpenPR(triggered)
                                                      │
runlore-kb PR ── validate-kb.yml ──> lore validate-kb --changed-only --semantic
                                       ├─ ValidateStructural → gate (block on Error)
                                       └─ ReviewSemantic     → PR comment (advisory)
                                                      │
                                          human review (criteria + advisory)
                                                      │
                                       merge=accept / close=reject
                                                      │
catalog.Load (every replica) → ValidateStructural → WARN + metric on bad merged entry
```

## 5. The documented process (`docs/kb-validation.md`)

1. RunLore opens a PR labelled `runlore` + `triggered` ("raw finding").
2. CI runs `validate-kb`: **structural gate must pass** (it's a required check).
3. CI posts the **semantic advisory** comment (cause-explains-symptom, durable?).
4. A human reviews against the criteria, weighting:
   - structural: already enforced;
   - **durability:** reject transient/bootstrap/environmental noise (e.g. the
     closed `DependencyNotReady` cluster of PRs);
   - **cause-explains-symptom:** the top Cause must explain *this* alert (the #51
     lesson).
5. **Merge = accept** (optionally label `accepted`); **close = reject**.
6. A malformed entry that somehow merges is caught loudly at load time
   (`catalog_invalid_entries_total` + WARN) and fixed via a follow-up PR.

## 6. Error handling / failure modes

- **No model key in CI:** `ReviewSemantic` returns `Skipped=true`; the Action
  still runs the structural gate and notes "semantic review skipped (no model)".
- **Model error / timeout:** advisory degrades to `Skipped` with the error noted;
  it **never** fails the check.
- **Unparseable frontmatter at validate time:** a structural `Error` (gate fail) —
  unlike `Load`, which skips — because at PR time we *want* to block it.
- **Reserved files (`index.md`, `log.md`):** skipped by the validator, as by `Load`.
- **Non-`.md` / dotfiles:** ignored.

## 7. Testing strategy (TDD)

- `internal/kbvalidate/validate_structural_test.go` — table-driven: a valid
  Incident (the #48 standard) passes; each rule violated in isolation produces the
  expected `Issue` (missing/!vocab `type`, empty `title`, bad `resource`, missing
  `## Cause`, empty section, bad `fingerprint`, Runbook/Concept relaxations).
- **Regression:** an entry from `draftKBEntry` over a `meetsBar`-passing
  investigation passes `ValidateStructural` (the two gates are consistent by
  construction).
- `ReviewSemantic` — fake `model.Client`: a clearly-transient entry → `durable=not
  ok`; a cause-mismatched entry → `cause_explains_symptom=not ok`; nil model →
  `Skipped`, no error.
- `cmd/lore` — `validate-kb` exits non-zero on structural error, zero on
  warnings-only; `--format=github` annotation shape; `--changed-only` filters.
- `catalog.Load` — a malformed merged entry increments
  `catalog_invalid_entries_total` and logs WARN while load still succeeds.
- e2e (`hack/e2e-k3d.sh` extension or a fixture dir): run `validate-kb` over a
  fixtures tree (good + each bad variant) and assert exit codes + annotations.

## 8. Open decisions

- **D1 — type vocabulary:** v1 = `{Incident, Runbook, Concept}`. Confirm against
  the existing `runlore-kb` entries' `type` values before locking (the current
  set was authored by RunLore as `Incident` plus human runbooks).
- **D2 — `resource` regex:** allow `ns/name`, bare `name` (cluster-scoped), and
  `kind/name`? v1 proposal: `^[a-z0-9.-]+(/[a-z0-9.-]+){0,2}$`, non-empty,
  no spaces. Refine against real entries.
- **D3 — semantic gate vs advisory:** spec'd as **advisory** (per design
  decision). Revisit only if false-accepts of transient noise persist.
- **D4 — CI image vs `go install`:** use the pinned `ghcr.io/smana/runlore` image
  (no Go toolchain in `runlore-kb` CI). Pin to a digest; bump with releases.

## 9. Success criteria

- **SC-1:** A structurally-malformed KB PR fails the required `validate-kb` check
  and cannot merge.
- **SC-2:** A well-formed entry (the #48 standard) passes structurally and merges.
- **SC-3:** The semantic advisory comment appears on RunLore-opened PRs, flagging
  a transient-noise or cause-mismatch entry; absence of a model key degrades to
  structural-only without failing CI.
- **SC-4:** A malformed entry merged into `runlore-kb` is surfaced at load time
  via `catalog_invalid_entries_total > 0` + a WARN, with the catalog still
  serving the valid entries.
- **SC-5:** `docs/kb-validation.md` documents the criteria + workflow; the
  `accepted` label convention is applied to merged entries.
- **SC-6:** `./...` tests + `golangci-lint` green; no change to `meetsBar`
  behaviour (existing curator tests unchanged).
