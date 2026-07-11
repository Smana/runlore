# `lore kb search` / `lore kb show` CLI Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A human search surface over the existing BM25 knowledge index: `lore kb search "<query>"` prints a readable hits table (optional resolve-rate from a ledger file, optional JSON), `lore kb show <entry>` prints one full entry. Spec: `dev/superpowers/specs/2026-07-07-kb-human-surfaces-design.md`, Feature 2.

**Architecture:** A new `internal/app/kb_cmd.go` hosts `RunKB` (dispatch) + `runKBSearch`/`runKBShow`, writing to an injected `io.Writer` for testability. The catalog loads like `lore mcp` does: explicit `--dir` wins, else config `catalog.dir` (the CLI never clones — it points at `lore catalog sync`). Resolve-rate is opt-in via `--ledger <jsonl>` (the ledger lives in-cluster, not on workstations) using `outcome.OpenCounts()` keyed by entry path. Output is a stdlib `text/tabwriter` table — no ANSI, no new dependencies.

**Tech Stack:** Go 1.26, stdlib only (`flag`, `text/tabwriter`, `encoding/json`). Tests: `go test -race ./...`, table-driven with fixture catalogs in `t.TempDir()`. Lint: `golangci-lint run ./...`, `gofmt`.

## Global Constraints

- Go 1.26 (go.mod); CI runs `go build ./...`, `go vet ./...`, `test -z "$(gofmt -l .)"`, `go test -race ./...`.
- Never add AI attribution to commits or PRs. Conventional-commit prefixes (`feat:`, `fix:`, `test:`, `docs:`).
- Zero new dependencies. No ANSI escape codes (tabwriter alignment suffices; also keeps output pipe-safe).
- Scripting contract: no results ⇒ non-zero exit (return an error); `--json` output is machine-stable.
- An unreadable `--ledger` warns and omits the RESOLVE column — it never fails the search. It must also never CREATE the file (stat before open).
- Keep the comment density/style of surrounding code (this repo comments the *why* heavily).
- Work on a dedicated branch (e.g. `feat/lore-kb-cli`), one PR for this whole plan.

---

### Task 1: `RunKB` dispatch + `kb search` with the hits table

**Files:**
- Create: `internal/app/kb_cmd.go`
- Test: `internal/app/kb_cmd_test.go`

**Interfaces:**
- Consumes: `catalog.New(dir)`, `Catalog.SearchScored(query, k)`, `catalog.ScoredEntry{Entry, Score}`, `config.Load(path)` (`cfg.Catalog.Dir`, `cfg.Catalog.Git.URL`).
- Produces: `RunKB(args []string) error` (dispatch: `search` | `show`); `runKBSearch(args []string, w io.Writer) error`; `loadKBCatalog(cfgPath, dir string) (*catalog.Catalog, error)`; `relAge(ts string) string`. Tasks 2–4 extend these; Task 5 wires `RunKB` into `main.go`.

- [ ] **Step 1: Write the failing test** — create `internal/app/kb_cmd_test.go`:

