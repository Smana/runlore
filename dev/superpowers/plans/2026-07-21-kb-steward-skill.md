# kb-steward Claude Code Skill Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the `kb-steward` Claude Code skill as a plugin served from the runlore repo, per the approved spec `dev/superpowers/specs/2026-07-21-kb-steward-skill-design.md`.

**Architecture:** The repo root gains `.claude-plugin/marketplace.json` (making the repo a Claude Code plugin marketplace named `runlore`); the plugin lives under `plugins/kb-steward/` with one skill (`skills/kb-steward/SKILL.md` routing four flows + three `references/` files). Two Go tests in `internal/catalog/skillcontract_test.go` keep the skill's documented OKF contract and the manifests from drifting. One new docs page + two cross-links.

**Tech Stack:** Markdown/JSON content + Go stdlib tests (`encoding/json`, `reflect`). No new dependencies, no CI changes (`ci.yaml` already runs `go test -race ./...`).

## Global Constraints

- Quality gate before EVERY commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — golangci-lint must print `0 issues`, `gofmt -l .` must print nothing.
- Every new `.go` file starts with the line `// SPDX-License-Identifier: Apache-2.0` (CI enforces SPDX headers).
- Conventional commit messages, English, **no AI attribution / no Co-Authored-By lines**.
- No new CI workflows or steps — all repo-side validation is plain `go test`.
- Facts about the loader in skill content must match `internal/catalog/load.go` + `internal/kbvalidate/kbvalidate.go` (they do as of main `19cb16f`; the drift test pins the field list).
- Execution branch: create `feat/kb-steward-skill` from `docs/kb-steward-skill-design` (so spec + plan + implementation land in one PR).

---

### Task 1: OKF format reference + loader drift guard

**Files:**
- Create: `plugins/kb-steward/skills/kb-steward/references/okf-format.md`
- Create: `internal/catalog/skillcontract_test.go`

**Interfaces:**
- Produces: const `pluginRoot = "../../plugins/kb-steward"` in `internal/catalog/skillcontract_test.go` (Task 4 appends to this file and reuses the const).
- Produces: the marker pair `<!-- parsed-fields:start -->` / `<!-- parsed-fields:end -->` inside `okf-format.md` — the drift test parses backticked field names between them.
- Consumes: `catalog.Entry` (internal/catalog/entry.go:27-51) — 10 frontmatter fields + `Body` + `Path`.

- [ ] **Step 1: Write the failing drift test**

Create `internal/catalog/skillcontract_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"unicode"
)

// The kb-steward Claude Code plugin is served from this repo (see
// docs/kb-steward.md). These tests keep the skill's documented OKF contract
// and the plugin manifests from drifting as the loader evolves.

const pluginRoot = "../../plugins/kb-steward"

var parsedFieldsRE = regexp.MustCompile("`([a-z_]+)`")

// TestOKFFormatDocMatchesLoader pins the skill's documented frontmatter field
// list to what the loader actually parses (the frontmatter fields of Entry).
func TestOKFFormatDocMatchesLoader(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join(pluginRoot, "skills/kb-steward/references/okf-format.md"))
	if err != nil {
		t.Fatalf("read okf-format.md: %v", err)
	}
	doc := string(raw)
	start := strings.Index(doc, "<!-- parsed-fields:start -->")
	end := strings.Index(doc, "<!-- parsed-fields:end -->")
	if start < 0 || end < 0 || end < start {
		t.Fatal("okf-format.md must contain a <!-- parsed-fields:start -->…<!-- parsed-fields:end --> block")
	}
	documented := map[string]bool{}
	for _, m := range parsedFieldsRE.FindAllStringSubmatch(doc[start:end], -1) {
		documented[m[1]] = true
	}

	parsed := map[string]bool{}
	et := reflect.TypeOf(Entry{})
	for i := 0; i < et.NumField(); i++ {
		name := et.Field(i).Name
		if name == "Body" || name == "Path" { // not frontmatter
			continue
		}
		parsed[snakeCase(name)] = true
	}

	for f := range parsed {
		if !documented[f] {
			t.Errorf("loader parses frontmatter field %q but okf-format.md does not document it", f)
		}
	}
	for f := range documented {
		if !parsed[f] {
			t.Errorf("okf-format.md documents field %q but the loader does not parse it", f)
		}
	}
}

func snakeCase(s string) string {
	var b strings.Builder
	for i, r := range s {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('_')
			}
			r = unicode.ToLower(r)
		}
		b.WriteRune(r)
	}
	return b.String()
}
```

