# Deterministic curation dedup fingerprint Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Replace the curator's free-text-title open-PR dedup with a deterministic fingerprint (affected-resource ref + root-cause token-set), stored in the entry frontmatter and a hidden PR-body marker, so two investigations of one incident coalesce instead of both filing.

**Architecture:** A pure `DupFingerprint(inv)` hashes the structured signals; the value is written into the KB entry frontmatter (durable) and a hidden HTML-comment marker in the PR body (matchable from the PR listing). `duplicateOpenPR` matches on that marker.

**Tech Stack:** Go 1.26, `crypto/sha256`, `encoding/hex`, `unicode`, `sort`, `strings` — all standard library.

## Global Constraints

- Go 1.26, standard library only, no new third-party deps.
- The catalog BM25 dedup (`Novelty`, the existing `Fingerprint()` query) is NOT changed — only the open-PR dedup.
- Marker helpers live in package `providers` (both `curator` and `forge/github` import it; do not create an import cycle by putting them in `curator`).
- After each task: `go build ./... && go vet ./... && go test ./...` green and `gofmt -l .` empty (CI's golangci-lint fails on any unformatted file).
- Open PRs filed before this change carry no marker and must not match (no retro-dedup); an empty fingerprint must never match anything.

---

### Task 1: `DupFingerprint` deterministic hash

**Files:**
- Modify: `internal/curator/fingerprint.go`
- Test: `internal/curator/fingerprint_test.go`

**Interfaces:**
- Produces: `func DupFingerprint(inv providers.Investigation) string` — sha256 hex of `lower(Resource.Ref()) + "|" + sortedTokenSet(topCauseSummary)`; `""` when both are empty.

- [ ] **Step 1: Write the failing tests**

Add to `internal/curator/fingerprint_test.go` (match the file's existing package + import style):

```go
func TestDupFingerprintStableAcrossTitlePhrasing(t *testing.T) {
	a := providers.Investigation{
		Title:      "Pod apps/web crash looping",
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "image tag rollout broke the readiness probe"}},
	}
	b := providers.Investigation{
		Title:      "apps/web is down after a deploy", // different prose
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "the readiness probe broke when the image tag rollout happened"}},
	}
	fa, fb := DupFingerprint(a), DupFingerprint(b)
	if fa == "" || fa != fb {
		t.Fatalf("same resource+cause must hash alike across phrasing: %q vs %q", fa, fb)
	}
}

func TestDupFingerprintDiffersByResource(t *testing.T) {
	base := providers.Investigation{
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "connection pool exhausted"}},
	}
	other := base
	other.Resource = providers.Workload{Namespace: "apps", Name: "worker"}
	if DupFingerprint(base) == DupFingerprint(other) {
		t.Fatal("different affected resource must change the fingerprint")
	}
}

func TestDupFingerprintDiffersByCause(t *testing.T) {
	base := providers.Investigation{
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "connection pool exhausted"}},
	}
	other := base
	other.RootCauses = []providers.Hypothesis{{Summary: "expired TLS certificate blocked startup"}}
	if DupFingerprint(base) == DupFingerprint(other) {
		t.Fatal("disjoint cause token-sets must change the fingerprint")
	}
}

func TestDupFingerprintEmptyWhenNoResourceOrCause(t *testing.T) {
	if got := DupFingerprint(providers.Investigation{Title: "something"}); got != "" {
		t.Fatalf("no resource and no cause must yield empty fingerprint, got %q", got)
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `go test ./internal/curator/ -run TestDupFingerprint -v`
Expected: FAIL — `DupFingerprint` undefined.

- [ ] **Step 3: Implement `DupFingerprint` + `tokenSet`**

In `internal/curator/fingerprint.go`, add imports `crypto/sha256`, `encoding/hex`, `sort`, `unicode` (keep existing `strings`, `providers`), and append:

```go
// DupFingerprint is a deterministic identity for "the same problem on the same
// resource": the affected-resource ref plus the sorted significant-token set of the
// top root cause, hashed. Unlike Fingerprint (a fuzzy BM25 query), it is stable
// across the LLM's prose phrasing, so two investigations of one incident hash
// alike. It returns "" when there is neither a resource nor a cause to key on — an
// empty fingerprint must never match another.
func DupFingerprint(inv providers.Investigation) string {
	ref := strings.ToLower(inv.Resource.Ref())
	cause := ""
	if len(inv.RootCauses) > 0 {
		cause = inv.RootCauses[0].Summary
	}
	tokens := tokenSet(cause)
	if ref == "" && len(tokens) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(ref + "|" + strings.Join(tokens, " ")))
	return hex.EncodeToString(sum[:])
}

// tokenSet lowercases s, splits on non-alphanumeric runes, drops tokens shorter
// than 3 chars, dedupes, and sorts — an order-independent significant-token set so
// reworded phrasings of one cause normalize to the same key.
func tokenSet(s string) []string {
	fields := strings.FieldsFunc(strings.ToLower(s), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	seen := make(map[string]struct{}, len(fields))
	var out []string
	for _, f := range fields {
		if len(f) < 3 {
			continue
		}
		if _, ok := seen[f]; ok {
			continue
		}
		seen[f] = struct{}{}
		out = append(out, f)
	}
	sort.Strings(out)
	return out
}
```

- [ ] **Step 4: Run the tests to verify they pass**

Run: `go test ./internal/curator/ -run TestDupFingerprint -v`
Expected: PASS.

- [ ] **Step 5: Full package + gofmt**

Run: `go test ./internal/curator/ && gofmt -l internal/curator/`
Expected: PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/curator/fingerprint.go internal/curator/fingerprint_test.go
git commit -m "feat(curator): DupFingerprint — deterministic resource+cause dedup hash"
```

---

### Task 2: `KBEntry.Fingerprint` + PR-body marker helpers (providers)

**Files:**
- Modify: `internal/providers/providers.go`
- Test: `internal/providers/providers_test.go` (create if absent)

**Interfaces:**
- Produces: `KBEntry.Fingerprint string`; `func FingerprintMarker(fp string) string`; `func ParseFingerprintMarker(body string) string`.

- [ ] **Step 1: Write the failing test**

Determine `internal/providers` test package style (likely `package providers`). Create/append `internal/providers/providers_test.go`:

```go
func TestFingerprintMarkerRoundTrip(t *testing.T) {
	const fp = "abc123def456"
	body := "Drafted by RunLore — x\n\n" + FingerprintMarker(fp)
	if got := ParseFingerprintMarker(body); got != fp {
		t.Fatalf("round-trip: want %q, got %q", fp, got)
	}
	if FingerprintMarker("") != "" {
		t.Fatal("empty fingerprint must render an empty marker")
	}
	if got := ParseFingerprintMarker("no marker here"); got != "" {
		t.Fatalf("absent marker must parse to empty, got %q", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/providers/ -run TestFingerprintMarker -v`
Expected: FAIL — `FingerprintMarker`/`ParseFingerprintMarker` undefined.

- [ ] **Step 3: Implement**

In `internal/providers/providers.go`: add the field to `KBEntry` (after `Resource` or `Tags`):

```go
	Fingerprint string // deterministic dedup fingerprint (see curator.DupFingerprint)
```

Add (ensure `strings` is imported — it likely already is; if not, add it):

```go
const fingerprintMarkerPrefix = "<!-- runlore-fingerprint: "

// FingerprintMarker renders a hidden PR-body marker carrying the dedup fingerprint,
// so an open PR's fingerprint is recoverable from the PR listing without fetching
// file contents. It returns "" for an empty fingerprint so callers may append it
// unconditionally.
func FingerprintMarker(fp string) string {
	if fp == "" {
		return ""
	}
	return fingerprintMarkerPrefix + fp + " -->"
}

// ParseFingerprintMarker extracts the fingerprint from a PR body, or "" if absent.
func ParseFingerprintMarker(body string) string {
	i := strings.Index(body, fingerprintMarkerPrefix)
	if i < 0 {
		return ""
	}
	rest := body[i+len(fingerprintMarkerPrefix):]
	j := strings.Index(rest, " -->")
	if j < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:j])
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/providers/ -run TestFingerprintMarker -v`
Expected: PASS.

- [ ] **Step 5: Build + gofmt**

Run: `go build ./... && gofmt -l internal/providers/`
Expected: build PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/providers/providers.go internal/providers/providers_test.go
git commit -m "feat(providers): KBEntry.Fingerprint + PR-body fingerprint marker helpers"
```

---

### Task 3: Carry the fingerprint into the drafted entry, frontmatter, and PR body

**Files:**
- Modify: `internal/curator/draft.go`
- Modify: `internal/forge/github/github.go`
- Test: `internal/curator/draft_test.go`, `internal/forge/github/github_test.go`

**Interfaces:**
- Consumes: `DupFingerprint` (Task 1), `KBEntry.Fingerprint` + `FingerprintMarker` (Task 2).

- [ ] **Step 1: Write the failing tests**

Add to `internal/curator/draft_test.go`:

```go
func TestDraftKBEntrySetsFingerprint(t *testing.T) {
	inv := providers.Investigation{
		Title:      "apps/web crash",
		Confidence: 0.9,
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "image tag rollout broke readiness", Evidence: []string{"e"}}},
	}
	if got := draftKBEntry(inv).Fingerprint; got != DupFingerprint(inv) {
		t.Fatalf("drafted entry fingerprint = %q, want %q", got, DupFingerprint(inv))
	}
}
```

Add to `internal/forge/github/github_test.go`:

```go
func TestRenderEntryIncludesFingerprintFrontmatter(t *testing.T) {
	out := renderEntry(providers.KBEntry{Type: "Incident", Title: "T", Fingerprint: "deadbeef"})
	if !strings.Contains(out, "fingerprint: deadbeef") {
		t.Fatalf("frontmatter missing fingerprint:\n%s", out)
	}
	out = renderEntry(providers.KBEntry{Type: "Incident", Title: "T"})
	if strings.Contains(out, "fingerprint:") {
		t.Fatalf("empty fingerprint must be omitted:\n%s", out)
	}
}

