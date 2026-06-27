# Learning Curation — Phase 1: File-time Gate (Plan 1 of 2)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop the curator from filing duplicate and low-quality KB artifacts: before opening anything, dedup the finding (against the catalog + open KB PRs) and gate on quality; only novel, quality-passing findings become a merge-ready PR; the old `uncertain → OpenIssue` branch is deleted.

**Architecture:** Refactor `internal/curator` from "confidence ≥ 0.75 → PR else issue" into a three-step gate (dedup → quality → draft). Add a `ListPRsByLabel` forge method (the existing `ListIssuesByLabel` filters PRs out) and a `Comment`-based coalesce path (reusing the forge methods reinvestigate already added). Upgrade the OKF drafter to the full Symptom/Investigate/Cause/Resolution shape with a decision card. **The file-time gate enforces quality + novelty only** (PRs are labelled `solved`); the `resolved`/`accepted` *merge* condition is enforced later by Phase 2 + the human.

**Tech Stack:** Go 1.26 stdlib + `net/http/httptest`, `gopkg.in/yaml.v3`. Reuses `providers.{Investigation,KBEntry,Ref,CuratedIssue}`, `catalog.{ScoredSearcher,ScoredEntry,Entry}`, `internal/forge/github`.

**Verified integration points (verbatim):**
- `curator.Curator{Issues providers.IssueProvider; MinConfidencePR float64; Log *slog.Logger}`; `Curate` routes `conf ≥ MinConfidencePR → Issues.OpenPR(draftKBEntry(inv))` else `Issues.OpenIssue(inv)` (`internal/curator/curator.go:15-67`).
- `providers.IssueProvider{ OpenIssue(ctx, Investigation)(Ref,error); OpenPR(ctx, KBEntry)(Ref,error) }` (`providers.go:273-276`). `providers.ReinvestForge{ ListIssuesByLabel(ctx,label)([]CuratedIssue,error); Comment(ctx,number,body)error; ReplaceLabel(ctx,number,remove,add)error }` (`providers.go:290-298`).
- `github.Client` methods: `OpenIssue`, `OpenPR` (4-call + `addLabels`), `addLabels`(priv), `ListIssuesByLabel`→`[]providers.CuratedIssue` (**filters PRs out**), `Comment`, `ReplaceLabel`; `do(ctx,method,path,body,out)`; `New(baseURL,owner,repo,baseBranch,TokenFunc)`; `var lifecycleLabels = []string{"runlore","triggered"}` (`internal/forge/github/github.go`).
- `catalog.ScoredSearcher{ SearchScored(query string,k int)([]ScoredEntry,error) }`; `ScoredEntry{Entry Entry; Score float64}`; `Entry{Type,Title,Description,Resource string; Tags []string; Body,Path string}` (`internal/catalog/catalog.go:98-143`, `entry.go:6-14`).
- `providers.Investigation{Title string; RootCauses []Hypothesis; Changes []Change; Unresolved []string; Confidence float64; Actions []Action; CuratedURL string}`; `Hypothesis{Summary,Confidence,ChangeRef string; Evidence []string; SuggestedAction string; Reversible bool}`; `Change{Workload Workload; ...}`; `Workload{Kind,Name,Namespace string}`.
- `buildCurator(cfg *config.Config, token forgeToken, log) *curator.Curator` builds `github.New(...)` and returns `&curator.Curator{Issues: client, MinConfidencePR: 0.75, Log: log}`; called in `buildInvestigator`, and `OnComplete` calls `cur.Curate(ctx, found)` *before* `notifier.Deliver` (`cmd/lore/main.go:514-530, 805`).
- Test idiom: interface fakes (`fakeIssues` in `curator_test.go`) + `httptest` (`github_test.go`, `New(srv.URL, "o","r","main", staticToken("tok"))`). Gate: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/curator/fingerprint.go` + `_test.go` *(create)* | `Fingerprint(inv)` query string + `Novelty` (catalog dedup via SearchScored) |
| `internal/forge/github/github.go` + `github_test.go` *(modify)* | add `ListPRsByLabel(ctx,label) ([]providers.CuratedIssue,error)` |
| `internal/providers/providers.go` *(modify)* | add `CurationForge` interface (`OpenPR` + `ListPRsByLabel` + `Comment`) |
| `internal/curator/draft.go` + `_test.go` *(create; move+expand `draftKBEntry`)* | upgraded OKF drafter (decision card + Symptom/Investigate/Cause/Resolution) |
| `internal/curator/curator.go` + `curator_test.go` *(modify)* | new 3-step `Curate` (dedup → quality gate → draft PR \| coalesce \| drop); delete OpenIssue branch |
| `cmd/lore/main.go` *(modify `buildCurator`)* + `internal/config/config.go` | inject catalog searcher + thresholds; config knobs |

---

## Task 1: Fingerprint + catalog novelty check

**Files:** Create `internal/curator/fingerprint.go`, `internal/curator/fingerprint_test.go`

- [ ] **Step 1: Write the failing test** — `internal/curator/fingerprint_test.go`:

```go
package curator