Note: `load_test.go` in this package uses `package catalog` (not `_test`) — this file matches it.

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/catalog/ -run TestOKFFormatDocMatchesLoader -v`
Expected: FAIL — `read okf-format.md: open ../../plugins/kb-steward/skills/kb-steward/references/okf-format.md: no such file or directory`

- [ ] **Step 3: Create the reference document**

Create `plugins/kb-steward/skills/kb-steward/references/okf-format.md`:

````markdown
# OKF entry format — what RunLore actually parses

RunLore's catalog loader reads every `*.md` file in the KB repo (recursively),
splits YAML frontmatter from the markdown body, and indexes the result for
recall during investigations. This file is the contract: what the loader
parses, what the merge gate enforces, and what recall rewards.

## Parsed frontmatter fields

The loader parses exactly these fields (anything else is tolerated and
ignored — OKF consumers must accept unknown keys):

<!-- parsed-fields:start -->
`type` · `title` · `description` · `resource` · `alert_resource` · `tags` ·
`timestamp` · `fingerprint` · `status` · `last_validated`
<!-- parsed-fields:end -->

| Field | Required | Why it matters |
|---|---|---|
| `type` | yes (gate) | `Incident`, `Playbook`, or `Concept` (capitalized). Decides the write directory and body requirements. |
| `title` | yes (gate) | Single line, ≤120 chars. Indexed as its own search field — make it the scoped symptom ("KubeContainerOOMKilled for oom-app"), never a vague theme ("OOM issues"). |
| `description` | yes (gate) | One or two sentences carrying the words an alert or a query would contain — prime recall signal. |
| `resource` | Incident: yes (gate) | `namespace/name` of the affected workload, no whitespace. Drives recall's structural workload filter. Omit only deliberately, for platform-wide knowledge (the "scopeless" tier). |
| `alert_resource` | no | Set when the alert fired on a different resource than the fault (symptom pod vs faulty config). An additional way for recall to match, never a replacement. |
| `tags` | no (warned if empty) | Include the workload kind and namespace at minimum; reuse the platform's tag vocabulary. Lexical + vector signal, not a hard filter. |
| `timestamp` | no | RFC3339 or `YYYY-MM-DD`. Fallback freshness date when `last_validated` is absent. |
| `fingerprint` | no | Opaque dedup identity on agent-drafted entries. Omit on hand-written entries. |
| `status` | no | `active` (or absent) / `retired` / `draft`. `retired` and `draft` are excluded from recall; anything else counts as active. Retire — don't delete — entries that no longer apply. |
| `last_validated` | no | Date a human last confirmed the entry works. Older than the deployment's `catalog.instant_recall.stale_after` ⇒ confidence down-weighted (×0.75), never rejected. Set it to today whenever you author or revalidate. |

Agent-drafted entries may also carry extension fields such as `confidence` or
`provenance`. They are legal OKF (unknown keys are ignored) but the loader
does not parse them — never rely on them for recall.

## Files & directories

- Write entries under the type directory: `incidents/`, `playbooks/`,
  `concepts/` (a write-side convention matching RunLore's own PRs — the
  loader itself reads every directory).
- Filename: `<kebab-title-slug>-<short-suffix>.md`. RunLore uses the first 8
  fingerprint chars as suffix; for hand-written entries any short stable
  suffix works (e.g. the date `20260721`). The suffix keeps two entries
  sharing a title from colliding.
- Reserved names the loader SKIPS: `index.md`, `log.md`, any `readme.md`,
  dotfiles, and hidden directories. Never put knowledge in those files.

## Body requirements (merge gate)

- Body must be non-empty for every type.
- **Incident** bodies must contain `## Symptom`, `## Cause`, and
  `## Resolution` sections; `## Investigate` (how to confirm this entry
  applies) is strongly recommended.
- Recall indexes ONE text corpus per entry: title + description + resource +
  tags + body. Everything you want the entry found by must appear in one of
  those.

## Example

```markdown
---
type: Incident
title: KubeContainerOOMKilled for oom-app
description: Container 'hog' is OOMKilled because its memory limit (100Mi) is below actual usage.
resource: shop-prod/oom-app
tags: [deployment, shop-prod, oomkilled, memory]
timestamp: "2026-07-03T09:14:00Z"
status: active
last_validated: "2026-07-21"
---

## Symptom
`KubeContainerOOMKilled` fires for pod oom-app-*; container restarts with reason OOMKilled.

## Cause
Memory limit 100Mi is below the working set (~180Mi) after the v2 image bump.

## Investigate
`kubectl -n shop-prod describe pod -l app=oom-app` → last state OOMKilled;
compare `container_memory_working_set_bytes` against the limit.

## Resolution
Raise the limit to 256Mi in the HelmRelease values (gitops repo:
`apps/shop/oom-app/values.yaml`), reconcile, confirm restarts stop.
```
````

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/catalog/ -run TestOKFFormatDocMatchesLoader -v`
Expected: PASS

- [ ] **Step 5: Run the full quality gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all pass, `gofmt -l .` prints nothing, golangci-lint prints `0 issues`.

- [ ] **Step 6: Commit**

```bash
git add internal/catalog/skillcontract_test.go plugins/kb-steward/skills/kb-steward/references/okf-format.md
git commit -m "feat(skill): kb-steward OKF format reference + loader drift guard"
```

---

### Task 2: Entry quality checklist + interview guides

**Files:**
- Create: `plugins/kb-steward/skills/kb-steward/references/entry-quality-checklist.md`
- Create: `plugins/kb-steward/skills/kb-steward/references/interview-guides.md`

**Interfaces:**
- Consumes: field semantics from Task 1's `okf-format.md` (referenced by name, not duplicated).
- Produces: the two reference filenames exactly as SKILL.md (Task 3) cites them: `references/entry-quality-checklist.md`, `references/interview-guides.md`.

- [ ] **Step 1: Create the checklist**

Create `plugins/kb-steward/skills/kb-steward/references/entry-quality-checklist.md`:

```markdown
# Entry quality checklist

