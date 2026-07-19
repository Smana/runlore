# OKF Staleness (status + last_validated) Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Parse the `status` and `last_validated` OKF frontmatter fields `docs/design.md` §Learn promises (today unparsed — audit N6), make recall honor `status` (retired/draft entries never fire) and down-weight stale entries by age — closing the KB's missing time dimension.

**Architecture:** Loader-parses-everything, recall-filters: `catalog.Entry` gains `Status`/`LastValidated`, the index still contains every entry (so `kb_search`/`kb_get` MCP consumers keep seeing retired knowledge — with its status visible), and the recall path skips non-active entries at the structural pre-filter plus applies a single step-down confidence multiplier when the entry's freshness date exceeds a configurable horizon. Absent fields reproduce today's behavior exactly (fail-safe). Age never *rejects* on its own — the existing confirm/verify backstops remain the hard gates; staleness only lowers delivered confidence.

**Tech Stack:** Go (toolchain go1.26.5); existing `internal/{catalog,kbvalidate,investigate,kbmcp,forge/github,config,app}`; `gopkg.in/yaml.v3`.

## Global Constraints

- Quality gate before every commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — `0 issues`, `gofmt -l` empty; plus `go test -race` on touched packages.
- SPDX header `// SPDX-License-Identifier: Apache-2.0` line 1 of new `.go` files.
- Conventional Commits; **no co-author trailer**.
- **Absent frontmatter fields = today's behavior exactly**: no `status` ⇒ active; no `last_validated` ⇒ fall back to `timestamp`; neither ⇒ no age penalty. Unknown `status` values ⇒ active (OKF §9 foreign-bundle tolerance, mirroring `kbvalidate.WarnInvalid`).
- Malformed dates/status are load-time **warnings**, never errors (one odd entry never empties the catalog).
- N4/N5 seams: the retirement pass (`2026-07-19-kb-retirement.md`) *writes* `status: retired` — this plan makes it effective. Stamping `last_validated` on confirmations belongs to the 👎-recovery work (N5); here the curator only stamps it at entry creation.

---

### Task 1: Parse `status` + `last_validated` into `catalog.Entry`

**Files:**
- Modify: `internal/catalog/entry.go` (Entry struct), `internal/catalog/load.go` (`parseEntry` meta struct + return)
- Test: `internal/catalog/load_test.go` (extend)

**Interfaces:**
- Produces: `Entry.Status string` (raw frontmatter value, "" when absent), `Entry.LastValidated string` (raw string like `Timestamp` — parsing to time happens at the consumer, keeping the loader tolerant). Plus a regression test proving unknown keys (`okf_version`) never error (already true: `yaml.Unmarshal` without `KnownFields` ignores unknown keys — pin it with a test so it stays true).

- [ ] **Step 1: Write the failing test** (in `load_test.go`, alongside existing `parseEntry`/`Load` tests — match their style):

```go
func TestLoadParsesStatusAndLastValidated(t *testing.T) {
	dir := t.TempDir()
	entry := `---
type: Incident
title: retired one
status: retired
last_validated: 2026-01-10
okf_version: "0.1"
---
Body.
`
	if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte(entry), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, skipped, err := Load(dir)
	if err != nil || len(skipped) != 0 || len(entries) != 1 {
		t.Fatalf("entries=%d skipped=%v err=%v", len(entries), skipped, err)
	}
	e := entries[0]
	if e.Status != "retired" || e.LastValidated != "2026-01-10" {
		t.Errorf("Status=%q LastValidated=%q, want retired / 2026-01-10", e.Status, e.LastValidated)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/catalog/ -run TestLoadParsesStatusAndLastValidated -v`
Expected: FAIL — `e.Status undefined`