import (
	"context"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

func TestFingerprintIncludesTitleRootAndWorkload(t *testing.T) {
	inv := providers.Investigation{
		Title:      "HarborRegistryDown",
		RootCauses: []providers.Hypothesis{{Summary: "IAM AccessKeysPerUser quota exceeded"}},
		Changes:    []providers.Change{{Workload: providers.Workload{Namespace: "tooling", Name: "harbor-registry"}}},
	}
	fp := Fingerprint(inv)
	for _, want := range []string{"HarborRegistryDown", "IAM AccessKeysPerUser quota", "tooling", "harbor-registry"} {
		if !contains(fp, want) {
			t.Fatalf("fingerprint %q missing %q", fp, want)
		}
	}
}

// fakeScored is a catalog.ScoredSearcher returning a fixed hit.
type fakeScored struct {
	score float64
	title string
}

func (f fakeScored) SearchScored(_ string, _ int) ([]catalog.ScoredEntry, error) {
	if f.title == "" {
		return nil, nil
	}
	return []catalog.ScoredEntry{{Entry: catalog.Entry{Title: f.title}, Score: f.score}}, nil
}

func TestNoveltyDuplicateAboveThreshold(t *testing.T) {
	inv := providers.Investigation{Title: "HarborRegistryDown"}
	n := Novelty{Catalog: fakeScored{score: 9.0, title: "HarborRegistryDown"}, DupScore: 5.0}
	dup, hit, err := n.IsDuplicate(context.Background(), inv)
	if err != nil {
		t.Fatal(err)
	}
	if !dup || hit.Title != "HarborRegistryDown" {
		t.Fatalf("expected duplicate hit, got dup=%v hit=%+v", dup, hit)
	}
}

func TestNoveltyBelowThresholdIsNovel(t *testing.T) {
	inv := providers.Investigation{Title: "BrandNewThing"}
	n := Novelty{Catalog: fakeScored{score: 1.0, title: "Unrelated"}, DupScore: 5.0}
	dup, _, err := n.IsDuplicate(context.Background(), inv)
	if err != nil || dup {
		t.Fatalf("expected novel, got dup=%v err=%v", dup, err)
	}
}

func TestNoveltyNilCatalogIsNovel(t *testing.T) {
	n := Novelty{Catalog: nil, DupScore: 5.0}
	dup, _, err := n.IsDuplicate(context.Background(), providers.Investigation{Title: "x"})
	if err != nil || dup {
		t.Fatalf("nil catalog must be novel, got dup=%v err=%v", dup, err)
	}
}

func contains(s, sub string) bool { return len(sub) == 0 || (len(s) >= len(sub) && indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
```

- [ ] **Step 2: Run to verify FAIL** — `cd /home/smana/Sources/runlore && go test ./internal/curator/ -run 'TestFingerprint|TestNovelty' -v`

- [ ] **Step 3: Implement** — `internal/curator/fingerprint.go`:

```go
package curator

import (
	"context"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// Fingerprint builds the dedup query string for a finding: the alert/title, the
// top root-cause signature, and the affected workload. It is a BM25 query (fuzzy),
// not a hash — matched against the catalog index and open-PR titles.
func Fingerprint(inv providers.Investigation) string {
	var b strings.Builder
	b.WriteString(inv.Title)
	if len(inv.RootCauses) > 0 {
		b.WriteString(" " + inv.RootCauses[0].Summary)
	}
	if len(inv.Changes) > 0 {
		w := inv.Changes[0].Workload
		b.WriteString(" " + w.Namespace + " " + w.Name)
	}
	return strings.TrimSpace(b.String())
}

// Novelty decides whether a finding duplicates an existing catalog entry, by
// scoring its fingerprint against the BM25 catalog index.
type Novelty struct {
	Catalog  catalog.ScoredSearcher // nil → everything is novel (no catalog configured)
	DupScore float64                // top-hit BM25 score ≥ this ⇒ duplicate
}

// IsDuplicate returns true + the matching entry when the catalog already covers
// this finding.
func (n Novelty) IsDuplicate(ctx context.Context, inv providers.Investigation) (bool, catalog.Entry, error) {
	if n.Catalog == nil {
		return false, catalog.Entry{}, nil
	}
	hits, err := n.Catalog.SearchScored(Fingerprint(inv), 1)
	if err != nil {
		return false, catalog.Entry{}, err
	}
	if len(hits) > 0 && hits[0].Score >= n.DupScore {
		return true, hits[0].Entry, nil
	}
	return false, catalog.Entry{}, nil
}
```

(`ctx` is unused by the in-memory index today but kept in the signature for symmetry with the forge calls and future remote indexes. If golangci-lint's `unparam`/`revive` flags it, keep it — it's part of the deliberate interface shape; do not remove.)

- [ ] **Step 4: Run to verify PASS** — same command. Expect PASS.

- [ ] **Step 5: Commit**
```bash
cd /home/smana/Sources/runlore
git add internal/curator/fingerprint.go internal/curator/fingerprint_test.go
git commit -m "feat(curator): fingerprint + catalog novelty check"
```

---

## Task 2: Forge — `ListPRsByLabel`

**Files:** Modify `internal/forge/github/github.go`; add test to `internal/forge/github/github_test.go`

Context: `ListIssuesByLabel` already exists but **filters PRs out** (GitHub's `/issues` endpoint returns both; it drops the ones with a `pull_request` field). We need the inverse for dedup: open *PRs* carrying the `runlore` label.

- [ ] **Step 1: Write the failing test** — append to `internal/forge/github/github_test.go`:

```go
func TestListPRsByLabel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/issues" || r.URL.Query().Get("labels") != "runlore" || r.URL.Query().Get("state") != "open" {
			t.Fatalf("unexpected request: %s?%s", r.URL.Path, r.URL.RawQuery)
		}
		// one PR (has pull_request), one plain issue (no pull_request) → only the PR is returned
		_, _ = w.Write([]byte(`[
		  {"number":48,"title":"KB: HarborRegistryDown","body":"b","labels":[{"name":"runlore"},{"name":"triggered"}],"pull_request":{"url":"x"}},
		  {"number":39,"title":"Harbor install failing","body":"b","labels":[{"name":"runlore"}]}
		]`))
	}))
	defer srv.Close()

	c := New(srv.URL, "o", "r", "main", staticToken("tok"))
	prs, err := c.ListPRsByLabel(context.Background(), "runlore")
	if err != nil {
		t.Fatalf("ListPRsByLabel: %v", err)
	}
	if len(prs) != 1 || prs[0].Number != 48 || prs[0].Title != "KB: HarborRegistryDown" {
		t.Fatalf("want only PR #48, got %+v", prs)
	}
	if len(prs[0].Labels) != 2 || prs[0].Labels[0] != "runlore" {
		t.Fatalf("labels not parsed: %+v", prs[0].Labels)
	}
}
```

- [ ] **Step 2: Run to verify FAIL** — `cd /home/smana/Sources/runlore && go test ./internal/forge/github/ -run TestListPRsByLabel -v`

- [ ] **Step 3: Implement** — add to `internal/forge/github/github.go` (model it on the existing `ListIssuesByLabel`; inspect that method first to mirror its JSON-decoding shape). Add after `ListIssuesByLabel`:

```go
// ListPRsByLabel returns open PRs carrying the given label. GitHub's issues
// endpoint returns both issues and PRs; this keeps ONLY the entries that have a
// pull_request object (the inverse of ListIssuesByLabel).
func (c *Client) ListPRsByLabel(ctx context.Context, label string) ([]providers.CuratedIssue, error) {
	var raw []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		Body        string `json:"body"`
		Labels      []struct {
			Name string `json:"name"`
		} `json:"labels"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request"`
	}
	path := fmt.Sprintf("/repos/%s/%s/issues?state=open&labels=%s&per_page=100", c.owner, c.repo, url.QueryEscape(label))
	if err := c.do(ctx, http.MethodGet, path, nil, &raw); err != nil {
		return nil, err
	}
	var out []providers.CuratedIssue
	for _, it := range raw {
		if it.PullRequest == nil {
			continue // a plain issue, not a PR
		}
		labels := make([]string, len(it.Labels))
		for i, l := range it.Labels {
			labels[i] = l.Name
		}
		out = append(out, providers.CuratedIssue{Number: it.Number, Title: it.Title, Body: it.Body, Labels: labels})
	}
	return out, nil
}
```

Add `"net/url"` to the imports if not present. **Before implementing, read the existing `ListIssuesByLabel` (github.go:162) and match its exact decoding/label-parsing style** — if it already has a shared decode helper or a `CuratedIssue` mapping, reuse it rather than duplicating.

- [ ] **Step 4: Run to verify PASS** — same command.

- [ ] **Step 5: Gate + commit**
```bash
cd /home/smana/Sources/runlore
go test ./internal/forge/github/ && gofmt -l internal/forge/github && golangci-lint run ./internal/forge/github/...
git add internal/forge/github/github.go internal/forge/github/github_test.go
git commit -m "feat(forge): ListPRsByLabel (open KB PRs, for dedup)"
```

---

## Task 3: `CurationForge` interface

**Files:** Modify `internal/providers/providers.go`

The curator needs more than `IssueProvider` (open-only): it must list open PRs and comment to coalesce. Define a focused interface the `*github.Client` already satisfies.

- [ ] **Step 1: Write the failing test** — `internal/providers/curationforge_test.go`:

```go
package providers_test