Run every draft through this list before writing it to the repo. The first
block mirrors RunLore's merge gate (failures block the PR); the second is
what separates a recall-able entry from noise.

## Gate (must pass — RunLore validates these on merge)

- [ ] `type` is `Incident`, `Playbook`, or `Concept`
- [ ] `title` is a single line, ≤120 chars, scoped to one symptom/procedure
- [ ] `description` present
- [ ] `resource` present for Incident (`namespace/name`, no whitespace)
- [ ] body non-empty; Incident body has `## Symptom`, `## Cause`, `## Resolution`

## Recall strength (the actual point)

- [ ] description contains the words an alert or on-call query would use
      (alert name, error string, resource kind)
- [ ] tags non-empty: workload kind + namespace + platform vocabulary
- [ ] one concern per entry — split anything covering two symptoms/procedures
- [ ] `## Investigate` section tells a stranger how to confirm the entry applies
- [ ] `last_validated` set to today
- [ ] near-duplicate check done: searched the catalog for the resource and
      the alert/title keywords; preferred updating an existing entry over
      adding a twin
- [ ] scopeless (no `resource`) only for genuinely platform-wide knowledge

## Secret scan (always, before writing the file)

Search the draft for and redact:

- private key blocks (`-----BEGIN … PRIVATE KEY-----`)
- cloud credentials (`AKIA…`, `aws_secret_access_key`, service-account JSON)
- tokens/headers (`Authorization:`, `Bearer `, `xox[a-z]-`, `ghp_`, `glpat-`)
- `password=`, `token=`, `secret=` values in URLs, env dumps, or configs
- kubeconfig blobs and base64 strings longer than ~40 chars of unclear origin

Replace with `<redacted>`. If a secret already reached git or chat via the
user's paste, tell them so they can rotate it.

## Unknowns

Write `unknown` rather than a plausible guess. A wrong "cause" recalled with
confidence during a future incident is worse than a gap.
```

- [ ] **Step 2: Create the interview guides**

Create `plugins/kb-steward/skills/kb-steward/references/interview-guides.md`:

````markdown
# Interview guides

One question at a time. Skip anything AGENTS.md already answers. Prefer
multiple-choice when the options are enumerable; open-ended otherwise.
Every answer is a candidate entry — keep a running list and confirm the
batch with the SRE before drafting.

## Seed interview (flow 1)

### 1. Platform inventory
- Which clusters exist, and what is each for? (name, environment, criticality)
- GitOps engine — Flux or ArgoCD? Where is the gitops repo, how is it laid out?
- Cloud/on-prem, regions, anything multi-?
- What workload classes run here (stateless services, stateful, batch/ML)?

### 2. Observability & alerting
- Metrics / logs / traces stacks — and which are actually trustworthy?
- Where do alerts route (Alertmanager → Slack/PagerDuty…), who gets paged?
- Which alerts are known-noisy, and why? (each answer → a Concept entry —
  exactly what keeps an agent from chasing ghosts)

### 3. Conventions
- Namespace scheme, naming conventions, labels/annotations that mean something
- Environment promotion flow — how does a change reach prod?
- A tag vocabulary for KB entries: agree on ~10 tags now, record them in the
  platform profile

### 4. Failure modes & tribal knowledge
- What breaks regularly? What's the fix nobody wrote down?
- What would you tell a new on-call in their first week?
- Which dependencies bite (external SaaS, DNS, certificates, quotas, IPs)?
- Any "never do X" rules — and what happened when someone did?

### 5. Existing material
- Runbooks, ADRs, wiki pages worth converting — where?
- For each: still accurate? Who owns it? One entry per symptom/procedure,
  never a bulk dump — confirm with the SRE which are worth converting.

### Mapping answers to types
- Facts about how the platform is built/behaves → `Concept`
- Step-by-step procedures ("how to drain", "how to rotate") → `Playbook`
- Past outages worth remembering → `Incident` (use the post-incident map)

### Platform profile (AGENTS.md at the KB root)

Write or refresh it after the interview. Give it OKF frontmatter so RunLore
can recall it as a scopeless Concept, and future kb-steward sessions can
skip answered questions:

```markdown
---
type: Concept
title: Platform profile
description: <one sentence: stack, cloud, GitOps engine, environments — use real names>
tags: [platform, profile, <gitops-engine>, <cloud>]
last_validated: "<today>"
---