- [ ] **Step 3: Implement.** `entry.go` — add to `Entry` (with doc comments matching the file's style):

```go
	// Status is frontmatter: status — the entry's lifecycle state ("", "active",
	// "retired", "draft", or any foreign value). Recall treats anything other than
	// retired/draft as active (OKF §9: consumers tolerate unknown vocabulary), so
	// absent-or-unknown behaves exactly as before the field existed.
	Status string
	// LastValidated is frontmatter: last_validated — when a human last confirmed the
	// entry still works (date or RFC3339; "" when absent). Kept as the raw string,
	// like Timestamp: the loader stays tolerant, consumers parse.
	LastValidated string
```

`load.go` — add to the `meta` struct: `Status string \`yaml:"status"\`` and `LastValidated string \`yaml:"last_validated"\``; carry both into the returned `Entry`.

- [ ] **Step 4: Run the package tests**

Run: `go test ./internal/catalog/ -count=1`
Expected: PASS (new + existing — `entryText` is untouched: status/dates must NOT leak into the BM25/embedding corpus).

- [ ] **Step 5: Commit**

```bash
git add internal/catalog/entry.go internal/catalog/load.go internal/catalog/load_test.go
git commit -m "feat(catalog): parse status + last_validated OKF frontmatter"
```

---

### Task 2: Load-time warnings for odd status / malformed dates

**Files:**
- Modify: `internal/kbvalidate/kbvalidate.go` (`ValidateStructural`)
- Test: `internal/kbvalidate/kbvalidate_test.go` (extend)

**Interfaces:**
- Consumes: `Entry.Status`, `Entry.LastValidated` (Task 1); `parseEntryDate` shared helper is produced HERE (exported from `catalog`, see below) so kbvalidate and recall share one date grammar.

First, add to `internal/catalog/entry.go` (it is the type's home; both consumers import catalog already):

```go
// ParseEntryDate parses an OKF entry date: RFC3339 or bare date (2006-01-02).
// Empty input returns the zero time and ok=false, distinct from malformed.
func ParseEntryDate(s string) (time.Time, bool) {
	if s == "" {
		return time.Time{}, false
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, true
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, true
	}
	return time.Time{}, false
}
```

- [ ] **Step 1: Failing tests** — in `kbvalidate_test.go`: (a) `status: bogus` yields a `SeverityWarning` on field `status` (never an error — assert `HasErrors` stays false for an otherwise-valid entry); (b) `last_validated: "not-a-date"` yields a `SeverityWarning` on field `last_validated`; (c) valid `retired`/`draft`/`active`/empty and valid dates yield no new issues. In `catalog`'s `load_test.go`: table test for `ParseEntryDate` (RFC3339, bare date, empty→!ok, garbage→!ok).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/kbvalidate/ ./internal/catalog/ -run 'Status|Date' -v`
Expected: FAIL — missing warnings / `undefined: catalog.ParseEntryDate`

- [ ] **Step 3: Implement** in `ValidateStructural`, next to the existing warn checks:

```go
	if s := strings.TrimSpace(e.Status); s != "" {
		switch s {
		case "active", "retired", "draft":
		default:
			addWarn("status", fmt.Sprintf("unknown status %q (known: active, retired, draft); treated as active", s))
		}
	}
	if e.LastValidated != "" {
		if _, ok := catalog.ParseEntryDate(e.LastValidated); !ok {
			addWarn("last_validated", fmt.Sprintf("unparseable date %q (want RFC3339 or 2006-01-02); age down-weighting will ignore it", e.LastValidated))
		}
	}
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/kbvalidate/ ./internal/catalog/ -count=1`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/catalog/entry.go internal/catalog/load_test.go internal/kbvalidate/
git commit -m "feat(kbvalidate): advisory warnings for status and last_validated"
```

---

### Task 3: Recall skips retired/draft entries

**Files:**
- Modify: `internal/investigate/recall.go` (structural pre-filter in `lookupWithUsage`, ~line 157; agreement loop in `nearMissExcluding`, ~line 296)
- Test: `internal/investigate/recall_test.go` (extend)

**Interfaces:**
- Produces: `func entryActive(e catalog.Entry) bool` — the single status predicate both loops use.

Decision (from the audit): **index everything, filter at recall.** Filtering at load would silently hide retired entries from `kb_search`/`kb_get` (MCP consumers doing KB archaeology legitimately want them, status-visible — Task 5); recall is where "must not fire" semantics live. The near-miss lead skips them too: an entry retired for being wrong must not be re-injected as a "possibly-related lead".

- [ ] **Step 1: Failing tests** — using the package's existing catalog/recall test fixtures: (a) a `Status: "retired"` entry that would otherwise be the structurally-agreeing winner is skipped and the runner-up (or `no_resource_match` rejection) results; assert the reject reason when it was the only candidate; (b) same for `"draft"`; (c) `Status: ""` and `Status: "SomeForeignState"` fire exactly as today (tolerance pin); (d) `nearMiss` never returns a retired entry.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/investigate/ -run 'TestRecall.*Status|TestNearMiss.*Retired' -v`
Expected: FAIL

- [ ] **Step 3: Implement.** In `recall.go`:

```go
// entryActive reports whether an entry may participate in recall. Only the two
// known inactive states are excluded — an absent or foreign status stays active
// (OKF §9 tolerance), so pre-status catalogs behave byte-for-byte as before.
func entryActive(e catalog.Entry) bool {
	s := strings.TrimSpace(strings.ToLower(e.Status))
	return s != "retired" && s != "draft"
}
```

In `lookupWithUsage`'s pre-filter loop:

```go
	for _, h := range hits {
		if !entryActive(h.Entry) {
			continue
		}
		if entryAgrees(req.Workload, h.Entry, r.RequireWorkloadMatch) != matchNone {
			agreeing = append(agreeing, h)
		}
	}
```

Same `!entryActive → continue` guard at the top of `nearMissExcluding`'s candidate loop.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/investigate/ -count=1`
Expected: PASS (existing recall/eval tests unchanged — no fixture carries a status).

- [ ] **Step 5: Commit**

```bash
git add internal/investigate/recall.go internal/investigate/recall_test.go
git commit -m "feat(recall): retired/draft entries never fire or lead"
```

---

### Task 4: Age-aware confidence down-weighting

**Files:**
- Modify: `internal/investigate/recall.go` (Recall struct + gate), `internal/config/config.go` (InstantRecall), `internal/config/load.go` (no default needed — 0 disables), `internal/app/investigate.go` (wiring)
- Test: `internal/investigate/recall_test.go`, `internal/config/config_test.go`

**Interfaces:**
- Produces: `Recall.StaleAfter time.Duration` (0 = disabled), `Recall.Now func() time.Time` (nil ⇒ `time.Now`; injectable clock, the `Lifecycle.Now` pattern); const `staleFactor = 0.75`; config key `catalog.instant_recall.stale_after` (Duration).

Placement: **after** the outcome-decay block, before the final log/return in `lookupWithUsage` (~line 253) — it is an independent multiplicative signal, and outcome decay's floor rejection must keep priority (track record beats calendar). One step-down, no curve zoo:

```go
	// Age down-weight: an entry no human has validated within StaleAfter carries
	// its years visibly. One multiplicative step — never a rejection: confirm and
	// verify remain the hard gates against a genuinely drifted answer, this only
	// stops a five-year-old runbook looking as confident as yesterday's. Freshness
	// is last_validated, else timestamp; a dateless or unparseable entry is exempt
	// (fail-safe: absent fields = pre-staleness behavior).
	if r.StaleAfter > 0 {
		now := time.Now
		if r.Now != nil {
			now = r.Now
		}
		fresh := e.LastValidated
		if fresh == "" {
			fresh = e.Timestamp
		}
		if t, ok := catalog.ParseEntryDate(fresh); ok && now().Sub(t) > r.StaleAfter {
			conf = clampF(conf*staleFactor, 0, 0.90)
			if r.Log != nil {
				r.Log.Info("recall: stale entry down-weighted", "entry_id", e.Path, "validated", fresh, "stale_after", r.StaleAfter)
			}
		}
	}
```

- [ ] **Step 1: Failing tests** — fixed clock; entry with `LastValidated` older than `StaleAfter` fires with `conf' = conf*0.75` (compare against the same lookup with `StaleAfter: 0`); fallback to `Timestamp` when `LastValidated` empty; no dates ⇒ untouched; `StaleAfter: 0` ⇒ untouched; a stale entry still FIRES (never rejected by age alone). Config test: `stale_after: 720h` round-trips; absent ⇒ 0.

- [ ] **Step 2: Run to verify failure** — `go test ./internal/investigate/ -run Stale -v` → FAIL.

- [ ] **Step 3: Implement**: struct fields + const + gate block above; config field `StaleAfter Duration \`yaml:"stale_after"\`` on `InstantRecall` with a doc comment (`// down-weight a recall whose last_validated/timestamp is older than this; 0 disables`); wiring in `app/investigate.go` next to the other InstantRecall fields: `recall.StaleAfter = cfg.Catalog.InstantRecall.StaleAfter.Std()` and include it in the "instant recall enabled" log line.

- [ ] **Step 4: Run tests** — `go test ./internal/investigate/ ./internal/config/ ./internal/app/ -count=1 -race` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/investigate/recall.go internal/investigate/recall_test.go internal/config/ internal/app/investigate.go
git commit -m "feat(recall): stale_after age down-weighting for recalled entries"
```

---

### Task 5: Surface status over MCP + stamp last_validated at creation

**Files:**
- Modify: `internal/kbmcp/kbmcp.go` (search + get result structs, ~lines 30/44 and their fills at ~118/148)
- Modify: `internal/forge/github/github.go` (`kbFrontmatter` ~line 414, `renderEntry`)
- Test: `internal/kbmcp/kbmcp_test.go`, `internal/forge/github/github_test.go` (extend)

- [ ] **Step 1: Failing tests** — kbmcp: a retired entry appears in `kb_search`/`kb_get` results with `"status":"retired"` (and absent `status` key when empty — use `omitempty`); github: `renderEntry` output frontmatter contains `last_validated: <timestamp>` when the KBEntry carries a timestamp.

- [ ] **Step 2: Run to verify failure** — `go test ./internal/kbmcp/ ./internal/forge/github/ -run 'Status|LastValidated' -v` → FAIL.

- [ ] **Step 3: Implement.** kbmcp: add `Status string \`json:"status,omitempty"\`` + `LastValidated string \`json:"last_validated,omitempty"\`` to both result structs; fill from the entry (retired entries stay searchable BY DESIGN — MCP consumers see the state and judge; recall is where the firing ban lives). github: add to `kbFrontmatter`:

```go
	// last_validated: stamped at entry creation (= timestamp); refreshed by humans
	// or by future confirmation flows (see the downvote-recovery plan). Recall's
	// stale_after down-weighting reads it — a never-revalidated entry ages visibly.
	LastValidated string `yaml:"last_validated,omitempty"`
```

and set it wherever the frontmatter is populated from a `providers.KBEntry` (same value as `Timestamp`).

- [ ] **Step 4: Run tests** — `go test ./internal/kbmcp/ ./internal/forge/github/ -count=1` → PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/kbmcp/ internal/forge/github/
git commit -m "feat(kb): surface entry status over MCP; stamp last_validated at creation"
```

---

### Task 6: Docs — close the design drift

**Files:**
- Modify: `docs/design.md` (§Learn, ~line 264), `docs/learning-loop.md`, `docs/configuration.md`, `docs/mcp.md` (result fields)

- [ ] **Step 1: Update.** `design.md`: the promise "entries carry `status`, `confidence`, `last_validated`" now matches reality for `status`/`last_validated` — reword to state exactly what is parsed and honored; for `confidence`, state it is a write-side extension key (curator emits it) deliberately NOT read back: recall derives trust from the live outcome track record, and a static authored confidence would fight that dynamic signal. `learning-loop.md`: add staleness to the decay section (status filter, stale_after step-down, the confirm/verify backstop argument). `configuration.md`: `catalog.instant_recall.stale_after`. `mcp.md`: new result fields.

- [ ] **Step 2: Full gate + commit**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: clean, `0 issues`.

```bash
git add docs/
git commit -m "docs: status/last_validated are real — close the design.md §Learn drift"
```

---

## Self-review checklist (run after writing code)

- Fail-safe pinned by tests: absent status ⇒ active; foreign status ⇒ active + warning; absent dates ⇒ no penalty; StaleAfter 0 ⇒ off; age never rejects.
- One date grammar (`catalog.ParseEntryDate`) and one status predicate (`entryActive`) — no duplicated parsing.
- BM25/embedding corpus untouched (`entryText` unchanged) — status/date changes never force a re-embed or alter retrieval ranking.
- MCP consumers still see retired entries, status-visible (decision documented in Task 3).
- N4 (writes `status: retired`) becomes effective through Task 3; N5 seam (revalidation stamping) named, not built.
