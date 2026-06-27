# Curation resolution join keyed on dedup fingerprint — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Re-key `LedgerResolutionChecker.IsResolved` off the deterministic dedup fingerprint (already on the curated PR's body marker and now plumbed onto the outcome episode), so a reworded re-investigation of the same incident still auto-queues its resolved PR. Keep a whitespace-robust title fallback for PRs/ledgers written before this change.

**Architecture:** `outcome` stays a leaf data package — it gains a pure `DupFingerprint` string field on `Event`/`Episode`, copied through replay. The producer (`cmd/lore/main.go`, which already imports `curator` + `outcome`) computes `curator.DupFingerprint(found)` once and stamps it on the open event from the same `found` it curates. The resolution checker matches the PR-body marker against the resolved episodes' `DupFingerprint`; with no marker it falls back to the normalized title join.

**Tech Stack:** Go 1.26, standard library only (`strings`, `encoding/json`). Reuses existing `curator.DupFingerprint` and `providers.ParseFingerprintMarker`.

## Global Constraints

- Go 1.26, standard library only, no new third-party deps.
- `internal/outcome` must remain a leaf (imports no `internal/*`): the dup fingerprint is a plain string field carried *through* it; it is computed by the producer, never imported into `outcome`.
- When the PR carries a marker, the join is fingerprint-only — do NOT fall through to a title match on a fingerprint mismatch (that resurrects the fragility R6 removes).
- An empty `DupFingerprint` must never match a non-empty one (mirrors the curator's empty-fingerprint guard).
- Full gate after the implementing tasks: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` (0 issues); `go test -race ./internal/outcome/ ./internal/curate/`.

---

### Task 1: Carry `DupFingerprint` through the outcome ledger

**Files:**
- Modify: `internal/outcome/ledger.go`
- Test: `internal/outcome/ledger_test.go`

**Interfaces:**
- Produces: `Event.DupFingerprint string` (JSON `dup_fingerprint,omitempty`), `Episode.DupFingerprint string`, copied through `Episodes()` and `Resolve()`.

- [ ] **Step 1: Write the failing test**

Add to `internal/outcome/ledger_test.go` (match its `package outcome` + style):

```go
func TestEpisodeCarriesDupFingerprint(t *testing.T) {
	p := filepath.Join(t.TempDir(), "outcomes.jsonl")
	l, err := New(p)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	t0 := time.Unix(2000, 0)
	if err := l.Open(Event{Fingerprint: "fp1", DupFingerprint: "dup-abc", Title: "T", At: t0}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	// live Resolve carries it
	ep, ok, err := l.Resolve("fp1", t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if ep.DupFingerprint != "dup-abc" {
		t.Fatalf("Resolve episode dup = %q, want dup-abc", ep.DupFingerprint)
	}
	// replayed Episodes() carries it too
	eps, err := l.Episodes()
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 1 || eps[0].DupFingerprint != "dup-abc" {
		t.Fatalf("Episodes dup = %+v, want one with dup-abc", eps)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/outcome/ -run TestEpisodeCarriesDupFingerprint -v`
Expected: FAIL — `DupFingerprint` undefined on `Event`/`Episode`.

- [ ] **Step 3: Implement**

In `internal/outcome/ledger.go`:
- Add to `Event` (after `Fingerprint`): `DupFingerprint string \`json:"dup_fingerprint,omitempty"\` // curator dedup fingerprint (resolution join key)`.
- Add to `Episode`: a `DupFingerprint string` field.
- In `Episodes()` open case (the `out = append(out, Episode{...})` literal) copy `DupFingerprint: e.DupFingerprint`.
- In `Resolve()` returned `Episode{...}` literal copy `DupFingerprint: o.DupFingerprint`.

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/outcome/ -v`
Expected: PASS (new test + all existing round-trip/recurrence tests).

- [ ] **Step 5: Build + gofmt**

Run: `go build ./... && gofmt -l internal/outcome/`
Expected: build PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/outcome/ledger.go internal/outcome/ledger_test.go
git commit -m "feat(outcome): carry dedup fingerprint on ledger event + episode"
```

---

### Task 2: Stamp the dup fingerprint on the recorded open

**Files:**
- Modify: `cmd/lore/main.go`

**Interfaces:**
- Consumes: `curator.DupFingerprint` (existing), `Event.DupFingerprint` (Task 1).

- [ ] **Step 1: Implement (no unit test — wiring in `OnComplete`; covered end-to-end by the resolution tests)**

In `cmd/lore/main.go` `OnComplete`, inside the `if len(fps) > 0 {` block, compute the dup fingerprint once before the per-fingerprint loop:

```go
dupFP := curator.DupFingerprint(found)
```

and add `DupFingerprint: dupFP,` to the `ledger.Open(outcome.Event{...})` literal (alongside `Fingerprint: fp`).

- [ ] **Step 2: Build + vet**

Run: `go build ./... && go vet ./cmd/lore/`
Expected: PASS.

- [ ] **Step 3: Commit**

```bash
git add cmd/lore/main.go
git commit -m "feat(outcome): stamp curator dedup fingerprint on the recorded open"
```

---

### Task 3: Re-key the resolution join (fingerprint-primary, title fallback)

**Files:**
- Modify: `internal/curate/resolution.go`
- Test: `internal/curate/resolution_test.go`

**Interfaces:**
- Consumes: `providers.ParseFingerprintMarker` (existing), `Episode.DupFingerprint` (Task 1).

- [ ] **Step 1: Write the failing tests**

Add to `internal/curate/resolution_test.go` (reuse `fakeLedger`, `discardLog`, `providers`):

```go
func TestLedgerResolutionRekeyedTitleDiffers(t *testing.T) {
	// resolved episode shares the FINGERPRINT but NOT the title (reworded re-investigation)
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{
		{Title: "totally different prose this run", DupFingerprint: "dup-xyz", Resolved: true},
	}}}
	pr := providers.CuratedIssue{
		Title: "KB: HarborRegistryDown",
		Body:  "Drafted by RunLore\n\n" + providers.FingerprintMarker("dup-xyz"),
	}
	if got, err := c.IsResolved(context.Background(), pr); err != nil || !got {
		t.Fatalf("a reworded re-investigation sharing the fingerprint must resolve; got %v err=%v", got, err)
	}
	// a PR whose fingerprint matches no resolved episode does NOT resolve
	other := providers.CuratedIssue{
		Title: "KB: HarborRegistryDown",
		Body:  "Drafted by RunLore\n\n" + providers.FingerprintMarker("dup-nomatch"),
	}
	if got, _ := c.IsResolved(context.Background(), other); got {
		t.Fatal("a non-matching fingerprint must NOT resolve")
	}
}

func TestLedgerResolutionLegacyTitleFallback(t *testing.T) {
	// PR with NO marker still resolves on the (whitespace-normalized) title join
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{
		{Title: "  HarborRegistryDown  ", Resolved: true}, // raw ledger title has stray spaces
	}}}
	pr := providers.CuratedIssue{Title: "KB: HarborRegistryDown"} // no body marker
	if got, err := c.IsResolved(context.Background(), pr); err != nil || !got {
		t.Fatalf("a markerless PR must resolve via the whitespace-robust title join; got %v err=%v", got, err)
	}
}

func TestLedgerResolutionFingerprintMismatchNoTitleFallthrough(t *testing.T) {
	// PR HAS a marker that matches no episode fingerprint, but shares a title — must stay unresolved
	c := LedgerResolutionChecker{Ledger: fakeLedger{eps: []outcome.Episode{
		{Title: "HarborRegistryDown", DupFingerprint: "dup-other", Resolved: true},
	}}}
	pr := providers.CuratedIssue{
		Title: "KB: HarborRegistryDown",
		Body:  "Drafted by RunLore\n\n" + providers.FingerprintMarker("dup-want"),
	}
	if got, _ := c.IsResolved(context.Background(), pr); got {
		t.Fatal("a fingerprint mismatch must not fall through to the brittle title match")
	}
}
```

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/curate/ -run 'TestLedgerResolutionRekeyed|TestLedgerResolutionLegacy|TestLedgerResolutionFingerprintMismatch' -v`
Expected: FAIL — current join ignores the marker / falls through on mismatch / compares raw title.

- [ ] **Step 3: Implement**

Rewrite `IsResolved` (`internal/curate/resolution.go:77-92`) to the fingerprint-primary form (see design): parse `ParseFingerprintMarker(pr.Body)`; normalize the title on both sides; when a marker is present match fingerprint-only, else fall back to the trimmed-title join; empty-on-both-sides → false. Update the type doc comment to describe the new join.

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/curate/ -v`
Expected: PASS (new tests + all existing resolution/recurrence/lifecycle tests — the existing title-only tests carry no marker and ride the legacy path).

- [ ] **Step 5: Full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all PASS; gofmt prints nothing; golangci-lint `0 issues`.
Then: `go test -race ./internal/outcome/ ./internal/curate/`.

- [ ] **Step 6: Commit**

```bash
git add internal/curate/resolution.go internal/curate/resolution_test.go
git commit -m "fix(curate): resolve curated PRs by dedup fingerprint, not free-text title"
```

---

## Self-Review

**Spec coverage:**
- `DupFingerprint` carried through `Event`/`Episode` (`outcome` stays a leaf) → Task 1. ✅
- Producer stamps `curator.DupFingerprint(found)` on the open → Task 2. ✅
- Fingerprint-primary join with whitespace-robust title fallback, no fall-through on mismatch → Task 3. ✅
- Acceptance: reworded re-investigation resolves the matching PR; a non-matching one does not → `TestLedgerResolutionRekeyedTitleDiffers`. ✅
- `TrimSpace`-vs-raw mismatch fixed → `TestLedgerResolutionLegacyTitleFallback`. ✅

**Type consistency:** `Event.DupFingerprint`/`Episode.DupFingerprint` (Task 1) set in Task 2, read in Task 3. `curator.DupFingerprint` + `providers.ParseFingerprintMarker` already exist. `cmd/lore/main.go` already imports `curator` + `outcome`. No new import edges into `outcome`. ✅
