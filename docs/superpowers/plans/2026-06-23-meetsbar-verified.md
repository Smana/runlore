# `Verified`/provenance in the curation merge bar Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Only adversarially-reviewed, actionable findings may be drafted into the shared KB: surface the verify outcome as `Investigation.Verified` and require it — plus a causing/fixing provenance anchor — in the curator's `meetsBar`.

**Architecture:** `applyVerdicts` sets `Verified = len(kept) > 0`; `meetsBar` requires `Verified` and that the top cause carries a `ChangeRef` or `SuggestedAction`. The verify pass is always-on in production, so this only newly excludes unverified/symptom-only findings.

**Tech Stack:** Go 1.26, standard library only.

## Global Constraints

- Go 1.26, no new dependencies.
- `Verified` is `true` only when the adversarial review ran AND ≥1 root cause survived; every best-effort early return in `verifyFindings` leaves it `false`.
- `meetsBar` provenance is `ChangeRef != "" || SuggestedAction != ""` (OR, not AND — non-GitOps incidents must still curate).
- Recalled findings (skipped by `Curate` before `meetsBar`) and eval (never curates) are out of the gate's path — do not touch them.
- After each task: `go build ./... && go vet ./... && go test ./...` green and `gofmt -l .` empty.

---

### Task 1: `Investigation.Verified` field + set it in the verify pass

**Files:**
- Modify: `internal/providers/providers.go`
- Modify: `internal/investigate/verify.go`
- Test: `internal/investigate/verify_test.go`

**Interfaces:**
- Produces: `Investigation.Verified bool`, set `true` in `applyVerdicts` iff a cause survived.

- [ ] **Step 1: Strengthen the verify tests to assert `Verified`**

Read `internal/investigate/verify_test.go`. It has `TestVerifyRejectsCorrelationFinding` (all causes rejected → `len(got.RootCauses)==0`) and `TestVerifyDowngradesUnproven` (a cause survives). Add assertions:

- In the all-rejected test, after the existing `len(got.RootCauses) != 0` check, add:
```go
	if got.Verified {
		t.Fatal("a finding with no surviving cause must not be marked Verified")
	}
```
- In the downgrade/survivor test, after confirming the cause survived, add:
```go
	if !got.Verified {
		t.Fatal("a finding with a surviving reviewed cause must be marked Verified")
	}
```

If the survivor test's captured variable differs (e.g. `got` vs a pointer), match the file's existing style.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/investigate/ -run 'TestVerifyRejectsCorrelationFinding|TestVerifyDowngradesUnproven' -v`
Expected: FAIL — `Verified` undefined (compile error).

- [ ] **Step 3: Add the field and set it**

In `internal/providers/providers.go`, add to the `Investigation` struct (after `RecalledEntry`):
```go
	Verified      bool     // true when the adversarial verify pass ran and a root cause survived it
```

In `internal/investigate/verify.go`, in `applyVerdicts`, set the flag where the surviving causes are finalized — immediately after `inv.RootCauses = kept` (and before the confidence recompute is fine; place it right after the assignment):
```go
	inv.RootCauses = kept
	inv.Verified = len(kept) > 0
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/investigate/ -run 'TestVerifyRejectsCorrelationFinding|TestVerifyDowngradesUnproven' -v`
Expected: PASS.

- [ ] **Step 5: Full package + gofmt**

Run: `go test ./internal/investigate/ && gofmt -l internal/investigate/ internal/providers/`
Expected: PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/providers/providers.go internal/investigate/verify.go internal/investigate/verify_test.go
git commit -m "feat(investigate): mark Investigation.Verified when a cause survives review"
```

---

### Task 2: Require `Verified` + provenance in `meetsBar`

**Files:**
- Modify: `internal/curator/curator.go`
- Test: `internal/curator/curator_test.go`

**Interfaces:**
- Consumes: `Investigation.Verified` (Task 1), `Hypothesis.ChangeRef`, `Hypothesis.SuggestedAction`.

- [ ] **Step 1: Update the fixture and add the failing tests**

In `internal/curator/curator_test.go`:

1. Update the `goodFinding()` fixture so it represents a verified finding — add `Verified: true` to the `Investigation` literal (it already has `ChangeRef` and `SuggestedAction` on its top cause, so it satisfies provenance). Without this, the existing happy-path test would break once `meetsBar` requires `Verified`.

2. Add these tests (mirror the existing test construction — `fakeForge`, `newCurator`/`Curator{...}` with `MinConfidence`, the logger helper):

