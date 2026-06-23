# Entity Precision in Track A — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the replay eval (Track A) score root-cause correctness deterministically at the entity level — recall-gated precision with an over-claim/false-positive penalty — instead of pure keyword presence.

**Architecture:** Two additive fields on `Expected` (`RootCauseEntities`, `Distractors`); a `claimText` helper that isolates what the investigation *blamed* (Title + each hypothesis Summary/SuggestedAction, excluding Evidence/Unresolved); and entity recall + over-claim scoring folded into `Score`, gating `Result.Pass`. Backward compatible: cases without `RootCauseEntities` score exactly as today.

**Tech Stack:** Go 1.26, stdlib `testing` (no testify). Case-insensitive substring matching, same style as the existing `MustContain` scoring.

**Spec:** `docs/superpowers/specs/2026-06-23-entity-precision-trackA-design.md`

**Branch:** `feat/entity-precision-trackA` (already checked out; the spec is already committed there).

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `internal/eval/case.go` | replay case schema | add `RootCauseEntities []string` + `Distractors []string` to `Expected` |
| `internal/eval/score.go` | deterministic scoring | add `OverClaimed []string` to `Result`; add `claimText` helper; fold entity recall + over-claim into `Score`, gate `Pass` |
| `internal/eval/eval_test.go` | scoring tests | add 5 entity-scoring tests + 1 parse assertion (co-located with the existing `TestScore`/`TestLoad`) |