## Platform
- Clusters: …
- GitOps: …

## Observability
- …

## Conventions
- Namespaces: …
- KB tag vocabulary: …

## Known-noisy alerts
- …

## Interview log
- <date>: seeded sections 1–5 (kb-steward)
```

## Post-incident interview (flow 2)

Confirm first: **is the incident resolved?** If it's live, stop — capture
happens after resolution; diagnosis is RunLore's (or the human's) job.

1. **Trigger** — what fired or how was it noticed? Exact alert name / error
   string (these words become the description).
2. **Impact & timeline** — start, detection, mitigation, resolution times;
   blast radius.
3. **What changed** — deploys, config, infra around onset? Git SHAs or
   releases if known.
4. **Root cause — with pushback.** Don't accept the first answer:
   - "Is that the cause, or the first symptom you noticed?"
   - "If you rolled only that back, would it recur?"
   - "What allowed the system to fail this way?" (keep asking why —
     usually 3–5 levels)
5. **Fix** — what actually resolved it? Temporary mitigation or permanent?
6. **Verification** — how would a teammate confirm the fix worked? (becomes
   `## Resolution` / `## Investigate` content)
7. **Prevention** — guardrails added, tickets opened, alerts tuned?

Then: near-duplicate check (search the catalog for the resource + alert
keywords). Update-and-revalidate an existing entry when one matches;
otherwise draft one Incident entry, plus a Playbook only when the procedure
generalizes beyond this resource.
````

- [ ] **Step 3: Verify the drift guard still passes (checklist references fields by name)**

Run: `go test ./internal/catalog/ -run TestOKFFormatDocMatchesLoader -v`
Expected: PASS (the marker block lives only in okf-format.md, untouched).

- [ ] **Step 4: Commit**

```bash
git add plugins/kb-steward/skills/kb-steward/references/entry-quality-checklist.md plugins/kb-steward/skills/kb-steward/references/interview-guides.md
git commit -m "feat(skill): entry quality checklist + interview guides"
```

---

### Task 3: SKILL.md router

**Files:**
- Create: `plugins/kb-steward/skills/kb-steward/SKILL.md`

**Interfaces:**
- Consumes: the three reference filenames from Tasks 1–2 (cited as `references/<name>.md`).
- Produces: the skill installed as `/kb-steward:kb-steward` (directory name = invoke name). Frontmatter `description` must stay well under the 1,536-char listing truncation.

- [ ] **Step 1: Create SKILL.md**

Create `plugins/kb-steward/skills/kb-steward/SKILL.md`:

```markdown
---
name: kb-steward
description: Steward a RunLore OKF knowledge catalog. Use when seeding a knowledge base with platform/company context, writing up a RESOLVED incident (post-mortem / RCA capture), triaging RunLore's KB pull requests, or curating stale entries. Interviews the SRE and writes recall-grade OKF entries. Never diagnoses live incidents — that is RunLore's job.
---

# kb-steward — RunLore knowledge catalog steward

You steward a RunLore knowledge catalog: a git repo of OKF entries (markdown +
YAML frontmatter) that RunLore recalls during automated investigations. Every
entry you write is either recall signal or noise — frontmatter is the product.

**Boundary:** you capture knowledge about resolved situations and platform
context. If the user is mid-incident and wants the cause found, say so and
stop: live diagnosis is RunLore's job (or the human's), not this skill's.

## Setup (every flow)

1. **Locate the catalog** — the repo configured as `forge.kb_repo` in their
   RunLore install. If the current directory (or a parent) holds OKF entries
   (`incidents/`, `playbooks/`, `concepts/`), use it; otherwise ask for the
   path. Never guess; never scaffold a new repo without explicit confirmation.
2. **Read `AGENTS.md`** at the KB root if present — the platform profile from
   earlier sessions. Don't re-ask what it answers.
3. **Read the references**: `references/okf-format.md` and
   `references/entry-quality-checklist.md` (all flows);
   `references/interview-guides.md` (flows 1–2).

## Choose the flow

| Situation | Flow |
|---|---|
| New or thin catalog; onboarding RunLore | 1 — Seed |
| An incident was just resolved | 2 — Post-incident capture |
| Open `runlore`-labelled KB PRs to review | 3 — PR triage |
| Periodic cleanup | 4 — Maintenance |

Ask which applies when it isn't obvious.

## Flow 1 — Seed

Convert platform context and tribal knowledge into many small, scoped entries.

1. Interview per the seed map in the interview guide — one question at a time.
2. For existing material (runbooks, ADRs, wiki) the user points at: read it
   and split it per symptom/procedure. One concern per entry — never a
   platform bible.
3. Draft entries per okf-format.md; run every draft through the checklist
   (including the secret scan).
4. Write or refresh `AGENTS.md` per the template in the interview guide.
5. Deliver via the git flow.

Target for a first sitting: 5–15 entries the SRE confirms are true.

## Flow 2 — Post-incident capture

1. Confirm the incident is resolved (else stop — see Boundary).
2. Interview per the post-incident map — push back on root cause
   (symptom vs cause, five whys).
3. Near-duplicate check: search the catalog for the resource and alert/title
   keywords. Prefer updating + revalidating an existing entry.
4. Draft one Incident entry (`## Symptom` / `## Cause` / `## Investigate` /
   `## Resolution`); add a Playbook only if the procedure generalizes.