import (
	"github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/providers"
)

// compile-time assertion: the GitHub client satisfies CurationForge.
var _ providers.CurationForge = (*github.Client)(nil)
```

- [ ] **Step 2: Run to verify FAIL** — `cd /home/smana/Sources/runlore && go test ./internal/providers/ -run xxx -v` (expect a COMPILE error: `CurationForge` undefined).

- [ ] **Step 3: Implement** — add to `internal/providers/providers.go` near `IssueProvider`:

```go
// CurationForge is the forge surface the curator's file-time gate needs: open a
// drafted PR, list open KB PRs (dedup), and comment to coalesce duplicates.
type CurationForge interface {
	OpenPR(ctx context.Context, entry KBEntry) (Ref, error)
	ListPRsByLabel(ctx context.Context, label string) ([]CuratedIssue, error)
	Comment(ctx context.Context, number int, body string) error
}
```

- [ ] **Step 4: Run to verify PASS** — `go build ./... && go test ./internal/providers/`. (If the assertion fails, `*github.Client` is missing a method — fix the method set, not the interface.)

- [ ] **Step 5: Commit**
```bash
cd /home/smana/Sources/runlore
git add internal/providers/providers.go internal/providers/curationforge_test.go
git commit -m "feat(providers): CurationForge interface (open PR + list PRs + comment)"
```

---

## Task 4: Upgraded OKF drafter (decision card + full sections)

**Files:** Create `internal/curator/draft.go`, `internal/curator/draft_test.go`. (Move `draftKBEntry` + `firstLine` out of `curator.go` into `draft.go` and expand.)

- [ ] **Step 1: Write the failing test** — `internal/curator/draft_test.go`:

```go
package curator

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestDraftKBEntryHasDecisionCardAndSections(t *testing.T) {
	inv := providers.Investigation{
		Title:      "HarborRegistryDown",
		Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{
			Summary: "IAM AccessKeysPerUser:2 quota → Secret missing username",
			Evidence: []string{"accesskey/xplane-harbor failed", "CreateContainerConfigError"},
			SuggestedAction: "delete an old IAM access key", Reversible: false, ChangeRef: "crossplane/xplane-harbor",
		}},
		Unresolved: []string{"which key to delete"},
	}
	e := draftKBEntry(inv)
	if e.Type != "Incident" || e.Title != "HarborRegistryDown" {
		t.Fatalf("meta: %+v", e)
	}
	body := e.Body
	for _, want := range []string{
		"## Decision", "why keep", "confidence", // decision card
		"## Symptom", "## Investigate", "## Cause", "## Resolution", // OKF sections
		"IAM AccessKeysPerUser:2", "delete an old IAM access key", "crossplane/xplane-harbor",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}