The schema + scoring + tests are one cohesive change (the tests won't compile until the fields exist), so it is one implementation task (T1) plus whole-tree verification (T2).

---

### Task 1: Entity recall + over-claim scoring in Track A

**Files:**
- Modify: `internal/eval/case.go` (`Expected` struct, currently lines 25-28)
- Modify: `internal/eval/score.go` (`Result` struct lines 10-15; `Score` lines 19-33; add `claimText`)
- Test: `internal/eval/eval_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/eval/eval_test.go` (it already imports `providers`, `os`, `path/filepath`, `testing`):

```go
func TestEntityRecallAllNamedPasses(t *testing.T) {
	inv := providers.Investigation{
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{{
			Summary:         "apps/web crashed because rds/prod-db hit its connection cap",
			SuggestedAction: "raise max_connections on rds/prod-db",
		}},
	}
	r := Score("c", inv, Expected{
		RootCauseEntities: []string{"apps/web", "rds/prod-db"},
		Distractors:       []string{"apps/worker"},
	})
	if !r.Pass || len(r.Missing) != 0 || len(r.OverClaimed) != 0 {
		t.Fatalf("expected clean pass, got %+v", r)
	}
}

func TestEntityMissingFails(t *testing.T) {
	inv := providers.Investigation{Confidence: 0.8, RootCauses: []providers.Hypothesis{{Summary: "apps/web is unhealthy"}}}
	r := Score("c", inv, Expected{RootCauseEntities: []string{"apps/web", "rds/prod-db"}})
	if r.Pass {
		t.Fatal("expected fail: rds/prod-db was not named as a cause")
	}
	if len(r.Missing) != 1 || r.Missing[0] != "rds/prod-db" {
		t.Fatalf("expected rds/prod-db in Missing, got %+v", r.Missing)
	}
}

func TestOverClaimDistractorBlamedFails(t *testing.T) {
	// All expected entities ARE named, but a distractor is also blamed → over-claim → fail.
	inv := providers.Investigation{
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{{Summary: "root cause is apps/web and apps/worker, both talking to rds/prod-db"}},
	}
	r := Score("c", inv, Expected{
		RootCauseEntities: []string{"apps/web", "rds/prod-db"},
		Distractors:       []string{"apps/worker"},
	})
	if r.Pass {
		t.Fatalf("expected fail on over-claim, got pass: %+v", r)
	}
	if len(r.OverClaimed) != 1 || r.OverClaimed[0] != "apps/worker" {
		t.Fatalf("expected apps/worker over-claimed, got %+v", r.OverClaimed)
	}
}

func TestDistractorInEvidenceNotPenalized(t *testing.T) {
	// The distractor appears only in Evidence/Unresolved, never in the claim → not an over-claim.
	inv := providers.Investigation{
		Confidence: 0.8,
		RootCauses: []providers.Hypothesis{{
			Summary:  "apps/web crashed due to rds/prod-db saturation",
			Evidence: []string{"ruled out apps/worker: its error rate was flat"},
		}},
		Unresolved: []string{"whether apps/worker retries amplified load"},
	}
	r := Score("c", inv, Expected{
		RootCauseEntities: []string{"apps/web", "rds/prod-db"},
		Distractors:       []string{"apps/worker"},
	})
	if !r.Pass {
		t.Fatalf("a distractor only in evidence/unresolved must not penalize, got %+v", r)
	}
	if len(r.OverClaimed) != 0 {
		t.Fatalf("no over-claim expected, got %+v", r.OverClaimed)
	}
}

func TestNoEntitiesBackwardCompatible(t *testing.T) {
	// A case with only must_contain behaves exactly as before: entities ignored.
	inv := providers.Investigation{Confidence: 0.8, RootCauses: []providers.Hypothesis{{Summary: "chart bump stalled harbor-db"}}}
	r := Score("c", inv, Expected{MustContain: []string{"chart", "harbor-db"}, MinConfidence: 0.5})
	if !r.Pass || len(r.Missing) != 0 || len(r.OverClaimed) != 0 {
		t.Fatalf("a must_contain-only case should pass cleanly, got %+v", r)
	}
}

func TestLoadParsesEntities(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "db.yaml"), []byte(`
name: db-saturation
prompt: web 5xx spike
tools:
  what_changed: "no change"
expected:
  root_cause_entities: [apps/web, rds/prod-db]
  distractors: [apps/worker]
  min_confidence: 0.5
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cases, err := Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cases) != 1 ||
		len(cases[0].Expected.RootCauseEntities) != 2 ||
		len(cases[0].Expected.Distractors) != 1 {
		t.Fatalf("entity fields not parsed: %+v", cases[0].Expected)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/eval/ -run 'TestEntity|TestOverClaim|TestDistractor|TestNoEntities|TestLoadParsesEntities'`
Expected: FAIL — compile errors `unknown field RootCauseEntities in struct literal of type Expected` and `r.OverClaimed undefined`.

- [ ] **Step 3: Add the schema fields**

In `internal/eval/case.go`, replace the `Expected` struct (lines 25-28):

```go
// Expected is the RCA scoring spec for a case.
type Expected struct {
	MustContain       []string `yaml:"must_contain"`        // keywords that must appear in the findings (recall, over full findings text)
	MinConfidence     float64  `yaml:"min_confidence"`      // confidence floor (0 = no floor)
	RootCauseEntities []string `yaml:"root_cause_entities"` // entities that MUST be named as the cause (entity recall, over claim text)
	Distractors       []string `yaml:"distractors"`         // plausible-but-wrong entities that must NOT be blamed (over-claim/FP)
}
```

- [ ] **Step 4: Add `claimText`, the `OverClaimed` field, and entity scoring**

In `internal/eval/score.go`, add `OverClaimed []string` to `Result`:

```go
// Result is the score for one case.
type Result struct {
	Name        string
	Pass        bool
	Confidence  float64
	Missing     []string // expected keywords/entities not found (or an error note); includes "over-claimed: <e>" markers
	OverClaimed []string // distractor entities the investigation wrongly blamed (over-claim/FP)
}
```

Replace the `Score` function body (lines 19-33) with:

```go
// Score reports whether the investigation identifies the expected root cause.
// Keyword recall (must_contain) is matched over the full findings text. Entity
// scoring — recall over root_cause_entities and an over-claim penalty over
// distractors — is matched over the CLAIM text only (what was blamed), and engages
// only when root_cause_entities is set. A case passes when nothing is missing,
// no distractor was blamed, and confidence meets the floor.
func Score(name string, inv providers.Investigation, exp Expected) Result {
	hay := strings.ToLower(investigationText(inv))
	var missing []string
	for _, kw := range exp.MustContain {
		if !strings.Contains(hay, strings.ToLower(kw)) {
			missing = append(missing, kw)
		}
	}

	var overClaimed []string
	if len(exp.RootCauseEntities) > 0 {
		claim := strings.ToLower(claimText(inv))
		for _, e := range exp.RootCauseEntities {
			if !strings.Contains(claim, strings.ToLower(e)) {
				missing = append(missing, e)
			}
		}
		for _, d := range exp.Distractors {
			if strings.Contains(claim, strings.ToLower(d)) {
				overClaimed = append(overClaimed, d)
				missing = append(missing, "over-claimed: "+d)
			}
		}
	}

	return Result{
		Name:        name,
		Pass:        len(missing) == 0 && inv.Confidence >= exp.MinConfidence,
		Confidence:  inv.Confidence,
		Missing:     missing,
		OverClaimed: overClaimed,
	}
}

// claimText is what the investigation BLAMED: the title plus each hypothesis's
// summary and suggested action. It deliberately excludes Evidence and Unresolved
// so entity matching means "named as a cause", not "mentioned while ruling out".
func claimText(inv providers.Investigation) string {
	var b strings.Builder
	b.WriteString(inv.Title)
	for _, rc := range inv.RootCauses {
		b.WriteString(" " + rc.Summary + " " + rc.SuggestedAction)
	}
	return b.String()
}
```

(`investigationText` is unchanged and still used for `MustContain` recall.)

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/eval/`
Expected: PASS — the 6 new tests plus all pre-existing eval tests (notably `TestScore`, which uses only `MustContain` → entity block skipped → unchanged).

- [ ] **Step 6: Commit**

```bash
git add internal/eval/case.go internal/eval/score.go internal/eval/eval_test.go
git commit -m "feat(eval): deterministic entity precision + over-claim penalty in Track A replay scoring"
```

---

### Task 2: Whole-tree verification

- [ ] **Step 1: Build, test, vet**

Run:
```bash
go build ./... && go test ./... && go vet ./...
```
Expected: build clean; all tests PASS; vet clean.

No commit (verification only).

---

## Notes for the implementer

- The entire entity block (recall AND distractors) is gated on `len(exp.RootCauseEntities) > 0` — this is the backward-compat contract (spec D5). A distractors-only case is not supported in v1; that's intentional.
- Over-claimed entities are mirrored into `Missing` as `"over-claimed: <e>"` so the existing failure renderer at `cmd/lore/main.go:514-515` (`missing: <joined>`) shows *why* a case failed, with no change to that call site. `OverClaimed` carries the structured list separately for any programmatic caller.
- Matching is case-insensitive substring, identical to `MustContain`. Do NOT add word-boundary or regex matching (spec §5, YAGNI).
- Do not touch `live.go`, the `ModelJudge`, `GroundTruth`, or `coverage.go` — Track B is explicitly out of scope (spec §3.4).
- `Pass` keys off `len(missing) == 0`: because over-claims are appended to `missing`, a run that names every expected entity but also blames a distractor correctly fails.