func TestPRBodyIncludesFingerprintMarker(t *testing.T) {
	body := prBody(providers.KBEntry{Title: "T", Description: "d", Fingerprint: "deadbeef"})
	if providers.ParseFingerprintMarker(body) != "deadbeef" {
		t.Fatalf("PR body missing recoverable fingerprint marker:\n%s", body)
	}
}
```

Confirm `github_test.go` imports `strings` and `providers` (add if missing).

- [ ] **Step 2: Run to verify they fail**

Run: `go test ./internal/curator/ -run TestDraftKBEntrySetsFingerprint -v && go test ./internal/forge/github/ -run 'TestRenderEntryIncludesFingerprint|TestPRBodyIncludesFingerprint' -v`
Expected: FAIL (fingerprint not set / not rendered / not in body).

- [ ] **Step 3: Implement**

In `internal/curator/draft.go`, set the field on the returned `KBEntry` (it currently sets Type/Title/Description/Resource/Tags/Body):

```go
		Fingerprint: DupFingerprint(inv),
```

In `internal/forge/github/github.go`:

Add to `kbFrontmatter`:
```go
	Fingerprint string `yaml:"fingerprint,omitempty"`
```
Pass it in `renderEntry`'s `kbFrontmatter{...}` literal:
```go
	fm, _ := yaml.Marshal(kbFrontmatter{Type: e.Type, Title: e.Title, Description: e.Description, Resource: e.Resource, Tags: e.Tags, Fingerprint: e.Fingerprint})
