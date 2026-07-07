# "Related knowledge" in KB PRs Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Every drafted KB PR carries a `## Related knowledge` section in its **body** (never the entry file): the nearest catalog entries with score + resource + web link, plus a trigger-recurrence line — so the reviewer can judge "duplicate? already documented?" without leaving the PR. Spec: `docs/superpowers/specs/2026-07-07-kb-human-surfaces-design.md`, Feature 3.

**Architecture:** The curator already runs a BM25 dedup search at draft time and discards the hits after the duplicate check; `Novelty` gains a `Hits(ctx, inv, k)` method so one k=5 search serves both the dup decision (top hit) and the reviewer context (all hits above a noise floor). `providers.KBEntry` gains `Related []RelatedEntry` + recurrence-fact fields (stamped by `draftKBEntry` from the Investigation — recurrence facts are read *before* curation in `onInvestigationComplete`, so they're already present). The GitHub forge's `prBody` becomes a method and renders the section with blob URLs derived from the client's API base.

**Tech Stack:** Go 1.26, stdlib only. Tests: `go test -race ./...`, table-driven with the existing `fakeForge`/`fakeScored` fakes. Lint: `golangci-lint run ./...`, `gofmt`.

## Global Constraints

- Go 1.26 (go.mod); CI runs `go build ./...`, `go vet ./...`, `test -z "$(gofmt -l .)"`, `go test -race ./...`.
- Never add AI attribution to commits or PRs. Conventional-commit prefixes (`feat:`, `fix:`, `test:`, `docs:`).
- The section goes in the **PR body only** — `renderEntry` (the committed OKF file) must not change, so `lore validate-kb` and the catalog loader are unaffected.
- The hidden `FingerprintMarker` must remain parseable from the PR body (`ParseFingerprintMarker`) — keep it the LAST element of the body.
- A related-search failure at draft time omits the section; the PR still opens (best-effort).
- Keep the comment density/style of surrounding code (this repo comments the *why* heavily).
- Work on a dedicated branch (e.g. `feat/kb-related-knowledge`), one PR for this whole plan.

---

### Task 1: `providers.RelatedEntry` + KBEntry fields, `Novelty.Hits`, curator population

**Files:**
- Modify: `internal/providers/providers.go` (KBEntry ~line 471; RelatedEntry type below it)
- Modify: `internal/curator/fingerprint.go` (`Novelty` ~line 109: add `Hits`, reimplement `TopHit` on top)
- Modify: `internal/curator/curator.go` (`Curate` dedup block ~lines 76–87 and the OpenPR call ~line 102)
- Modify: `internal/curator/draft.go` (`draftKBEntry` return literal ~line 80)
- Test: `internal/curator/curator_test.go`, `internal/curator/draft_test.go`

**Interfaces:**
- Consumes: `catalog.ScoredSearcher.SearchScored(query, k)`; existing `Fingerprint(inv)` query builder; `Investigation.Occurrences` / `PrevCuratedURL` (stamped before curation by `onInvestigationComplete`).
- Produces: `providers.RelatedEntry{Path, Title, Resource string; Score float64}`; `KBEntry.Related []RelatedEntry`, `KBEntry.Occurrences int`, `KBEntry.PrevCuratedURL string`; `Novelty.Hits(ctx, inv, k) ([]catalog.ScoredEntry, error)`. Task 2 renders these in the PR body.

- [ ] **Step 1: Write the failing tests.** In `internal/curator/curator_test.go` (reuse `fakeForge`, `newCurator`, `goodFinding`; the file already has a `fakeScored` — add a variant that returns several hits if it can't):

```go
// multiScored returns a fixed multi-hit result for any query — the shape the
// related-knowledge section consumes.
type multiScored struct{ hits []catalog.ScoredEntry }

func (m multiScored) SearchScored(string, int) ([]catalog.ScoredEntry, error) { return m.hits, nil }

func TestCurateAttachesRelatedKnowledge(t *testing.T) {
	f := &fakeForge{}
	cat := multiScored{hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Path: "incidents/a.md", Title: "A", Resource: "apps/web"}, Score: 2.5},
		{Entry: catalog.Entry{Path: "incidents/b.md", Title: "B"}, Score: 0.9},
		{Entry: catalog.Entry{Path: "incidents/noise.md", Title: "noise"}, Score: 0.05}, // below the floor
	}}
	inv := goodFinding()
	inv.Occurrences = 3
	inv.PrevCuratedURL = "https://kb/pr/12"
	if _, err := newCurator(f, cat).Curate(context.Background(), inv); err != nil {
		t.Fatalf("curate: %v", err)
	}
	if f.openedPR == nil {
		t.Fatal("no PR opened")
	}
	e := *f.openedPR
	if len(e.Related) != 2 {
		t.Fatalf("Related = %+v, want the 2 hits above the noise floor", e.Related)
	}
	if e.Related[0].Path != "incidents/a.md" || e.Related[0].Score != 2.5 || e.Related[0].Resource != "apps/web" {
		t.Errorf("Related[0] = %+v", e.Related[0])
	}
	if e.Occurrences != 3 || e.PrevCuratedURL != "https://kb/pr/12" {
		t.Errorf("recurrence facts not stamped: occ=%d prev=%q", e.Occurrences, e.PrevCuratedURL)
	}
}

// The dup decision is unchanged: a top hit at/above DupScore still skips the PR
// (no Related work happens for a duplicate).
func TestCurateDupStillSkipsWithMultiHits(t *testing.T) {
	f := &fakeForge{}
	cat := multiScored{hits: []catalog.ScoredEntry{
		{Entry: catalog.Entry{Path: "incidents/dup.md", Title: "dup"}, Score: 9.0}, // ≥ DupScore 5.0
	}}
	ref, err := newCurator(f, cat).Curate(context.Background(), goodFinding())
	if err != nil {
		t.Fatalf("curate: %v", err)
	}
	if ref.URL != "" || f.openedPR != nil {
		t.Fatal("duplicate finding must not open a PR")
	}
}
```

In `internal/curator/draft_test.go`:

```go
func TestDraftKBEntryCarriesRecurrenceFacts(t *testing.T) {
	inv := goodFinding()
	inv.Occurrences = 2
	inv.PrevCuratedURL = "https://kb/pr/7"
	e := draftKBEntry(inv)
	if e.Occurrences != 2 || e.PrevCuratedURL != "https://kb/pr/7" {
		t.Errorf("KBEntry recurrence facts = occ %d prev %q", e.Occurrences, e.PrevCuratedURL)
	}
	// The committed FILE must not gain reviewer-context sections.
	if strings.Contains(e.Body, "Related knowledge") {
		t.Error("entry body must never contain the PR-only Related knowledge section")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/curator/ -run 'TestCurateAttaches|TestCurateDupStill|TestDraftKBEntryCarries' -v`
Expected: compile FAIL — `Related`, `Occurrences` undefined on KBEntry.

- [ ] **Step 3: Implement.**

In `internal/providers/providers.go`, extend `KBEntry` (after `Provenance`):

```go
	// Reviewer context, rendered in the PR BODY only — never in the committed
	// entry file (renderEntry ignores these), so the catalog and validator are
	// untouched. Related is the draft-time BM25 neighborhood; the recurrence
	// facts mirror Investigation.Occurrences/PrevCuratedURL.
	Related        []RelatedEntry
	Occurrences    int
	PrevCuratedURL string
```

and add below the struct:

```go
// RelatedEntry is a nearby catalog entry surfaced to the KB PR reviewer so
// "is this a duplicate / what do we already know?" is answerable in the PR.
type RelatedEntry struct {
	Path     string  // bundle-relative entry path (the forge renders the web link)
	Title    string
	Resource string  // affected resource, when the entry names one
	Score    float64 // BM25 score at draft time (corpus-relative — a hint, not a ranking guarantee)
}
```

In `internal/curator/fingerprint.go`, add `Hits` and express `TopHit` through it:

```go
// Hits returns up to k catalog entries scored against the finding's
// fingerprint — hits[0] drives the duplicate decision, the full slice feeds
// the PR's related-knowledge section (one search, two consumers).
func (n Novelty) Hits(ctx context.Context, inv providers.Investigation, k int) ([]catalog.ScoredEntry, error) { //nolint:revive // ctx kept for future remote-index symmetry
	if n.Catalog == nil {
		return nil, nil
	}
	return n.Catalog.SearchScored(Fingerprint(inv), k)
}
```

and reimplement `TopHit` (keeping its signature and doc comment) as:

```go
	hits, err := n.Hits(ctx, inv, 1)
	if err != nil {
		return catalog.ScoredEntry{}, false, err
	}
	if len(hits) == 0 {
		return catalog.ScoredEntry{}, false, nil
	}
	return hits[0], true, nil
```

In `internal/curator/curator.go`, add the constants near the top of the file:

```go
// Related-knowledge bounds: how many BM25 neighbors a drafted PR shows the
// reviewer, and the noise floor below which a "neighbor" is lexical debris.
// The floor is deliberately low — BM25 absolute scores are corpus-dependent
// (see the recall margin gate), so the top-k cap does the real limiting and
// the floor only drops near-zero matches on genuinely novel incidents.
const (
	relatedK     = 5
	relatedFloor = 0.2
)
```

replace the `nov := ...; if top, ok, err := nov.TopHit(...)` block (~lines 76–87) with:

```go
	nov := Novelty{Catalog: c.Catalog, DupScore: c.DupScore}
	hits, herr := nov.Hits(ctx, inv, relatedK)
	if herr != nil {
		c.Log.Warn("dedup: catalog search failed", "err", herr)
	} else if len(hits) > 0 {
		if c.Metrics != nil {
			c.Metrics.CurationDedupScore.Record(ctx, hits[0].Score)
		}
		if hits[0].Score >= c.DupScore {
			c.Log.Info("finding duplicates a catalog entry; not filing", "entry", hits[0].Entry.Title, "score", hits[0].Score)
			return providers.Ref{}, nil
		}
	}
```

and replace the OpenPR call (~line 102):

```go
	entry := draftKBEntry(inv)
	// Reviewer context: the finding is novel, so its BM25 neighborhood is
	// precisely what the reviewer needs to double-check that call.
	entry.Related = relatedEntries(hits)
	ref, err := c.Forge.OpenPR(ctx, entry)
```

and add the helper:

```go
// relatedEntries maps the draft-time search hits to the PR's reviewer-context
// list, dropping noise-floor matches. Order (best first) is preserved.
func relatedEntries(hits []catalog.ScoredEntry) []providers.RelatedEntry {
	var out []providers.RelatedEntry
	for _, h := range hits {
		if h.Score < relatedFloor {
			continue
		}
		out = append(out, providers.RelatedEntry{
			Path: h.Entry.Path, Title: h.Entry.Title, Resource: h.Entry.Resource, Score: h.Score,
		})
	}
	return out
}
```

In `internal/curator/draft.go`, add to the `providers.KBEntry{...}` literal in `draftKBEntry`:

```go
		// Recurrence facts for the PR body's related-knowledge section — stamped
		// on the Investigation BEFORE curation runs (see onInvestigationComplete).
		Occurrences:    inv.Occurrences,
		PrevCuratedURL: inv.PrevCuratedURL,
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/curator/ -race -v`
Expected: PASS — new tests plus every pre-existing curator test (the TopHit rewrite must not change dedup behavior).

- [ ] **Step 5: Commit**

```bash
git add internal/providers/providers.go internal/curator/
git commit -m "feat(curator): attach the BM25 neighborhood + recurrence facts to drafted KB entries"
```

---

### Task 2: render `## Related knowledge` in the GitHub PR body

**Files:**
- Modify: `internal/forge/github/github.go` (`prBody` ~line 367 → method; `OpenPR` call site ~line 146; new `relatedSection` + `blobURL` helpers)
- Test: `internal/forge/github/github_test.go`

**Interfaces:**
- Consumes: `KBEntry.Related` / `.Occurrences` / `.PrevCuratedURL` (Task 1); `Client.baseURL/owner/repo/baseBranch`.
- Produces: PR bodies containing `## Related knowledge` with `[Title](https://<host>/<owner>/<repo>/blob/<branch>/<path>)` items and a `Trigger seen ×N` line; the fingerprint marker stays last and parseable.

- [ ] **Step 1: Write the failing test** — add to `github_test.go` (the package already tests unexported helpers directly):

```go
func TestPRBodyRelatedKnowledge(t *testing.T) {
	c := New("", "acme", "kb", "main", nil) // public GitHub host
	e := providers.KBEntry{
		Title: "T", Description: "d", Fingerprint: "abc123",
		Related: []providers.RelatedEntry{
			{Path: "incidents/a.md", Title: "A", Resource: "apps/web", Score: 2.5},
			{Path: "incidents/b.md", Title: "B", Score: 0.9},
		},
		Occurrences:    3,
		PrevCuratedURL: "https://kb/pr/12",
	}
	body := c.prBody(e)
	for _, want := range []string{
		"## Related knowledge",
		"[A](https://github.com/acme/kb/blob/main/incidents/a.md)",
		"score 2.50",
		"resource apps/web",
		"[B](https://github.com/acme/kb/blob/main/incidents/b.md)",
		"Trigger seen ×3",
		"previous entry: https://kb/pr/12",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("prBody missing %q\n---\n%s", want, body)
		}
	}
	// The dedup marker survives, still parseable, still last.
	if got := providers.ParseFingerprintMarker(body); got != "abc123" {
		t.Errorf("ParseFingerprintMarker = %q, want abc123", got)
	}
	if !strings.HasSuffix(strings.TrimSpace(body), providers.FingerprintMarker("abc123")) {
		t.Error("fingerprint marker must remain the last body element")
	}
}

func TestPRBodyNoRelatedSectionWhenEmpty(t *testing.T) {
	c := New("", "acme", "kb", "main", nil)
	body := c.prBody(providers.KBEntry{Title: "T", Fingerprint: "abc123"})
	if strings.Contains(body, "Related knowledge") {
		t.Errorf("no-hit, first-sighting PR must not carry an empty section:\n%s", body)
	}
}

func TestBlobURLEnterpriseHost(t *testing.T) {
	c := New("https://ghe.example.com/api/v3", "acme", "kb", "main", nil)
	want := "https://ghe.example.com/acme/kb/blob/main/incidents/a.md"
	if got := c.blobURL("incidents/a.md"); got != want {
		t.Errorf("blobURL = %q, want %q", got, want)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/forge/github/ -run 'TestPRBody|TestBlobURL' -v`
Expected: compile FAIL — `c.prBody` / `c.blobURL` undefined (prBody is currently a package function).

- [ ] **Step 3: Implement.** Turn `prBody` into a method and append the section **before** the marker (the marker stays last):

```go
func (c *Client) prBody(e providers.KBEntry) string {
	desc := e.Description
	if desc == "" {
		desc = e.Title
	}
	body := fmt.Sprintf("Drafted by RunLore — %s\n\nReview the decision card + OKF entry in the changed file.", desc)
	if s := c.relatedSection(e); s != "" {
		body += "\n\n" + s
	}
	if m := providers.FingerprintMarker(e.Fingerprint); m != "" {
		body += "\n\n" + m
	}
	return body
}

// relatedSection renders the reviewer context: the draft-time BM25 neighborhood
// (linked, scored) and the trigger's recurrence line. Empty when there is
// nothing to say — a genuinely novel first sighting gets no noise section.
func (c *Client) relatedSection(e providers.KBEntry) string {
	if len(e.Related) == 0 && e.Occurrences <= 1 {
		return ""
	}
	var b strings.Builder
	b.WriteString("## Related knowledge\n")
	for _, r := range e.Related {
		fmt.Fprintf(&b, "\n- [%s](%s) — score %.2f", r.Title, c.blobURL(r.Path), r.Score)
		if r.Resource != "" {
			fmt.Fprintf(&b, " · resource %s", r.Resource)
		}
	}
	if e.Occurrences > 1 {
		fmt.Fprintf(&b, "\n\nTrigger seen ×%d", e.Occurrences)
		if e.PrevCuratedURL != "" {
			fmt.Fprintf(&b, " · previous entry: %s", e.PrevCuratedURL)
		}
	}
	return b.String()
}

// blobURL is the web URL of a catalog file on the base branch. The web host is
// the API base with its API suffix stripped: api.github.com → github.com;
// GHES https://ghe.example.com/api/v3 → https://ghe.example.com. Relative
// links are NOT an option here — GitHub does not resolve them in PR bodies.
func (c *Client) blobURL(path string) string {
	host := c.baseURL
	if host == DefaultBaseURL {
		host = "https://github.com"
	} else {
		host = strings.TrimSuffix(host, "/api/v3")
	}
	branch := c.baseBranch
	if branch == "" {
		branch = "main"
	}
	return fmt.Sprintf("%s/%s/%s/blob/%s/%s", host, c.owner, c.repo, branch, path)
}
```

Update the one call site in `OpenPR` (~line 146): `"body": prBody(e)` → `"body": c.prBody(e)`.

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/forge/github/ -race -v`
Expected: PASS — new tests plus every pre-existing forge test.

- [ ] **Step 5: Commit**

```bash
git add internal/forge/github/
git commit -m "feat(forge): render a Related knowledge section in drafted KB PR bodies"
```

---

### Task 3: docs + full verification

**Files:**
- Modify: `docs/reviewing-knowledge.md` (reviewer-facing description)
- Modify: `docs/learning-loop.md` (§5 Curate, one sentence)

- [ ] **Step 1: Docs.** In `docs/reviewing-knowledge.md`, where the PR review flow is described, add:

```markdown
### Related knowledge in the PR

Each drafted PR ends with a **Related knowledge** section: the closest existing
entries at draft time (linked, with their BM25 score and affected resource) and
— when the trigger has fired before — a `Trigger seen ×N` line pointing at the
previous entry. Use it to answer the two review questions cheaply: *is this a
duplicate of something merged?* and *does this incident keep coming back?*
Scores are corpus-relative hints, not a ranking guarantee.
```

In `docs/learning-loop.md` §5 (Curate), after the DRAFT box description, add one sentence:

> The drafted PR body also carries a *Related knowledge* section — the dedup search's k=5 neighborhood plus the trigger's recurrence line — so the human reviewing the entry sees what the catalog already holds.

- [ ] **Step 2: Full verification**

Run: `go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test -race ./... && golangci-lint run ./...`
Expected: all green.

- [ ] **Step 3: Commit**

```bash
git add docs/reviewing-knowledge.md docs/learning-loop.md
git commit -m "docs(kb): document the Related knowledge PR section"
```