```go
package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeKBFixture materializes a small OKF catalog: two entries whose lexical
// overlap with the test queries is deliberately asymmetric.
func writeKBFixture(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	ts := time.Now().Add(-12 * 24 * time.Hour).UTC().Format(time.RFC3339)
	entries := map[string]string{
		"incidents/crashloop-web.md": "---\ntype: Incident\ntitle: CrashLoop web ConfigMap truncated\ndescription: web pods crashloop after kustomize bump\nresource: apps/web\ntags: [runlore, incident]\ntimestamp: \"" + ts + "\"\n---\n## Cause\n\nConfigMap truncated\n\n## Resolution\n\nrevert the patch\n",
		"incidents/oom-worker.md":    "---\ntype: Incident\ntitle: OOM worker limits too low\ndescription: worker OOMKilled under load\nresource: apps/worker\n---\n## Cause\n\nlimits too low\n",
	}
	for path, content := range entries {
		full := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestKBSearchTable(t *testing.T) {
	dir := writeKBFixture(t)
	var out strings.Builder
	err := runKBSearch([]string{"--dir", dir, "crashloop", "web", "configmap"}, &out)
	if err != nil {
		t.Fatalf("runKBSearch: %v", err)
	}
	got := out.String()
	for _, want := range []string{"SCORE", "ENTRY", "TITLE", "RESOURCE", "LAST SEEN",
		"incidents/crashloop-web.md", "apps/web", "12d ago"} {
		if !strings.Contains(got, want) {
			t.Errorf("table missing %q:\n%s", want, got)
		}
	}
	// The lexically-distant entry may or may not appear, but the best match must
	// be the FIRST data row.
	lines := strings.Split(strings.TrimSpace(got), "\n")
	if len(lines) < 2 || !strings.Contains(lines[1], "crashloop-web") {
		t.Errorf("best hit is not the first row:\n%s", got)
	}
	// No RESOLVE column without --ledger.
	if strings.Contains(got, "RESOLVE") {
		t.Errorf("RESOLVE column must be absent without --ledger:\n%s", got)
	}
}

func TestKBSearchNoResultsIsError(t *testing.T) {
	dir := writeKBFixture(t)
	var out strings.Builder
	if err := runKBSearch([]string{"--dir", dir, "zzz-nothing-matches-this"}, &out); err == nil {
		t.Fatal("want error on zero hits (non-zero exit for scripting)")
	}
}

func TestKBSearchUsageErrors(t *testing.T) {
	var out strings.Builder
	if err := runKBSearch([]string{"--dir", t.TempDir()}, &out); err == nil {
		t.Fatal("want usage error when the query is empty")
	}
	if err := RunKB([]string{"frobnicate"}); err == nil {
		t.Fatal("want usage error on unknown subcommand")
	}
	if err := RunKB(nil); err == nil {
		t.Fatal("want usage error with no subcommand")
	}
}

func TestRelAge(t *testing.T) {
	now := time.Now()
	cases := []struct {
		ts   string
		want string
	}{
		{now.Add(-30 * time.Minute).Format(time.RFC3339), "30m ago"},
		{now.Add(-5 * time.Hour).Format(time.RFC3339), "5h ago"},
		{now.Add(-26 * time.Hour).Format(time.RFC3339), "1d ago"},
		{"", ""},          // hand-written entry without a timestamp
		{"not-a-date", ""}, // malformed
		{now.Add(time.Hour).Format(time.RFC3339), ""}, // future: clock skew, say nothing
	}
	for _, c := range cases {
		if got := relAge(c.ts); got != c.want {
			t.Errorf("relAge(%q) = %q, want %q", c.ts, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/app/ -run 'TestKBSearch|TestRelAge' -v`
Expected: compile FAIL — `runKBSearch`, `RunKB`, `relAge` undefined.

- [ ] **Step 3: Implement `internal/app/kb_cmd.go`**

```go
package app

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/outcome"
)

// RunKB dispatches the human-facing knowledge-base read commands. `lore catalog
// sync` remains the machine/ops write surface; `lore kb` is how a person asks
// "what do we already know about this?" without an MCP client or a cluster.
func RunKB(args []string) error {
	const usage = "usage: lore kb search <query> [flags] | lore kb show <entry> [flags]"
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "search":
		return runKBSearch(args[1:], os.Stdout)
	case "show":
		return runKBShow(args[1:], os.Stdout)
	}
	return fmt.Errorf("unknown kb subcommand %q\n%s", args[0], usage)
}

// loadKBCatalog opens the catalog for the read commands: an explicit --dir
// wins; otherwise config catalog.dir. The CLI never clones — a git-synced
// catalog is materialized by `lore catalog sync` (or a running agent), so the
// error message points there instead of failing cryptically.
func loadKBCatalog(cfgPath, dir string) (*catalog.Catalog, error) {
	if dir == "" {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return nil, fmt.Errorf("load config: %w (or pass --dir <catalog>)", err)
		}
		dir = cfg.Catalog.Dir
		if dir == "" {
			return nil, fmt.Errorf("no catalog configured (set catalog.dir or pass --dir <catalog>)")
		}
	}
	cat, err := catalog.New(dir)
	if err != nil {
		return nil, fmt.Errorf("load catalog %s: %w (for a git-synced catalog, run `lore catalog sync` first)", dir, err)
	}
	if cat.Len() == 0 {
		return nil, fmt.Errorf("catalog %s has no entries (for a git-synced catalog, run `lore catalog sync` first)", dir)
	}
	return cat, nil
}