```
Append the marker in `prBody` (return value currently a single `fmt.Sprintf`):
```go
func prBody(e providers.KBEntry) string {
	desc := e.Description
	if desc == "" {
		desc = e.Title
	}
	body := fmt.Sprintf("Drafted by RunLore — %s\n\nReview the decision card + OKF entry in the changed file.", desc)
	if m := providers.FingerprintMarker(e.Fingerprint); m != "" {
		body += "\n\n" + m
	}
	return body
}
```

- [ ] **Step 4: Run to verify they pass**

Run: `go test ./internal/curator/ ./internal/forge/github/`
Expected: PASS.

- [ ] **Step 5: Build + gofmt**

Run: `go build ./... && gofmt -l internal/curator/ internal/forge/github/`
Expected: build PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/curator/draft.go internal/forge/github/github.go internal/curator/draft_test.go internal/forge/github/github_test.go
git commit -m "feat(curator): write dedup fingerprint into entry frontmatter + PR body"
```

---

### Task 4: Fingerprint-based open-PR dedup

**Files:**
- Modify: `internal/curator/curator.go`
- Test: `internal/curator/curator_test.go`

**Interfaces:**
- Consumes: `DupFingerprint` (Task 1), `providers.ParseFingerprintMarker` (Task 2), `CuratedIssue.Body` (existing).

- [ ] **Step 1: Update + add the failing tests**

In `internal/curator/curator_test.go`:

1. Update `TestCurateDuplicateCoalescesNoPR` — the existing fake open PR matches by title; change it to match by fingerprint marker in the **Body**. Set the fake PR's `Body` to include `providers.FingerprintMarker(DupFingerprint(inv))` for the same `inv` the test curates, and keep the assertion that it coalesces (comments, no new PR).

2. Add:

