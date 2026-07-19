# KB Retirement Pass Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A new `lore curate` Phase-2 pass that opens a human-reviewed "retire" PR for a *merged* catalog entry whose outcome track record has decayed below the floor — closing the missing garbage-collection half of the learning loop (roadmap N4; audit: `dev/plans/2026-07-19-project-audit.md`).

**Architecture:** A `curate.Retirement` pass (pattern-identical to `Contested`: consumer-side forge interface, ledger-derived state, hidden-marker idempotency, per-item error isolation). It reads `Ledger.OpenCounts()` (keyed by entry path), applies a sustained-decay condition, and calls a new `github.Client.OpenRetirePR` that stamps `status: retired` into the entry's frontmatter on a branch and opens a labelled PR. A human merges — curation stays the load-bearing gate; nothing is auto-deleted. The decay formula moves to `outcome.Aggregate.Factor` so recall and retirement share one definition (DRY).

**Tech Stack:** Go (toolchain go1.26.5), stdlib + existing `internal/{curate,outcome,forge/github,config,app}`; `httptest` for forge-client tests.

## Global Constraints

- Quality gate before every commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — golangci-lint must report `0 issues`, `gofmt -l` empty; plus `go test -race` on touched packages.
- SPDX header `// SPDX-License-Identifier: Apache-2.0` as line 1 of every new `.go` file.
- Conventional Commits; **no co-author trailer**.
- The pass is **opt-in** (`curate.retirement.enabled: false` default) — default behavior unchanged.
- Forge-only writes; the pass never merges, never touches a human-labelled (protected) artifact, never deletes a file.
- Coordination note (N6): this pass *writes* `status: retired` frontmatter. The catalog loader/recall honoring `status` ships separately in `2026-07-19-okf-staleness.md`; until then a merged retirement PR is inert but correct. No dependency either way.

---

### Task 1: Move the decay formula to `outcome.Aggregate.Factor`

`investigate.outcomeFactor` (`internal/investigate/recall.go:495`) is the only definition of the Beta-posterior decay. Retirement must apply the *same* formula; duplicating it risks silent drift. `outcome.Aggregate` is its natural home.

**Files:**
- Modify: `internal/outcome/ledger.go` (add method next to the `Aggregate` type, ~line 958)
- Modify: `internal/investigate/recall.go:495` (delegate)
- Test: `internal/outcome/ledger_factor_test.go` (create)

**Interfaces:**
- Produces: `func (a Aggregate) Factor(k float64) float64` — used by Task 2 and by `investigate`.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: Apache-2.0

package outcome

import "testing"