func runKBSearch(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("kb search", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	dir := fs.String("dir", "", "catalog directory (overrides config catalog.dir)")
	k := fs.Int("k", 10, "maximum results")
	asJSON := fs.Bool("json", false, "emit results as JSON")
	ledgerPath := fs.String("ledger", "", "outcome ledger JSONL; adds the RESOLVE column")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("usage: lore kb search <query> [--dir <catalog>] [-k 10] [--json] [--ledger <jsonl>]")
	}
	cat, err := loadKBCatalog(*cfgPath, *dir)
	if err != nil {
		return err
	}
	hits, err := cat.SearchScored(query, *k)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		return fmt.Errorf("no entries match %q", query)
	}
	counts := ledgerCounts(*ledgerPath, w)
	if *asJSON {
		return writeHitsJSON(w, hits, counts)
	}
	writeHitsTable(w, hits, counts, *ledgerPath != "")
	return nil
}

// ledgerCounts loads per-entry recall/resolve aggregates from an optional
// ledger file. The ledger lives in-cluster, so this is opt-in for humans who
// copied it locally; a missing/unreadable file warns and omits the column —
// never fails the search, and never CREATES the file (outcome.New would).
func ledgerCounts(path string, w io.Writer) map[string]outcome.Aggregate {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		fmt.Fprintf(w, "warning: ledger %s unreadable (%v); RESOLVE column omitted\n", path, err)
		return nil
	}
	l, err := outcome.New(path)
	if err != nil {
		fmt.Fprintf(w, "warning: ledger %s unreadable (%v); RESOLVE column omitted\n", path, err)
		return nil
	}
	counts, _ := l.OpenCounts() // documented always-nil error
	return counts
}

func writeHitsTable(w io.Writer, hits []catalog.ScoredEntry, counts map[string]outcome.Aggregate, withResolve bool) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	head := "SCORE\tENTRY\tTITLE\tRESOURCE\tLAST SEEN"
	if withResolve {
		head += "\tRESOLVE"
	}
	fmt.Fprintln(tw, head)
	for _, h := range hits {
		row := fmt.Sprintf("%.2f\t%s\t%s\t%s\t%s",
			h.Score, h.Entry.Path, truncateCell(h.Entry.Title, 60), h.Entry.Resource, relAge(h.Entry.Timestamp))
		if withResolve {
			// Resolve-rate is "resolved/recalled" per catalog entry; "-" for an
			// entry the ledger has never seen recalled.
			if agg := counts[h.Entry.Path]; agg.Recalls > 0 {
				row += fmt.Sprintf("\t%d/%d", agg.Resolved, agg.Recalls)
			} else {
				row += "\t-"
			}
		}
		fmt.Fprintln(tw, row)
	}
	_ = tw.Flush()
}

// truncateCell keeps table rows scannable: cap a free-text cell at max runes.
func truncateCell(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return strings.TrimRight(string(r[:max]), " ") + "…"
}

// relAge renders an entry's RFC3339 timestamp as a coarse relative age ("12d
// ago") — what a human scans for ("is this knowledge fresh?"). "" for absent,
// malformed, or future timestamps (hand-written entries carry none).
func relAge(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return ""
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}
```

Also add these two stubs at the bottom so the file compiles before Tasks 2 and 4 (each task replaces its stub):

```go
// runKBShow is implemented in Task 2 of the plan.
func runKBShow(args []string, w io.Writer) error {
	return fmt.Errorf("kb show: not implemented yet")
}