```

- [ ] **Step 2: Run to verify FAIL** — `cd /home/smana/Sources/runlore && go test ./internal/curator/ -run TestDraftKBEntry -v`

- [ ] **Step 3: Implement** — create `internal/curator/draft.go` (move the existing `draftKBEntry`/`firstLine` here and replace the body builder):

```go
package curator

import (
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// draftKBEntry renders an investigation as a merge-ready OKF knowledge entry: a
// decision card (why-keep + confidence) followed by the OKF sections
// Symptom / Investigate / Cause / Resolution. The decision card makes the human
// merge trivial; the sections make the entry reusable knowledge (the #48 standard).
func draftKBEntry(inv providers.Investigation) providers.KBEntry {
	var b strings.Builder

	// --- decision card ---
	fmt.Fprintf(&b, "## Decision\n\n")
	fmt.Fprintf(&b, "- **why keep:** %s\n", firstLine(inv))
	fmt.Fprintf(&b, "- **confidence:** %.0f%%\n", inv.Confidence*100)
	if cr := changeRefs(inv); cr != "" {
		fmt.Fprintf(&b, "- **provenance:** %s\n", cr)
	}

	// --- Symptom ---
	fmt.Fprintf(&b, "\n## Symptom\n\n%s\n", inv.Title)

	// --- Investigate (evidence trail) ---
	b.WriteString("\n## Investigate\n\n")
	for _, rc := range inv.RootCauses {
		for _, e := range rc.Evidence {
			fmt.Fprintf(&b, "- %s\n", e)
		}
	}

	// --- Cause (ranked root causes) ---
	b.WriteString("\n## Cause\n\n")
	for i, rc := range inv.RootCauses {
		fmt.Fprintf(&b, "%d. **%s** (%.0f%%)", i+1, rc.Summary, rc.Confidence*100)
		if rc.ChangeRef != "" {
			fmt.Fprintf(&b, " — change: %s", rc.ChangeRef)
		}
		b.WriteString("\n")
	}

	// --- Resolution (suggested, reversible-first) ---
	b.WriteString("\n## Resolution\n\n")
	for _, rc := range inv.RootCauses {
		if rc.SuggestedAction != "" {
			fmt.Fprintf(&b, "- %s (reversible=%t)\n", rc.SuggestedAction, rc.Reversible)
		}
	}
	if len(inv.Unresolved) > 0 {
		b.WriteString("\n## Unresolved\n\n")
		for _, u := range inv.Unresolved {
			fmt.Fprintf(&b, "- %s\n", u)
		}
	}

	return providers.KBEntry{
		Type:        "Incident",
		Title:       inv.Title,
		Description: firstLine(inv),
		Tags:        []string{"runlore", "incident"},
		Body:        b.String(),
	}
}

func firstLine(inv providers.Investigation) string {
	if len(inv.RootCauses) > 0 {
		return inv.RootCauses[0].Summary
	}
	return inv.Title
}

// changeRefs collects the distinct change references cited across root causes
// (the causing/fixing-change provenance the merge bar requires).
func changeRefs(inv providers.Investigation) string {
	var refs []string
	seen := map[string]bool{}
	for _, rc := range inv.RootCauses {
		if rc.ChangeRef != "" && !seen[rc.ChangeRef] {
			seen[rc.ChangeRef] = true
			refs = append(refs, rc.ChangeRef)
		}
	}
	return strings.Join(refs, ", ")
}
```

Then **delete** the old `draftKBEntry` and `firstLine` from `curator.go` (they now live in `draft.go`).

- [ ] **Step 4: Run to verify PASS** — `cd /home/smana/Sources/runlore && go test ./internal/curator/ -run TestDraftKBEntry -v`

- [ ] **Step 5: Commit**
```bash
cd /home/smana/Sources/runlore
git add internal/curator/draft.go internal/curator/draft_test.go internal/curator/curator.go
git commit -m "feat(curator): merge-ready OKF drafter (decision card + Symptom/Investigate/Cause/Resolution)"
```

---

## Task 5: Refactor `Curate` — dedup → quality gate → draft (delete issue branch)

**Files:** Modify `internal/curator/curator.go`, `internal/curator/curator_test.go`

- [ ] **Step 1: Write the failing test** — replace the body of `internal/curator/curator_test.go` with:

```go
package curator

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// fakeForge records OpenPR / Comment calls and serves a fixed open-PR list.
type fakeForge struct {
	openedPR  *providers.KBEntry
	commented []int
	openPRs   []providers.CuratedIssue
}

func (f *fakeForge) OpenPR(_ context.Context, e providers.KBEntry) (providers.Ref, error) {
	f.openedPR = &e
	return providers.Ref{URL: "https://github.com/x/y/pull/2"}, nil
}
func (f *fakeForge) ListPRsByLabel(_ context.Context, _ string) ([]providers.CuratedIssue, error) {
	return f.openPRs, nil
}
func (f *fakeForge) Comment(_ context.Context, n int, _ string) error {
	f.commented = append(f.commented, n)
	return nil
}

func newCurator(f *fakeForge, cat catalog.ScoredSearcher) *Curator {
	return &Curator{
		Forge: f, Catalog: cat, DupScore: 5.0, MinConfidence: 0.75,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func goodFinding() providers.Investigation {
	return providers.Investigation{
		Title: "HarborRegistryDown", Confidence: 0.9,
		RootCauses: []providers.Hypothesis{{
			Summary: "IAM quota exceeded", Confidence: 0.9,
			Evidence: []string{"CreateContainerConfigError"}, ChangeRef: "xplane-harbor", SuggestedAction: "delete a key",
		}},
	}
}

func TestCurateNovelHighQualityOpensPR(t *testing.T) {
	f := &fakeForge{}
	ref, err := newCurator(f, fakeScored{}).Curate(context.Background(), goodFinding())
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR == nil || ref.URL == "" {
		t.Fatalf("expected a PR, got %+v / %s", f.openedPR, ref.URL)
	}
}

func TestCurateDuplicateCoalescesNoPR(t *testing.T) {
	f := &fakeForge{openPRs: []providers.CuratedIssue{{Number: 48, Title: "KB: HarborRegistryDown"}}}
	// catalog also has it → IsDuplicate true; AND an open PR with a matching title.
	ref, err := newCurator(f, fakeScored{score: 9, title: "HarborRegistryDown"}).Curate(context.Background(), goodFinding())
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR != nil {
		t.Fatalf("duplicate must NOT open a PR, got %+v", f.openedPR)
	}
	if len(f.commented) == 0 {
		t.Fatal("duplicate should coalesce by commenting on the existing artifact")
	}
	if ref.URL != "" {
		t.Fatalf("duplicate ref should be empty, got %s", ref.URL)
	}
}

func TestCurateLowQualityDropsNoArtifact(t *testing.T) {
	f := &fakeForge{}
	weak := providers.Investigation{Title: "vague", Confidence: 0.3} // below bar: no root cause, low conf
	ref, err := newCurator(f, fakeScored{}).Curate(context.Background(), weak)
	if err != nil {
		t.Fatal(err)
	}
	if f.openedPR != nil || len(f.commented) != 0 || ref.URL != "" {
		t.Fatalf("low-quality finding must produce no repo artifact, got pr=%+v comment=%v ref=%s", f.openedPR, f.commented, ref.URL)
	}
}
```

- [ ] **Step 2: Run to verify FAIL** — `cd /home/smana/Sources/runlore && go test ./internal/curator/ -run TestCurate -v` (compile error: `Curator` fields changed).

- [ ] **Step 3: Implement** — replace `internal/curator/curator.go` (keep the package doc comment at the top):

```go
package curator

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// Curator is the file-time learning gate: it dedups a finding against the catalog
// and open PRs, gates on quality, and drafts a merge-ready PR for novel, quality
// findings. Uncertain/low-quality findings produce NO repo artifact (the chat
// delivery already informed the human). It never opens issues — the only issues
// are knowledge-gap issues, opened by the curate agent (Phase 2).
type Curator struct {
	Forge         providers.CurationForge
	Catalog       catalog.ScoredSearcher // nil ⇒ no catalog dedup
	DupScore      float64                // catalog BM25 dup threshold
	MinConfidence float64                // quality gate: minimum overall confidence
	Log           *slog.Logger
}

// Curate applies the three-step gate. It returns the created PR ref, or an empty
// ref when the finding was coalesced (duplicate) or dropped (below the bar).
func (c *Curator) Curate(ctx context.Context, inv providers.Investigation) (providers.Ref, error) {
	// 1. dedup — catalog, then open PRs
	if dup, hit, err := (Novelty{Catalog: c.Catalog, DupScore: c.DupScore}).IsDuplicate(ctx, inv); err != nil {
		c.Log.Warn("dedup: catalog search failed", "err", err)
	} else if dup {
		c.Log.Info("finding duplicates a catalog entry; not filing", "entry", hit.Title)
		return providers.Ref{}, nil
	}
	if n, ok, err := c.duplicateOpenPR(ctx, inv); err != nil {
		c.Log.Warn("dedup: list open PRs failed", "err", err)
	} else if ok {
		if err := c.Forge.Comment(ctx, n, coalesceComment(inv)); err != nil {
			return providers.Ref{}, fmt.Errorf("coalesce comment: %w", err)
		}
		c.Log.Info("finding coalesced onto an open PR", "pr", n)
		return providers.Ref{}, nil
	}

	// 2. quality gate — below the bar ⇒ no repo artifact (chat alert only)
	if !meetsBar(inv, c.MinConfidence) {
		c.Log.Info("finding below the quality bar; chat-only, no KB artifact",
			"confidence", inv.Confidence, "root_causes", len(inv.RootCauses))
		return providers.Ref{}, nil
	}

	// 3. draft a merge-ready PR (labelled solved by the forge lifecycle labels)
	ref, err := c.Forge.OpenPR(ctx, draftKBEntry(inv))
	if err != nil {
		return providers.Ref{}, fmt.Errorf("open PR: %w", err)
	}
	c.Log.Info("curated as PR", "url", ref.URL, "confidence", inv.Confidence)
	return ref, nil
}

// duplicateOpenPR reports an open KB PR whose normalized title matches this
// finding (cheap title-slug match; deep cross-incident dedup is the curate agent).
func (c *Curator) duplicateOpenPR(ctx context.Context, inv providers.Investigation) (int, bool, error) {
	prs, err := c.Forge.ListPRsByLabel(ctx, "runlore")
	if err != nil {
		return 0, false, err
	}
	want := normTitle(inv.Title)
	for _, pr := range prs {
		if normTitle(strings.TrimPrefix(pr.Title, "KB: ")) == want {
			return pr.Number, true, nil
		}
	}
	return 0, false, nil
}

// meetsBar is the file-time QUALITY gate (not the merge condition): a confirmed,
// confident root cause with cited evidence. The resolved/accepted MERGE condition
// is enforced later by the curate agent + the human.
func meetsBar(inv providers.Investigation, minConf float64) bool {
	if inv.Confidence < minConf || len(inv.RootCauses) == 0 {
		return false
	}
	top := inv.RootCauses[0]
	return top.Summary != "" && len(top.Evidence) > 0
}

func normTitle(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func coalesceComment(inv providers.Investigation) string {
	return fmt.Sprintf("RunLore saw this incident again (confidence %.0f%%). Coalesced rather than re-filed.", inv.Confidence*100)
}
```

- [ ] **Step 4: Run to verify PASS** — `cd /home/smana/Sources/runlore && go test ./internal/curator/ -v` (all curator tests).

- [ ] **Step 5: Gate + commit**
```bash
cd /home/smana/Sources/runlore
go build ./... && go vet ./... && go test ./internal/curator/ ./internal/forge/... ./internal/providers/ && gofmt -l internal && golangci-lint run ./internal/...
git add internal/curator/curator.go internal/curator/curator_test.go
git commit -m "feat(curator): three-step file-time gate (dedup -> quality -> PR); delete issue branch"
```

---

## Task 6: Wire `buildCurator` + config knobs

**Files:** Modify `cmd/lore/main.go` (`buildCurator`, ~514-530), `internal/config/config.go`

- [ ] **Step 1: Add config knobs** — in `internal/config/config.go`, extend the `Forge` (or a new `Curation`) struct. Add to the `Forge` struct:

```go
	DupScore      float64 `yaml:"dup_score"`      // catalog BM25 dedup threshold (default 5.0)
	MinConfidence float64 `yaml:"min_confidence"` // file-time quality gate (default 0.75)
```

- [ ] **Step 2: Update `buildCurator`** — it must now pass the catalog searcher + thresholds. `buildCurator`'s signature gains the catalog (built in `buildModelAndTools`/`buildCatalog`). Change it to:

```go
// buildCurator returns a Curator when the GitHub App token + KB repo are
// configured, else nil. cat may be nil (no catalog ⇒ no catalog dedup).
func buildCurator(cfg *config.Config, token forgeToken, cat catalog.ScoredSearcher, log *slog.Logger) *curator.Curator {
	if token == nil || cfg.Forge.KBRepo == "" {
		return nil
	}
	owner, repo, ok := strings.Cut(cfg.Forge.KBRepo, "/")
	if !ok {
		log.Warn("curator disabled: kb_repo must be owner/name", "kb_repo", cfg.Forge.KBRepo)
		return nil
	}
	base := cfg.Forge.BaseBranch
	if base == "" {
		base = "main"
	}
	dup := cfg.Forge.DupScore
	if dup == 0 {
		dup = 5.0
	}
	minConf := cfg.Forge.MinConfidence
	if minConf == 0 {
		minConf = 0.75
	}
	client := github.New(cfg.Forge.GitHubAPIURL, owner, repo, base, github.TokenFunc(token))
	log.Info("curator enabled", "repo", cfg.Forge.KBRepo, "dup_score", dup, "min_confidence", minConf)
	return &curator.Curator{Forge: client, Catalog: cat, DupScore: dup, MinConfidence: minConf, Log: log}
}
```

Update the **call site** in `buildInvestigator` (main.go ~759-805): the catalog is built there as `cat` inside `buildModelAndTools` — thread it out (or call `buildCatalog` once and pass both to the tools and the curator). Change `cur := buildCurator(cfg, buildForgeTokenSource(cfg, log), log)` to pass the catalog: `cur := buildCurator(cfg, buildForgeTokenSource(cfg, log), cat, log)` where `cat` is the `*catalog.Catalog` already built for `kb_search` (it satisfies `catalog.ScoredSearcher`). If `buildModelAndTools` does not currently return the catalog, have it also return `cat` (it builds it internally at line 641) and thread it through — a small signature change, done in this task.

- [ ] **Step 3: Build + gate** — `cd /home/smana/Sources/runlore && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — all green.

- [ ] **Step 4: Smoke** — serve still starts with curation enabled (or cleanly disabled without a forge):
```bash
cd /home/smana/Sources/runlore && go build -o /tmp/lore ./cmd/lore && /tmp/lore version
```

- [ ] **Step 5: Commit**
```bash
cd /home/smana/Sources/runlore
git add cmd/lore/main.go internal/config/config.go
git commit -m "feat(curator): wire catalog dedup + thresholds into buildCurator"
```

---

## Self-Review

- **Spec coverage** (`2026-06-21-runlore-learning-curation-workflow-design.md`): §4 dedup (catalog Task 1 + open-PR Task 2/5), quality gate (Task 5 `meetsBar`), merge-ready PR decision card + OKF sections (Task 4), **delete issue branch** (Task 5 — no `OpenIssue` call remains), forge "list open PRs" (Task 2), config knobs (Task 6). The §3 *merge condition* (`resolved`/`accepted`) is deliberately **out of Phase 1** (it gates merge, enforced by Phase 2 + human) — Phase 1 labels PRs `solved` via the existing `lifecycleLabels`. Coalesce uses the existing `Comment`. Cross-incident fuzzy dedup of the standing backlog is **Phase 2** (Plan 2), noted in Task 5's `duplicateOpenPR` comment.
- **Placeholder scan:** every code step is complete; the `ctx`-unused note (Task 1) and the "read the sibling method first" note (Task 2) are guidance, not placeholders.
- **Type consistency:** `Fingerprint`/`Novelty`/`IsDuplicate` (Task 1) consumed by `Curate` (Task 5); `CurationForge` (Task 3: `OpenPR`+`ListPRsByLabel`+`Comment`) is exactly the fake `fakeForge` and the real `*github.Client` method set (Tasks 2,5); `draftKBEntry` (Task 4) consumed by `Curate` (Task 5); `Curator` fields `{Forge,Catalog,DupScore,MinConfidence,Log}` are identical across Tasks 5 and 6. `catalog.ScoredSearcher`/`ScoredEntry`/`Entry`, `providers.CuratedIssue{Number,Title,Body,Labels}`, `KBEntry`, `Investigation`/`Hypothesis`/`Change`/`Workload` match the verbatim signatures.

## What this delivers

After Phase 1, the curator files **only novel, quality-passing** findings as **merge-ready** PRs (decision card + full OKF), coalesces duplicates onto the existing PR/entry instead of re-filing, and **never opens an issue**. The wall of duplicate low-quality PRs stops at the source. The standing backlog cleanup, draft upgrades, recurrence→gap-issues, the `resolved`-gated decision-ready queue, and lifecycle/decay are **Plan 2** (`...-phase2-curate-agent.md`).