```go
func TestCurateUnverifiedDropsNoArtifact(t *testing.T) {
	inv := goodFinding()
	inv.Verified = false // identical to the happy-path finding, but verify did not confirm it
	f := &fakeForge{}
	c := &Curator{Forge: f, MinConfidence: 0.75, Log: testLogger()}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if f.openedPR != nil {
		t.Fatal("an unverified finding must not draft a KB PR")
	}
}

func TestCurateSymptomOnlyDropsNoArtifact(t *testing.T) {
	inv := goodFinding()
	inv.Verified = true
	inv.RootCauses[0].ChangeRef = ""       // no causing-change anchor
	inv.RootCauses[0].SuggestedAction = "" // no fixing-action anchor
	f := &fakeForge{}
	c := &Curator{Forge: f, MinConfidence: 0.75, Log: testLogger()}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if f.openedPR != nil {
		t.Fatal("a symptom-only finding (no provenance) must not draft a KB PR")
	}
}

func TestCurateVerifiedWithSuggestedActionOnlyOpensPR(t *testing.T) {
	inv := goodFinding()
	inv.Verified = true
	inv.RootCauses[0].ChangeRef = ""               // no GitOps change...
	inv.RootCauses[0].SuggestedAction = "scale up" // ...but a fixing action anchors it
	f := &fakeForge{}
	c := &Curator{Forge: f, MinConfidence: 0.75, Log: testLogger()}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if f.openedPR == nil {
		t.Fatal("a verified finding with a fixing action must curate (provenance is OR)")
	}
}
```

Match the actual helper names in the file: use whatever the existing tests use to build the `Curator` and the logger (e.g. an inline `slog.New(slog.NewTextHandler(io.Discard, nil))` if there is no `testLogger()`), and the real `fakeForge` field name for the opened PR (the #11 map showed `openedPR *providers.KBEntry`). If the existing tests construct the `Curator` via a helper like `newCurator(f, cat)`, reuse it and set `MinConfidence` the way they do.

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/curator/ -run 'TestCurateUnverified|TestCurateSymptomOnly|TestCurateVerifiedWithSuggestedActionOnly' -v`
Expected: FAIL — `meetsBar` does not yet require `Verified`/provenance (the unverified and symptom-only findings would currently open a PR).

- [ ] **Step 3: Update `meetsBar`**

Replace `meetsBar` in `internal/curator/curator.go` with:

```go
// meetsBar is the file-time QUALITY gate (not the merge condition): an
// adversarially-reviewed, confident root cause with cited evidence AND a provenance
// anchor (a causing change or a fixing action). The resolved/accepted MERGE
// condition is enforced later by the curate agent + the human.
func meetsBar(inv providers.Investigation, minConf float64) bool {
	if !inv.Verified {
		return false // only findings that survived the adversarial review reach the shared catalog
	}
	if inv.Confidence < minConf || len(inv.RootCauses) == 0 {
		return false
	}
	top := inv.RootCauses[0]
	if top.Summary == "" || len(top.Evidence) == 0 {
		return false
	}
	// Provenance: actionable knowledge, not a symptom restatement — anchored to a
	// causing change (ChangeRef) or a fixing action (SuggestedAction).
	return top.ChangeRef != "" || top.SuggestedAction != ""
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/curator/ -run 'TestCurateUnverified|TestCurateSymptomOnly|TestCurateVerifiedWithSuggestedActionOnly|TestCurateNovelHighQuality|TestCurateLowQuality' -v`
Expected: PASS (new gates fire; the happy-path test still opens a PR because `goodFinding()` now sets `Verified: true`).

- [ ] **Step 5: Full suite + vet + gofmt**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l .`
Expected: all PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/curator/curator.go internal/curator/curator_test.go
git commit -m "feat(curator): meetsBar requires Verified + provenance before drafting"
```

---

## Self-Review

**Spec coverage:**
- `Verified bool` field → Task 1. ✅
- Set in `applyVerdicts` (len(kept)>0) → Task 1. ✅
- `meetsBar` requires `Verified` → Task 2. ✅
- Provenance OR (ChangeRef|SuggestedAction) → Task 2. ✅
- `goodFinding` fixture updated; unverified + symptom-only + suggested-action-only tests → Task 2. ✅
- Recall/eval untouched → no edits there. ✅

**Placeholder scan:** all code shown; test-helper references explicitly "match the existing file" (the #16/#11 maps confirm `fakeForge.openedPR`). ✅

**Type consistency:** `Verified` defined in Task 1 (providers) and consumed in Task 2 (curator) and the Task 1 verify tests; `ChangeRef`/`SuggestedAction` are existing `Hypothesis` fields. ✅