// writeHitsJSON is implemented in Task 4 of the plan.
func writeHitsJSON(w io.Writer, hits []catalog.ScoredEntry, counts map[string]outcome.Aggregate) error {
	_ = json.NewEncoder(w)
	return fmt.Errorf("kb search --json: not implemented yet")
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/app/ -race -run 'TestKBSearch|TestRelAge' -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/app/kb_cmd.go internal/app/kb_cmd_test.go
git commit -m "feat(cli): lore kb search — human search over the knowledge catalog"
```

---

### Task 2: `kb show` — print one full entry

**Files:**
- Modify: `internal/app/kb_cmd.go` (replace the `runKBShow` stub)
- Test: `internal/app/kb_cmd_test.go`

**Interfaces:**
- Consumes: `loadKBCatalog`, `Catalog.Entries()`, `Catalog.SearchScored` (Task 1).
- Produces: `runKBShow(args []string, w io.Writer) error` — exact path or filename match first, unique-search-hit fallback, disambiguation error otherwise.

- [ ] **Step 1: Write the failing test** — append to `kb_cmd_test.go`:

```go
func TestKBShow(t *testing.T) {
	dir := writeKBFixture(t)
	cases := []struct{ label, arg string }{
		{"exact path", "incidents/crashloop-web.md"},
		{"filename slug", "crashloop-web"},
		{"search fallback unique", "configmap truncated kustomize"},
	}
	for _, c := range cases {
		var out strings.Builder
		if err := runKBShow([]string{"--dir", dir, c.arg}, &out); err != nil {
			t.Fatalf("%s: runKBShow: %v", c.label, err)
		}
		got := out.String()
		for _, want := range []string{"CrashLoop web ConfigMap truncated", "type: Incident",
			"resource: apps/web", "## Cause", "revert the patch"} {
			if !strings.Contains(got, want) {
				t.Errorf("%s: output missing %q:\n%s", c.label, want, got)
			}
		}
	}
}

func TestKBShowNoMatchIsError(t *testing.T) {
	dir := writeKBFixture(t)
	var out strings.Builder
	if err := runKBShow([]string{"--dir", dir, "zzz-nothing"}, &out); err == nil {
		t.Fatal("want error when nothing matches")
	}
	if err := runKBShow([]string{"--dir", dir}, &out); err == nil {
		t.Fatal("want usage error when the entry argument is missing")
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/app/ -run TestKBShow -v`
Expected: FAIL — "not implemented yet".

- [ ] **Step 3: Implement** — replace the `runKBShow` stub:

```go
// runKBShow prints one entry in full: the frontmatter card, then the body. The
// argument is a bundle-relative path or a bare filename; when neither matches
// exactly, a search fallback accepts a UNIQUE hit and otherwise lists the
// candidates instead of guessing — showing the wrong runbook is worse than
// asking the human to pick.
func runKBShow(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("kb show", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	dir := fs.String("dir", "", "catalog directory (overrides config catalog.dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	arg := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if arg == "" {
		return fmt.Errorf("usage: lore kb show <entry-path | filename | query> [--dir <catalog>]")
	}
	cat, err := loadKBCatalog(*cfgPath, *dir)
	if err != nil {
		return err
	}
	e, ok := findEntry(cat, arg)
	if !ok {
		hits, serr := cat.SearchScored(arg, 5)
		if serr != nil {
			return serr
		}
		switch len(hits) {
		case 0:
			return fmt.Errorf("no entry matches %q", arg)
		case 1:
			e = hits[0].Entry
		default:
			var b strings.Builder
			for _, h := range hits {
				fmt.Fprintf(&b, "  %s — %s\n", h.Entry.Path, h.Entry.Title)
			}
			return fmt.Errorf("no exact match for %q; candidates:\n%s", arg, strings.TrimRight(b.String(), "\n"))
		}
	}
	writeEntry(w, e)
	return nil
}

// findEntry matches by exact bundle-relative path, then by bare filename (with
// or without the .md suffix).
func findEntry(cat *catalog.Catalog, arg string) (catalog.Entry, bool) {
	base := strings.TrimSuffix(arg, ".md")
	for _, e := range cat.Entries() {
		if e.Path == arg || strings.TrimSuffix(filepath.Base(e.Path), ".md") == base {
			return e, true
		}
	}
	return catalog.Entry{}, false
}

// writeEntry prints the frontmatter card then the markdown body — the same
// information a reviewer sees on the file, without leaving the terminal.
func writeEntry(w io.Writer, e catalog.Entry) {
	fmt.Fprintf(w, "# %s\n\n", e.Title)
	card := [][2]string{
		{"path", e.Path}, {"type", e.Type}, {"description", e.Description},
		{"resource", e.Resource}, {"tags", strings.Join(e.Tags, ", ")},
		{"last seen", relAge(e.Timestamp)}, {"fingerprint", shortFP(e.Fingerprint)},
	}
	for _, kv := range card {
		if kv[1] != "" {
			fmt.Fprintf(w, "%s: %s\n", kv[0], kv[1])
		}
	}
	fmt.Fprintf(w, "\n%s\n", strings.TrimSpace(e.Body))
}

// shortFP abbreviates the 64-hex dup fingerprint for display; identity checks
// belong to machines, humans only need "has one / which one roughly".
func shortFP(fp string) string {
	if len(fp) > 12 {
		return fp[:12] + "…"
	}
	return fp
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/app/ -race -run TestKBShow -v`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add internal/app/kb_cmd.go internal/app/kb_cmd_test.go
git commit -m "feat(cli): lore kb show — print one knowledge entry in full"
```

---

### Task 3: `--ledger` resolve-rate column

**Files:**
- Modify: `internal/app/kb_cmd_test.go` (the plumbing already exists from Task 1 — this task proves it end-to-end)
- Test: `internal/app/kb_cmd_test.go`

**Interfaces:**
- Consumes: `outcome.New(path)`, `Ledger.Open/Resolve`, `Aggregate{Recalls, Resolved}` keyed by entry path (the `Entry` field of recall opens).

- [ ] **Step 1: Write the failing-or-proving test** — append:

```go
func TestKBSearchLedgerResolveColumn(t *testing.T) {
	dir := writeKBFixture(t)
	// Build a real ledger: the crashloop entry was recalled twice, resolved once.
	ledgerPath := filepath.Join(t.TempDir(), "outcomes.jsonl")
	l, err := outcome.New(ledgerPath)
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	rv := true
	for _, fp := range []string{"a", "b"} {
		if err := l.Open(outcome.Event{
			Fingerprint: fp, Kind: "recall", Entry: "incidents/crashloop-web.md",
			Resolvable: &rv, At: time.Now().Add(-time.Hour),
		}); err != nil {
			t.Fatalf("seed open: %v", err)
		}
	}
	if _, _, err := l.Resolve("a", time.Now()); err != nil {
		t.Fatalf("seed resolve: %v", err)
	}

	var out strings.Builder
	if err := runKBSearch([]string{"--dir", dir, "--ledger", ledgerPath, "crashloop", "web"}, &out); err != nil {
		t.Fatalf("runKBSearch: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "RESOLVE") {
		t.Errorf("RESOLVE header missing:\n%s", got)
	}
	if !strings.Contains(got, "1/2") {
		t.Errorf("resolve-rate 1/2 missing:\n%s", got)
	}
}

func TestKBSearchLedgerMissingWarnsAndOmits(t *testing.T) {
	dir := writeKBFixture(t)
	missing := filepath.Join(t.TempDir(), "nope.jsonl")
	var out strings.Builder
	if err := runKBSearch([]string{"--dir", dir, "--ledger", missing, "crashloop"}, &out); err != nil {
		t.Fatalf("a missing ledger must not fail the search: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "warning: ledger") {
		t.Errorf("missing warning line:\n%s", got)
	}
	// Stat-before-open: the warning path must not have created the file.
	if _, err := os.Stat(missing); err == nil {
		t.Error("--ledger must never create the ledger file")
	}
}
```

(Add `github.com/Smana/runlore/internal/outcome` to the test imports.)

- [ ] **Step 2: Run**

Run: `go test ./internal/app/ -race -run TestKBSearchLedger -v`
Expected: PASS if Task 1's plumbing is correct; otherwise fix `ledgerCounts`/`writeHitsTable` until green. (This task exists as its own reviewer gate: the ledger join is the only cross-package behavior in the CLI.)

- [ ] **Step 3: Commit**

```bash
git add internal/app/kb_cmd_test.go
git commit -m "test(cli): prove the kb search --ledger resolve-rate column end-to-end"
```

---

### Task 4: `--json` output

**Files:**
- Modify: `internal/app/kb_cmd.go` (replace the `writeHitsJSON` stub)
- Test: `internal/app/kb_cmd_test.go`

**Interfaces:**
- Produces: `writeHitsJSON(w, hits, counts) error` emitting a JSON array of `{path, type, title, description, resource, tags, score, last_seen, recalls, resolved}` (omitempty on optional fields).

- [ ] **Step 1: Write the failing test** — append:

```go
func TestKBSearchJSON(t *testing.T) {
	dir := writeKBFixture(t)
	var out strings.Builder
	if err := runKBSearch([]string{"--dir", dir, "--json", "crashloop", "web"}, &out); err != nil {
		t.Fatalf("runKBSearch --json: %v", err)
	}
	var hits []struct {
		Path     string  `json:"path"`
		Type     string  `json:"type"`
		Title    string  `json:"title"`
		Resource string  `json:"resource"`
		Score    float64 `json:"score"`
		LastSeen string  `json:"last_seen"`
	}
	if err := json.Unmarshal([]byte(out.String()), &hits); err != nil {
		t.Fatalf("output is not a JSON array: %v\n%s", err, out.String())
	}
	if len(hits) == 0 || hits[0].Path != "incidents/crashloop-web.md" {
		t.Fatalf("unexpected hits: %+v", hits)
	}
	if hits[0].Score <= 0 || hits[0].Type != "Incident" || hits[0].Resource != "apps/web" {
		t.Errorf("hit fields not mapped: %+v", hits[0])
	}
}
```

(Add `encoding/json` to the test imports.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/app/ -run TestKBSearchJSON -v`
Expected: FAIL — "not implemented yet".

- [ ] **Step 3: Implement** — replace the `writeHitsJSON` stub:

```go
// kbHit is the machine-readable search result (the CLI counterpart of the
// kb_search MCP tool's hit shape, plus the optional ledger track record).
type kbHit struct {
	Path        string   `json:"path"`
	Type        string   `json:"type,omitempty"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Resource    string   `json:"resource,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Score       float64  `json:"score"`
	LastSeen    string   `json:"last_seen,omitempty"` // frontmatter timestamp, RFC3339
	Recalls     int      `json:"recalls,omitempty"`
	Resolved    int      `json:"resolved,omitempty"`
}

func writeHitsJSON(w io.Writer, hits []catalog.ScoredEntry, counts map[string]outcome.Aggregate) error {
	out := make([]kbHit, 0, len(hits))
	for _, h := range hits {
		agg := counts[h.Entry.Path]
		out = append(out, kbHit{
			Path: h.Entry.Path, Type: h.Entry.Type, Title: h.Entry.Title,
			Description: h.Entry.Description, Resource: h.Entry.Resource, Tags: h.Entry.Tags,
			Score: h.Score, LastSeen: h.Entry.Timestamp,
			Recalls: agg.Recalls, Resolved: agg.Resolved,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/app/ -race -run TestKBSearch -v`
Expected: PASS (JSON + table + ledger tests).

- [ ] **Step 5: Commit**

```bash
git add internal/app/kb_cmd.go internal/app/kb_cmd_test.go
git commit -m "feat(cli): lore kb search --json for scripting"
```

---

### Task 5: wire into `main.go`, usage, docs, full verification

**Files:**
- Modify: `cmd/lore/main.go` (usage string ~line 21, dispatch switch ~line 41)
- Modify: `docs/reviewing-knowledge.md` (new section)
- Test: manual smoke (below) — `main.go` has no unit tests; dispatch parity with the other cases.

**Interfaces:**
- Consumes: `app.RunKB(args)` (Task 1).

- [ ] **Step 1: Wire the dispatch.** In `cmd/lore/main.go` add to the usage string (after the `lore catalog sync` line):

```
  lore kb search <query> [--dir <catalog>] [-k 10] [--json] [--ledger <jsonl>]   search the knowledge base
  lore kb show <entry> [--dir <catalog>]              print one KB entry (frontmatter card + body)
```

and to the switch (after `case "catalog":`):

```go
	case "kb":
		if err := app.RunKB(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "kb:", err)
			os.Exit(1)
		}
```

- [ ] **Step 2: Docs.** In `docs/reviewing-knowledge.md`, add a section (place it near the reviewing-workflow material):

```markdown
## Searching the KB from the CLI

The same BM25 index the agent uses for recall is available to humans:

    # against a local checkout of your KB repo
    lore kb search "crashloop web configmap" --dir ./my-kb

    # or via config (catalog.dir), after `lore catalog sync`
    lore kb search "oom worker"

    # one entry in full — path, filename, or a query with a unique hit
    lore kb show crashloop-web

`--json` emits the hits for scripting. `--ledger /var/lib/runlore/outcomes.jsonl`
adds a RESOLVE column (how often each entry's recalls preceded the incident
actually resolving) when you have a copy of the outcome ledger.

This is also the 30-second evaluation path: clone any OKF knowledge repo and
point `--dir` at it — no cluster, no model, no config.
```

- [ ] **Step 3: Smoke test end-to-end**

```bash
go build -o /tmp/lore ./cmd/lore
mkdir -p /tmp/kbdemo/incidents
printf -- '---\ntype: Incident\ntitle: demo entry\ndescription: d\nresource: apps/demo\n---\n## Cause\n\nx\n' > /tmp/kbdemo/incidents/demo.md
/tmp/lore kb search "demo" --dir /tmp/kbdemo
/tmp/lore kb show demo --dir /tmp/kbdemo
/tmp/lore kb search "no-match-zzz" --dir /tmp/kbdemo; echo "exit=$?"
```
Expected: a table with the demo entry; the full entry; `exit=1` with a clear message.

- [ ] **Step 4: Full verification**

Run: `go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test -race ./... && golangci-lint run ./...`
Expected: all green.

- [ ] **Step 5: Commit**

```bash
git add cmd/lore/main.go docs/reviewing-knowledge.md
git commit -m "feat(cli): wire lore kb into the entrypoint and document it"
```