5. Checklist + secret scan, then the git flow.

## Flow 3 — PR triage

1. List open KB PRs: `gh pr list --label runlore` in the KB repo.
2. Per PR: run the proposed entry through the checklist; scan the catalog for
   near-duplicates; then recommend one of merge / refine (offer the concrete
   frontmatter or body fix) / close (say why: duplicate, benign churn, not
   knowledge).
3. You recommend — the human merges. Never merge or close yourself unless
   explicitly told to.
4. If most of the queue is noise, say so and point at the config levers:
   `forge.skip_verdicts: ["no_action"]`, `forge.min_confidence`,
   `forge.dup_score` (see RunLore's docs/reviewing-knowledge.md).

## Flow 4 — Maintenance

1. Scan entries for: `status: draft` leftovers, missing/empty `tags`, and
   `last_validated` (or `timestamp`) older than the deployment's
   `catalog.instant_recall.stale_after` (ask the user what it is set to).
2. For each stale entry ask: still true? → bump `last_validated` to today.
   No longer applies? → set `status: retired` (retire, never delete — recall
   excludes it, git history keeps it).
3. Fix weak frontmatter while you're there (tags, scoped titles) — but never
   change the meaning of an entry without the SRE confirming.
4. Deliver via the git flow, one PR for the whole pass.

## Git flow (all writes)

- Branch `kb-steward/<short-slug>`; commit; open a PR with `gh pr create`.
  PR body: what was captured or changed and why, with the entry list. No AI
  attribution.
- **Never merge and never push to the default branch.** Nothing enters the KB
  without a human merge — the same rule RunLore itself follows. A solo
  maintainer may explicitly ask for a direct commit; comply and say so.

## Hard rules

- **No fabrication.** Interview answers and files the user provides are the
  only sources of fact. Unknowns are written as unknowns.
- **Secret scan every draft** (list in the checklist) before it touches disk.
- **Small entries.** Split anything covering two concerns.
- **Respect reserved files:** never write knowledge into `index.md`,
  `log.md`, or `readme.md` — the loader skips them.
```

- [ ] **Step 2: Sanity-check the frontmatter description length**

Run: `awk '/^description:/{print length($0)}' plugins/kb-steward/skills/kb-steward/SKILL.md`
Expected: a number well under 1536 (it is ~360).

- [ ] **Step 3: Commit**

```bash
git add plugins/kb-steward/skills/kb-steward/SKILL.md
git commit -m "feat(skill): kb-steward SKILL.md router"
```

---

### Task 4: Marketplace + plugin manifests with validity test

**Files:**
- Create: `.claude-plugin/marketplace.json`
- Create: `plugins/kb-steward/.claude-plugin/plugin.json`
- Modify: `internal/catalog/skillcontract_test.go` (append the manifest test + `encoding/json` import)

**Interfaces:**
- Consumes: `pluginRoot` const and file layout from Tasks 1–3.
- Produces: marketplace name `runlore`, plugin name `kb-steward` — the exact install coordinates `kb-steward@runlore` used by the docs (Task 5).

- [ ] **Step 1: Append the failing manifest test**

In `internal/catalog/skillcontract_test.go`, add `"encoding/json"` to the import block (gofmt orders it first), then append:

```go
// TestPluginManifestsValid keeps the marketplace/plugin manifests installable:
// docs tell users to run `/plugin install kb-steward@runlore`.
func TestPluginManifestsValid(t *testing.T) {
	var marketplace struct {
		Name  string `json:"name"`
		Owner struct {
			Name string `json:"name"`
		} `json:"owner"`
		Plugins []struct {
			Name        string `json:"name"`
			Source      string `json:"source"`
			Description string `json:"description"`
		} `json:"plugins"`
	}
	raw, err := os.ReadFile("../../.claude-plugin/marketplace.json")
	if err != nil {
		t.Fatalf("read marketplace.json: %v", err)
	}
	if err := json.Unmarshal(raw, &marketplace); err != nil {
		t.Fatalf("marketplace.json is not valid JSON: %v", err)
	}
	if marketplace.Name != "runlore" {
		t.Errorf("marketplace name = %q, want %q", marketplace.Name, "runlore")
	}
	if marketplace.Owner.Name == "" {
		t.Error("marketplace owner.name must be set")
	}
	if len(marketplace.Plugins) != 1 || marketplace.Plugins[0].Name != "kb-steward" {
		t.Fatalf("plugins = %+v, want exactly one entry named kb-steward", marketplace.Plugins)
	}
	if got, want := marketplace.Plugins[0].Source, "./plugins/kb-steward"; got != want {
		t.Errorf("plugin source = %q, want %q", got, want)
	}
	if marketplace.Plugins[0].Description == "" {
		t.Error("plugin description must be set (shown in /plugin listings)")
	}

	var plugin struct {
		Name string `json:"name"`
	}
	raw, err = os.ReadFile(filepath.Join(pluginRoot, ".claude-plugin/plugin.json"))
	if err != nil {
		t.Fatalf("read plugin.json: %v", err)
	}
	if err := json.Unmarshal(raw, &plugin); err != nil {
		t.Fatalf("plugin.json is not valid JSON: %v", err)
	}
	if plugin.Name != "kb-steward" {
		t.Errorf("plugin name = %q, want kb-steward", plugin.Name)
	}

	for _, p := range []string{
		"skills/kb-steward/SKILL.md",
		"skills/kb-steward/references/okf-format.md",
		"skills/kb-steward/references/entry-quality-checklist.md",
		"skills/kb-steward/references/interview-guides.md",
	} {
		if _, err := os.Stat(filepath.Join(pluginRoot, p)); err != nil {
			t.Errorf("plugin file missing: %s: %v", p, err)
		}
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/catalog/ -run TestPluginManifestsValid -v`
Expected: FAIL — `read marketplace.json: open ../../.claude-plugin/marketplace.json: no such file or directory`

- [ ] **Step 3: Create the manifests**

Create `.claude-plugin/marketplace.json`:

```json
{
  "name": "runlore",
  "owner": { "name": "Smaine Kahlouch" },
  "description": "Claude Code plugins shipped with RunLore",
  "plugins": [
    {
      "name": "kb-steward",
      "source": "./plugins/kb-steward",
      "description": "Interview-driven stewardship of a RunLore OKF knowledge catalog: seeding, post-incident capture, PR triage, maintenance"
    }
  ]
}
```

Create `plugins/kb-steward/.claude-plugin/plugin.json`:

```json
{
  "name": "kb-steward",
  "displayName": "RunLore KB Steward",
  "version": "0.1.0",
  "description": "Interviews SREs and turns platform knowledge into recall-grade OKF entries for a RunLore knowledge catalog",
  "author": { "name": "Smaine Kahlouch" },
  "homepage": "https://github.com/Smana/runlore/blob/main/docs/kb-steward.md",
  "repository": "https://github.com/Smana/runlore",
  "license": "Apache-2.0",
  "keywords": ["runlore", "sre", "knowledge-base", "okf"]
}
```

- [ ] **Step 4: Run the test to verify it passes**

Run: `go test ./internal/catalog/ -run TestPluginManifestsValid -v`
Expected: PASS

- [ ] **Step 5: Run the full quality gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all pass, no gofmt output, `0 issues`.

- [ ] **Step 6: Commit**

```bash
git add .claude-plugin/marketplace.json plugins/kb-steward/.claude-plugin/plugin.json internal/catalog/skillcontract_test.go
git commit -m "feat(skill): plugin + marketplace manifests with validity test"
```

---

### Task 5: Docs page + cross-links

**Files:**
- Create: `docs/kb-steward.md`
- Modify: `README.md` (footer nav, the "## Docs" block around line 243)
- Modify: `docs/reviewing-knowledge.md` (tip after the intro blockquote, before the first `---`)

**Interfaces:**
- Consumes: install coordinates `kb-steward@runlore` (Task 4), test path `internal/catalog/skillcontract_test.go` (Tasks 1/4).

- [ ] **Step 1: Create the docs page**

Create `docs/kb-steward.md`:

````markdown
# kb-steward — a Claude Code skill for your knowledge base

> RunLore investigates and proposes knowledge automatically. **kb-steward** is
> the human half: a [Claude Code](https://code.claude.com/docs) skill that
> interviews you and turns what you know into recall-grade OKF entries.

Diagnosis stays RunLore's job — the skill only captures and curates knowledge.

## Install

```
/plugin marketplace add Smana/runlore
/plugin install kb-steward@runlore
```

Update later with `/plugin update kb-steward`. No binary, no server change —
the plugin is served from this repo.

## What it does

| You say | It does |
|---|---|
| "Seed my RunLore knowledge base" | Structured interview about your platform (clusters, GitOps, alerting, conventions, tribal knowledge) → small scoped Concept/Playbook entries, plus an `AGENTS.md` platform profile so it never re-asks |
| "Write up the incident we just resolved" | RCA interview with pushback (symptom vs cause, five whys) → one gate-passing Incident entry — updating a near-duplicate instead when one exists |
| "Review RunLore's KB PRs" | Quality + duplicate check per PR, merge/refine/close recommendation — and points at `forge.skip_verdicts` & friends when the queue is systematically noisy |
| "Clean up the catalog" | Finds stale or weak entries, proposes revalidation or `status: retired` |

## Ground rules the skill enforces on itself

- **PR by default, never merges** — nothing enters the KB without a human
  merge, the same gate RunLore's own findings go through.
- **No fabrication** — unknowns are recorded as unknowns.
- **Secret scan** before any draft is written (SREs paste logs; logs leak).

## Staying honest

The skill documents the exact frontmatter contract the catalog loader parses.
A test in this repo (`internal/catalog/skillcontract_test.go`) fails if the
two drift apart, and `claude plugin validate .` checks the plugin layout.

See also: [Reviewing & approving RunLore's knowledge](reviewing-knowledge.md)
— the merge side of the same loop.
````

- [ ] **Step 2: Link it from the README footer nav**

In `README.md`, in the `## Docs` block, change:

```markdown
✅ [Reviewing knowledge](docs/reviewing-knowledge.md) ·
```

to:

```markdown
✅ [Reviewing knowledge](docs/reviewing-knowledge.md) · 🧑‍🔧 [KB steward skill](docs/kb-steward.md) ·
```

(The nav is `·`-separated; keep the rest of the line untouched.)

- [ ] **Step 3: Link it from reviewing-knowledge.md**

In `docs/reviewing-knowledge.md`, insert after the intro paragraph ("...the knowledge is yours, reviewed, and in your Git.") and before the first `---`:

```markdown
> **Guided workflow:** the [kb-steward Claude Code skill](kb-steward.md) walks
> you through PR triage, post-incident capture, and seeding the KB with your
> platform's context — interview-style, PR-gated like everything else.
```

- [ ] **Step 4: Commit**

```bash
git add docs/kb-steward.md README.md docs/reviewing-knowledge.md
git commit -m "docs: kb-steward skill install page + cross-links"
```

---

### Task 6: End-to-end verification (no new code)

**Files:** none created — verification only; fix-forward any failures.

- [ ] **Step 1: Validate the plugin layout with the official validator**

Run from the repo root: `claude plugin validate .`
Expected: validation passes (no errors; warnings acceptable — record any).

- [ ] **Step 2: Full quality gate on the final tree**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all pass, no gofmt output, `0 issues`.

- [ ] **Step 3: Cold-run the skill (writing-skills verification)**

Dispatch a FRESH subagent with no other context than this prompt:

> Read `plugins/kb-steward/skills/kb-steward/SKILL.md` and follow it exactly.
> Scenario: I'm an SRE. My KB checkout is at `<scratch>/kb` (create it empty
> with `git init` first). We just resolved an incident: alert
> `KubePodCrashLooping` fired for `payments/checkout-api`; a config change
> (commit abc123, new env var `PAYMENT_TIMEOUT=1`) made every request time
> out; rolling the value back to `30` fixed it; we verified by watching the
> restart count stop and p99 latency recover. I don't know why the bad value
> was merged. Capture this incident.

Check the produced entry against every Gate item in
`references/entry-quality-checklist.md`, plus:
- file is under `incidents/` with a slug + suffix filename
- `resource: payments/checkout-api`; description contains `KubePodCrashLooping`
- the unknown ("why was it merged") is recorded as unknown, not invented
- the work landed on a `kb-steward/*` branch, not `main` (a PR can't be opened
  in a scratch repo — the subagent should say so rather than push anywhere)

Expected: all checks pass. If any fail, fix SKILL.md/references (not the
scenario), re-run with a fresh subagent, and note the fix in the PR body.

- [ ] **Step 4: Commit any fixes**

```bash
git add -A plugins/ && git commit -m "fix(skill): tighten kb-steward flow after cold-run verification"
```

(Skip if Step 3 passed clean on the first run.)

---

### Task 7: Portable core — neutrality guard, non-Claude install paths, git-flow hardening

Added after Tasks 1-6, from two findings: the maintainer asked that the skill
not be Claude-only, and the Task 6 cold run exposed an ambiguous git flow.

**Files:**
- Modify: `plugins/kb-steward/skills/kb-steward/SKILL.md` (Git flow section)
- Modify: `docs/kb-steward.md` (add "Using it with another agent" section)
- Modify: `internal/catalog/skillcontract_test.go` (append the neutrality test)

**Interfaces:**
- Consumes: `pluginRoot` const from Task 1.
- Produces: `TestSkillContentIsHarnessNeutral` — a guard later edits must satisfy.

- [ ] **Step 1: Write the failing neutrality test**

Append to `internal/catalog/skillcontract_test.go`:

```go
// TestSkillContentIsHarnessNeutral keeps the skill body portable: the plugin
// manifests and SKILL.md's frontmatter are Claude Code packaging, but the
// instructions themselves must run under any agent that can read markdown
// (see docs/kb-steward.md, "Using it with another agent").
func TestSkillContentIsHarnessNeutral(t *testing.T) {
	// Vocabulary that would tie the instructions to one harness.
	banned := []string{
		"Claude", "claude", "/plugin", "slash command",
		"TodoWrite", "Task tool", "subagent", "Cursor", "Copilot", "Codex",
	}
	files := []string{
		"skills/kb-steward/SKILL.md",
		"skills/kb-steward/references/okf-format.md",
		"skills/kb-steward/references/entry-quality-checklist.md",
		"skills/kb-steward/references/interview-guides.md",
	}
	for _, f := range files {
		raw, err := os.ReadFile(filepath.Join(pluginRoot, f))
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		body := stripFrontmatter(string(raw)) // frontmatter is packaging metadata
		for _, word := range banned {
			if strings.Contains(body, word) {
				t.Errorf("%s: harness-specific term %q in skill body — the portable core must not name a specific agent or its tools", f, word)
			}
		}
	}
}

// stripFrontmatter drops a leading YAML frontmatter block, which is harness
// packaging metadata rather than instruction content.
func stripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---\n") {
		return s
	}
	if end := strings.Index(s[4:], "\n---"); end >= 0 {
		return s[4+end:]
	}
	return s
}
```

Add `"strings"` to the import block if not already present.

- [ ] **Step 2: Run the test**

Run: `go test ./internal/catalog/ -run TestSkillContentIsHarnessNeutral -v`
Expected: PASS immediately — the content is already neutral (verified by grep
before this task was written). This test is a regression guard, not a driver.
If it FAILS, the failure names the offending file and term: remove the term
from the skill body (never weaken the banned list to make it pass).

- [ ] **Step 3: Harden the git flow in SKILL.md**

In SKILL.md's "Git flow (all writes)" section, replace the first bullet:

```markdown
- Branch `kb-steward/<short-slug>`; commit; open a PR with `gh pr create`.
  PR body: what was captured or changed and why, with the entry list. No AI
  attribution.
```

with:

```markdown
- Run every git command against the KB repo explicitly (`git -C <kb-repo>`,
  `gh --repo <kb-remote>`) — never rely on the shell's current directory,
  which may be a different repository.
- Before any push or PR, confirm the KB repo actually has a remote and that
  it is the catalog you were pointed at (`git -C <kb-repo> remote -v`). If it
  has none, stop after committing the local branch and tell the user — never
  push, and never substitute another remote.
- Branch `kb-steward/<short-slug>`; commit; push the branch; then open a PR
  with `gh pr create`. PR body: what was captured or changed and why, with
  the entry list. No AI attribution.
```

- [ ] **Step 4: Add the non-Claude install section to `docs/kb-steward.md`**

Insert after the "## Install" section, before "## What it does":

````markdown
## Using it with another agent

The plugin above is packaging, not a dependency. The skill is plain markdown
with no harness-specific instruction in it — a Go test
(`TestSkillContentIsHarnessNeutral`) fails the build if that ever stops being
true — so any coding agent that reads files can run it:

1. Copy `plugins/kb-steward/skills/kb-steward/` (SKILL.md plus `references/`)
   into your KB repo, e.g. as `.kb-steward/`:

   ```bash
   git clone --depth 1 https://github.com/Smana/runlore /tmp/runlore
   cp -r /tmp/runlore/plugins/kb-steward/skills/kb-steward .kb-steward
   ```

2. Point your agent at it — either directly ("follow `.kb-steward/SKILL.md`")
   or, better, from your KB's `AGENTS.md`, which most agents read
   automatically:

   ```markdown
   ## Knowledge-base conventions
   When adding or curating entries in this repo, follow `.kb-steward/SKILL.md`.
   ```

SKILL.md's YAML frontmatter is metadata for Claude Code's skill loader; other
agents ignore it harmlessly. Nothing else in the file assumes a runtime.

Reading the catalog is already agent-agnostic by a different route: `lore mcp
<kb-dir>` serves `kb_search` and `kb_get` to any MCP client, with no cluster,
model, or config — see [MCP](mcp.md). This skill is the writing half.
````

- [ ] **Step 5: Run the full quality gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all pass, no gofmt output, `0 issues`.

- [ ] **Step 6: Commit**

```bash
git add internal/catalog/skillcontract_test.go plugins/kb-steward/skills/kb-steward/SKILL.md docs/kb-steward.md
git commit -m "feat(skill): guard harness neutrality, document non-Claude use, harden git flow"
```

---

## Execution notes

- Branch: `feat/kb-steward-skill` from `docs/kb-steward-skill-design`; one PR
  to `main` carrying spec + plan + implementation (single concern).
- PR body in English, no AI attribution, cite the spec and this plan.
- Out of scope (per spec): live diagnosis, deep triage/maintenance flows,
  `lore` CLI involvement, publishing to any external marketplace.
- **Post-merge (maintainer, not this PR):** dogfood the seed interview on the
  Cloud Native Ref platform KB and verify RunLore recall surfaces the new
  entries — the spec's v1 acceptance ("≥5 recall-firing entries in one
  sitting") is only provable against a live install.