```go
func TestCurateDistinctTitleSameFingerprintCoalesces(t *testing.T) {
	inv := providers.Investigation{
		Title:      "freshly reworded title the LLM produced this time",
		Confidence: 0.9,
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "image tag rollout broke readiness", Evidence: []string{"e"}}},
	}
	openPR := providers.CuratedIssue{
		Number: 7,
		Title:  "KB: a completely different earlier title",
		Body:   "Drafted by RunLore\n\n" + providers.FingerprintMarker(DupFingerprint(inv)),
	}
	f := &fakeForge{openPRs: []providers.CuratedIssue{openPR}}
	c := &Curator{Forge: f, MinConfidence: 0.5, Log: testLogger()}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if f.openedPR != nil {
		t.Fatal("a same-fingerprint finding must coalesce, not open a second PR")
	}
	if len(f.commented) != 1 || f.commented[0] != 7 {
		t.Fatalf("expected a coalesce comment on PR 7, got %+v", f.commented)
	}
}

func TestCurateDifferentFingerprintOpensSecondPR(t *testing.T) {
	inv := providers.Investigation{
		Title:      "apps/web readiness failure",
		Confidence: 0.9,
		Resource:   providers.Workload{Namespace: "apps", Name: "web"},
		RootCauses: []providers.Hypothesis{{Summary: "image tag rollout broke readiness", Evidence: []string{"e"}}},
	}
	openPR := providers.CuratedIssue{
		Number: 7, Title: "KB: unrelated",
		Body: "Drafted by RunLore\n\n" + providers.FingerprintMarker("0000ffff_a_different_hash"),
	}
	f := &fakeForge{openPRs: []providers.CuratedIssue{openPR}}
	c := &Curator{Forge: f, MinConfidence: 0.5, Log: testLogger()}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if f.openedPR == nil {
		t.Fatal("a different-fingerprint finding must open its own PR")
	}
}
```

Use whatever logger helper the existing tests use (e.g. an inline `slog.New(slog.NewTextHandler(io.Discard, nil))` — match the file). If the existing tests construct `Curator` differently (e.g. with a `Catalog`), mirror that; a nil `Catalog` skips catalog dedup so these tests isolate the open-PR path.

- [ ] **Step 2: Run to verify the new tests fail**

Run: `go test ./internal/curator/ -run 'TestCurateDistinctTitleSameFingerprint|TestCurateDifferentFingerprint' -v`
Expected: FAIL — current title-based dedup neither coalesces the reworded-title case nor distinguishes by fingerprint.

- [ ] **Step 3: Rewrite `duplicateOpenPR` and drop `normTitle`**

Replace `duplicateOpenPR` (`curator.go:83-95`) with:

```go
// duplicateOpenPR reports an open KB PR whose stored dedup fingerprint matches this
// finding's — deterministic identity (resource + cause), not the LLM's free-text
// title. An empty fingerprint (no resource and no cause) never matches.
func (c *Curator) duplicateOpenPR(ctx context.Context, inv providers.Investigation) (int, bool, error) {
	want := DupFingerprint(inv)
	if want == "" {
		return 0, false, nil
	}
	prs, err := c.Forge.ListPRsByLabel(ctx, "runlore")
	if err != nil {
		return 0, false, err
	}
	for _, pr := range prs {
		if providers.ParseFingerprintMarker(pr.Body) == want {
			return pr.Number, true, nil
		}
	}
	return 0, false, nil
}
```

Delete the now-unused `normTitle` function (`curator.go:108`). If `strings` becomes unused in `curator.go` after this, remove it from the imports; if still used elsewhere, leave it (the build/`go vet` will tell you).

- [ ] **Step 4: Run the curator package**

Run: `go test ./internal/curator/ -v`
Expected: PASS (updated `TestCurateDuplicateCoalescesNoPR` + both new tests + all others).

- [ ] **Step 5: Full suite + vet + gofmt**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l .`
Expected: all PASS; gofmt prints nothing.

- [ ] **Step 6: Commit**

```bash
git add internal/curator/curator.go internal/curator/curator_test.go
git commit -m "feat(curator): dedup open PRs by deterministic fingerprint, not title"
```

---

## Self-Review

**Spec coverage:**
- `DupFingerprint` (resource + cause token-set, empty-guard) → Task 1. ✅
- `KBEntry.Fingerprint` + marker helpers in `providers` → Task 2. ✅
- Fingerprint into frontmatter (durable) + PR-body marker → Task 3. ✅
- Fingerprint-based `duplicateOpenPR`, drop `normTitle` → Task 4. ✅
- Catalog BM25 dedup untouched; no retro-dedup; empty never matches → Tasks 1, 4. ✅

**Placeholder scan:** all code steps show complete code; test-helper references (logger, fakeForge) are explicitly "match the existing file." ✅

**Type consistency:** `DupFingerprint` (Task 1) consumed in Tasks 3, 4; `FingerprintMarker`/`ParseFingerprintMarker` (Task 2) consumed in Tasks 3, 4; `KBEntry.Fingerprint` (Task 2) set in Task 3 and rendered in Task 3. The `providers` import is already present in `curator` and `forge/github`. ✅