func TestAggregateFactor(t *testing.T) {
	cases := []struct {
		name string
		agg  Aggregate
		k    float64
		want float64
	}{
		{"no history is the prior mean", Aggregate{}, 2.0, 0.5},
		{"single unresolved recall decays fast", Aggregate{Recalls: 1}, 2.0, 1.0 / 3.0},
		{"single downvote decays identically", Aggregate{FeedbackDown: 1}, 2.0, 1.0 / 3.0},
		{"resolving entry climbs", Aggregate{Recalls: 4, Resolved: 4}, 2.0, 5.0 / 6.0},
		{"upvotes count as successes", Aggregate{FeedbackUp: 2}, 2.0, 3.0 / 4.0},
	}
	for _, c := range cases {
		if got := c.agg.Factor(c.k); got != c.want {
			t.Errorf("%s: Factor(%v) = %v, want %v", c.name, c.k, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/outcome/ -run TestAggregateFactor -v`
Expected: FAIL — `a.Factor undefined (type Aggregate has no field or method Factor)`

- [ ] **Step 3: Add the method (below the `Aggregate` type in `ledger.go`)**

```go
// Factor is the entry's outcome-decay factor: the posterior mean of a symmetric
// Beta(k/2, k/2) prior over the success rate, folding resolves and human votes
// into one trust signal:
//
//	factor = (Resolved + FeedbackUp + k/2) / (Recalls + FeedbackUp + FeedbackDown + k)
//
// It is THE single definition of decay — recall's fire gate and the curate
// retirement pass both consume it, so they can never drift apart. See
// investigate's gate docs for the full statistical rationale.
func (a Aggregate) Factor(k float64) float64 {
	return (float64(a.Resolved+a.FeedbackUp) + k/2) / (float64(a.Recalls+a.FeedbackUp+a.FeedbackDown) + k)
}
```

- [ ] **Step 4: Delegate in `internal/investigate/recall.go`** — replace the body of `outcomeFactor` (keep the function and its doc comment; every call site and test stays valid):

```go
func outcomeFactor(recalls, resolved, up, down int, k float64) float64 {
	return outcome.Aggregate{Recalls: recalls, Resolved: resolved, FeedbackUp: up, FeedbackDown: down}.Factor(k)
}
```

Add `"github.com/Smana/runlore/internal/outcome"` to the imports of `recall.go` if not present (it is already imported by the package — check; `OutcomeStats` lives in `loop.go`'s package scope, the import may be in another file. If the compiler flags an unused/missing import, fix per its message.)

- [ ] **Step 5: Run both packages' tests**

Run: `go test ./internal/outcome/ ./internal/investigate/ -count=1`
Expected: `ok` for both — the existing `outcomeFactor` expectations in `recall_test.go` / `recall_decay_integration_test.go` must pass unchanged (this proves the formula moved without changing).

- [ ] **Step 6: Commit**

```bash
git add internal/outcome/ledger.go internal/outcome/ledger_factor_test.go internal/investigate/recall.go
git commit -m "refactor(outcome): single Aggregate.Factor definition for decay"
```

---

### Task 2: Frontmatter status stamping helper (pure function)

The forge needs to rewrite one line of an entry file without reformatting human YAML. A surgical line edit — never a re-marshal of foreign frontmatter.

**Files:**
- Create: `internal/forge/github/retire.go`
- Test: `internal/forge/github/retire_test.go`

**Interfaces:**
- Produces: `func setStatusRetired(content []byte) (out []byte, already bool, err error)` — package-private; used by Task 3's `OpenRetirePR`. `already=true` when the frontmatter already carries `status: retired` (idempotency signal); `err` non-nil when the file has no frontmatter block (never write blind).

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"strings"
	"testing"
)

func TestSetStatusRetired(t *testing.T) {
	t.Run("inserts status after the opening fence", func(t *testing.T) {
		in := "---\ntype: Incident\ntitle: t\n---\nbody\n"
		out, already, err := setStatusRetired([]byte(in))
		if err != nil || already {
			t.Fatalf("err=%v already=%v", err, already)
		}
		want := "---\nstatus: retired\ntype: Incident\ntitle: t\n---\nbody\n"
		if string(out) != want {
			t.Errorf("got:\n%s\nwant:\n%s", out, want)
		}
	})
	t.Run("replaces an existing status line in place", func(t *testing.T) {
		in := "---\ntype: Incident\nstatus: active\ntitle: t\n---\nbody\n"
		out, already, err := setStatusRetired([]byte(in))
		if err != nil || already {
			t.Fatalf("err=%v already=%v", err, already)
		}
		if !strings.Contains(string(out), "\nstatus: retired\n") || strings.Contains(string(out), "active") {
			t.Errorf("status not replaced in place:\n%s", out)
		}
	})
	t.Run("already retired reports already, content unchanged", func(t *testing.T) {
		in := "---\nstatus: retired\ntype: Incident\n---\nbody\n"
		out, already, err := setStatusRetired([]byte(in))
		if err != nil || !already {
			t.Fatalf("err=%v already=%v", err, already)
		}
		if string(out) != in {
			t.Errorf("content changed on already-retired entry")
		}
	})
	t.Run("no frontmatter is an error, never a blind write", func(t *testing.T) {
		if _, _, err := setStatusRetired([]byte("just a body\n")); err == nil {
			t.Fatal("want error on missing frontmatter")
		}
	})
	t.Run("status in the BODY does not fool the fence scan", func(t *testing.T) {
		in := "---\ntype: Incident\n---\nstatus: retired appears in prose\n"
		out, already, err := setStatusRetired([]byte(in))
		if err != nil || already {
			t.Fatalf("err=%v already=%v", err, already)
		}
		if !strings.HasPrefix(string(out), "---\nstatus: retired\n") {
			t.Errorf("status not inserted into frontmatter:\n%s", out)
		}
	})
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/forge/github/ -run TestSetStatusRetired -v`
Expected: FAIL — `undefined: setStatusRetired`

- [ ] **Step 3: Implement `internal/forge/github/retire.go`**

```go
// SPDX-License-Identifier: Apache-2.0

package github

import (
	"fmt"
	"strings"
)

// setStatusRetired stamps `status: retired` into an OKF entry's YAML frontmatter,
// editing ONLY the status line — human formatting, key order and comments are
// preserved (this file is a human-authored artifact under review; a re-marshal
// would produce an unreadable retirement diff). Scanning is fence-bounded so a
// "status:" string in the markdown body is never touched. already=true means the
// entry is retired on the base branch and no PR is needed. A file without a
// frontmatter block errors: retirement must never write blind.
func setStatusRetired(content []byte) (out []byte, already bool, err error) {
	s := string(content)
	if !strings.HasPrefix(s, "---\n") {
		return nil, false, fmt.Errorf("entry has no YAML frontmatter block")
	}
	rest := s[len("---\n"):]
	end := strings.Index(rest, "\n---")
	if end < 0 {
		return nil, false, fmt.Errorf("entry frontmatter block is unterminated")
	}
	fm, body := rest[:end], rest[end:]
	lines := strings.Split(fm, "\n")
	for i, ln := range lines {
		if key, val, ok := strings.Cut(ln, ":"); ok && strings.TrimSpace(key) == "status" {
			if strings.TrimSpace(val) == "retired" {
				return content, true, nil
			}
			lines[i] = "status: retired"
			return []byte("---\n" + strings.Join(lines, "\n") + body), false, nil
		}
	}
	return []byte("---\nstatus: retired\n" + fm + body), false, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/forge/github/ -run TestSetStatusRetired -v`
Expected: PASS (all five subtests)

- [ ] **Step 5: Commit**

```bash
git add internal/forge/github/retire.go internal/forge/github/retire_test.go
git commit -m "feat(forge): frontmatter status:retired stamping helper"
```

---

### Task 3: `github.Client.OpenRetirePR`

**Files:**
- Modify: `internal/forge/github/retire.go` (add the method)
- Test: `internal/forge/github/retire_test.go` (extend, `httptest` server — mirror the style of the existing `OpenPR` tests in `github_test.go`)

**Interfaces:**
- Consumes: `setStatusRetired` (Task 2); the existing `c.do` HTTP helper, `c.owner/c.repo/c.baseBranch` fields, `c.addLabels` (`github.go:162`), `providers.Ref`.
- Produces: `func (c *Client) OpenRetirePR(ctx context.Context, entryPath, body string) (providers.Ref, error)`. Sentinel: `var ErrAlreadyRetired = errors.New("entry already retired on base branch")` — the pass treats it as done-skip. A 404 on the entry file returns an error wrapping the status (entry deleted → pass logs+skips). Labels applied: `"runlore", "runlore-retire"`.

GitHub API sequence (all via `c.do`, matching `OpenPR` at `github.go:113-160`):
1. `GET /repos/{o}/{r}/contents/{path}?ref={base}` → `{content (base64), sha}`; decode, run `setStatusRetired`; on `already` → `ErrAlreadyRetired`.
2. `GET /repos/{o}/{r}/git/ref/heads/{base}` → base SHA (same call OpenPR makes).
3. `POST /repos/{o}/{r}/git/refs` with branch `runlore/retire-<slugified path>-<unix>`.
4. `PUT /repos/{o}/{r}/contents/{path}` with `{message, content (new, base64), branch, sha}` — the `sha` is what makes this an update, not a create.
5. `POST /repos/{o}/{r}/pulls` — title `"KB retire: <entryPath>"`, `head` branch, `base`, `body` (caller-provided; carries the track record + hidden marker).
6. `addLabels(number, ["runlore", "runlore-retire"])` — best-effort like OpenPR.

- [ ] **Step 1: Write the failing test** — an `httptest.Server` scripting the six calls; assert (a) the PUT body's decoded `content` starts with `---\nstatus: retired\n`, (b) the PUT carries the file `sha` from step 1, (c) already-retired content short-circuits with `ErrAlreadyRetired` after only the first GET, (d) a 404 contents GET returns a non-nil error and no further calls. Reuse the `newTestClient(t, srv.URL)` construction pattern from `github_test.go` (read it first; keep fakes minimal).

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/forge/github/ -run TestOpenRetirePR -v`
Expected: FAIL — `undefined: (*Client).OpenRetirePR` / `undefined: ErrAlreadyRetired`

- [ ] **Step 3: Implement the method** in `retire.go` following the sequence above; base64 via `encoding/base64.StdEncoding` (strip newlines from the GET's content before decoding: GitHub wraps at 60 chars — `strings.ReplaceAll(raw, "\n", "")`); reuse `slugify` (`github.go:545`) for the branch name; timestamp from `time.Now().Unix()` mirroring `OpenPR`.

- [ ] **Step 4: Run tests**

Run: `go test ./internal/forge/github/ -count=1`
Expected: PASS (new + all existing)

- [ ] **Step 5: Commit**

```bash
git add internal/forge/github/retire.go internal/forge/github/retire_test.go
git commit -m "feat(forge): OpenRetirePR — status:retired PR against an existing entry"
```

---

### Task 4: The `curate.Retirement` pass

**Files:**
- Create: `internal/curate/retirement.go`
- Test: `internal/curate/retirement_test.go`

**Interfaces:**
- Consumes: `outcome.Aggregate.Factor` (Task 1); forge behavior of Task 3 via its own interface.
- Produces:

```go
// RetireForge is the forge surface the Retirement pass needs (consumer-side,
// like ContestedForge — widening the shared Forge would bloat every fake).
type RetireForge interface {
	ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	ListClosedUnmergedPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error)
	OpenRetirePR(ctx context.Context, entryPath, body string) (providers.Ref, error)
}

// RetireStats is the ledger view the pass needs.
type RetireStats interface {
	OpenCounts() (map[string]outcome.Aggregate, error)
}

type Retirement struct {
	Forge           RetireForge
	Stats           RetireStats
	MinObservations int     // sustained-decay bar: total observations before retirement is considered
	Floor           float64 // retire when Factor(k) < Floor
	Prior           float64 // k — must equal recall's outcome_prior so both gates agree
	Log             *slog.Logger
}
func (p Retirement) Run(ctx context.Context) error
func retireMarker(entryPath string) string // "<!-- runlore:retire:<sha256[:8] of path> -->"
```

**Semantics (encode each as a test case):**
1. Candidate = entry path where `agg.Recalls+agg.FeedbackUp+agg.FeedbackDown >= MinObservations` **and** `agg.Factor(Prior) < Floor`. The observation floor is what makes decay *sustained* — a single bad recall (factor 0.33) must NOT retire an entry; with the defaults (MinObservations 3, Floor 0.5, Prior 2.0) the minimal retiring histories are e.g. 3 unresolved recalls (factor 0.2) or 2 recalls + 1 👎 unresolved.
2. Idempotency: skip a candidate when any OPEN `runlore-retire` PR body contains `retireMarker(path)` (one `ListPRsByLabel` call per run, cached in a local map — the Contested per-run-cache pattern).
3. Human veto: skip when any CLOSED-UNMERGED `runlore-retire` PR body contains the marker — a human rejected retirement; never re-nag (the `ClosedPRSuppression` philosophy, `recurrence.go`).
4. `ErrAlreadyRetired` from the forge → `Log.Debug`, continue (merged retirement already landed).
5. Any other per-entry forge error → `Log.Warn`, continue (per-item isolation — one flaky entry never starves the rest, mirroring `Contested.Run`).
6. Empty candidate set → zero forge calls (assert: fake records no calls).
7. Deterministic order: sort candidate paths before iterating (stable logs/tests).

- [ ] **Step 1: Write the failing tests** — table-driven with a recorded fake implementing `RetireForge` + a map-backed `RetireStats`; one test per semantic above. PR body assertion: contains the marker, the factor, the counts, and the phrase `merging this PR retires the entry` (reviewer-facing honesty; body text is authored in a `retireBody(path string, agg outcome.Aggregate, factor, floor float64, marker string) string` helper — full text in Step 3).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/curate/ -run TestRetirement -v`
Expected: FAIL — `undefined: Retirement`

- [ ] **Step 3: Implement `retirement.go`.** Body template (complete; marker last, mirroring `contestedComment`):

```go
func retireBody(path string, agg outcome.Aggregate, factor, floor float64, marker string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "**RunLore proposes retiring `%s`** — its outcome track record decayed below the trust floor.\n\n", path)
	fmt.Fprintf(&b, "| recalls | resolved | 👍 | 👎 | factor | floor |\n|---|---|---|---|---|---|\n| %d | %d | %d | %d | %.2f | %.2f |\n\n",
		agg.Recalls, agg.Resolved, agg.FeedbackUp, agg.FeedbackDown, factor, floor)
	b.WriteString("Recall already rejects this entry at the same floor, so every recurrence pays a full investigation; ")
	b.WriteString("merging this PR retires the entry (`status: retired` — it stops being recallable but stays in git history). ")
	b.WriteString("Close this PR to keep the entry: RunLore will not propose it again.\n\n")
	b.WriteString(marker)
	return b.String()
}
```

- [ ] **Step 4: Run tests (with race)**

Run: `go test ./internal/curate/ -count=1 -race`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/curate/retirement.go internal/curate/retirement_test.go
git commit -m "feat(curate): retirement pass — retire PRs for decayed merged entries"
```

---

### Task 5: Config + wiring + docs

**Files:**
- Modify: `internal/config/config.go` (the `Curate` struct, ~line 1174)
- Modify: `internal/config/load.go` (defaults) and `config.go` `Validate` (bounds)
- Modify: `internal/app/curate.go` (wire the pass inside the ledger-configured block, after `Contested`)
- Modify: `docs/learning-loop.md` (retirement section), `docs/configuration.md` (new keys)
- Test: `internal/config/config_test.go`, `internal/app/curate_test.go` (extend existing patterns)

- [ ] **Step 1: Failing config test** — assert defaults (`MinObservations=3`, `Floor=0.5`, `Prior=2.0` applied only when `Enabled`), and `Validate` errors: `curate.retirement.floor` outside `(0,1]`, `min_observations < 1`.

- [ ] **Step 2: Extend the `Curate` struct:**

```go
type Curate struct {
	StaleAfter          Duration   `yaml:"stale_after"`
	RecurrenceThreshold int        `yaml:"recurrence_threshold"`
	// Retirement opens a human-reviewed "retire" PR for a merged catalog entry whose
	// outcome factor stayed below Floor across at least MinObservations observations.
	// Opt-in: the pass writes to the forge on a schedule, and retirement is a
	// judgment call an operator must consciously enable. Prior/Floor default to the
	// recall gate's outcome_prior/outcome_floor defaults (2.0 / 0.5) so the two
	// gates agree unless deliberately tuned apart.
	Retirement Retirement `yaml:"retirement"`
}

// Retirement configures the curate retirement pass.
type Retirement struct {
	Enabled         bool    `yaml:"enabled"`
	MinObservations int     `yaml:"min_observations"` // sustained-decay bar (default 3)
	Floor           float64 `yaml:"floor"`            // retire below this factor (default 0.5)
	Prior           float64 `yaml:"prior"`            // Beta prior strength k (default 2.0)
}
```

Defaults in `load.go` (inside `applyDefaults`, guarded by `Enabled`); validation in `Validate` mirrors neighboring checks (exact error strings asserted in the test).

- [ ] **Step 3: Wire in `internal/app/curate.go`** — inside the existing `if cfg.Outcome.LedgerPath != ""` block, after `curate.Contested{...}`:

```go
if cfg.Curate.Retirement.Enabled {
	agent.Passes = append(agent.Passes, curate.Retirement{
		Forge:           forge,
		Stats:           ledger,
		MinObservations: cfg.Curate.Retirement.MinObservations,
		Floor:           cfg.Curate.Retirement.Floor,
		Prior:           cfg.Curate.Retirement.Prior,
		Log:             log,
	})
}
```

(`*github.Client` satisfies `RetireForge` via Task 3 + existing list methods; `*outcome.Ledger` satisfies `RetireStats` — both assertions are free compile-time checks; add `var _ curate.RetireForge = (*github.Client)(nil)` in `curate_test.go` if no existing pattern conflicts.)

- [ ] **Step 4: Docs** — `docs/learning-loop.md`: extend the lifecycle section: validation (decay) existed, retirement now closes the loop; a retired entry stays in git history; humans veto by closing the PR. `docs/configuration.md`: the four new keys with defaults. Update the "deliberately incomplete" list if it names missing GC.

- [ ] **Step 5: Full gate + commit**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: build/vet/test clean, empty gofmt output, `0 issues`.

```bash
git add internal/config/ internal/app/curate.go docs/learning-loop.md docs/configuration.md
git commit -m "feat(curate): opt-in retirement pass config + wiring + docs"
```

---

## Self-review checklist (run after writing code)

- Spec coverage: sustained condition ✓ (Task 4.1), idempotent ✓ (4.2), human veto ✓ (4.3), protected artifacts untouched ✓ (pass never labels/closes; Lifecycle's protected-label logic untouched), frontmatter-only edit ✓ (Task 2), opt-in ✓ (Task 5), N6 seam documented ✓.
- Both gates share one formula: `Aggregate.Factor` is the only decay definition (Task 1).
- No placeholder steps; every code step carries real code; forge test scripts all six HTTP calls.
