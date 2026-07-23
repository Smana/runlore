# D1 — Runbook/KB import Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Solve the cold-start problem: `lore kb import <dir>` converts a directory of existing markdown runbooks/postmortems into OKF-compatible catalog entries — adding/normalizing YAML frontmatter with **deterministic heuristics** (title from the first heading or filename, tags from existing frontmatter plus detected alert-name patterns, `Incident` vs `Playbook` from the body's OKF sections), validating each entry with the **same merge gate** as `lore validate-kb`, **deduplicating** against the existing catalog (exact/fuzzy title, destination-path collision), and writing the results into a **local KB checkout for the user to review and commit**. `--dry-run` previews; an **optional** `--model` flag refines frontmatter with the already-configured LLM. **No new required config** — `--into` falls back to the existing `catalog.dir`, `--model` reuses the existing `model:` block.

**Architecture:** One new pure package `internal/kbimport` (Infer → Plan → Enrich, all side-effect free), one new tiny package `internal/okf` that hosts the OKF entry serializer **extracted** from `internal/forge/github` (`renderEntry`/`slugify` at `internal/forge/github/github.go:509-568` become `okf.Render`/`okf.Slugify`; the forge delegates — this is also pre-work item M2/GitLab needs). The command reuses, not reinvents: `providers.KBEntry` (`internal/providers/providers.go:746`) is the entry struct; `kbvalidate.ValidateStructural` (`internal/kbvalidate/kbvalidate.go:105`) is the validator; `catalog.Load` (`internal/catalog/load.go:24`) reads the existing catalog for dedup; `curate`'s title-Jaccard (`internal/curate/dedup.go:113`) is the fuzzy-dup primitive; `curator.capTitle` (`internal/curator/draft.go:221`) is the title cap. Four tiny unexported helpers get exported first (Task 2). The CLI wiring follows the repo's actual pattern — **stdlib `flag` + a `switch` dispatcher, not cobra** (`cmd/lore/main.go:46`, `internal/app/kb_cmd.go:24` `RunKB`): a new `case "import"` under `lore kb`.

**Tech Stack:** Go 1.x (module `github.com/Smana/runlore`), stdlib `flag`/`text/tabwriter`, `gopkg.in/yaml.v3` (already a dep, used by `catalog` and `forge/github`), table-driven `testing` with `t.TempDir()` (house style, see `cmd/lore/validate_test.go`). No new dependencies.

---

### Task 1: Extract the OKF entry serializer into `internal/okf`

The forge's `renderEntry` (`internal/forge/github/github.go:509`), its `kbFrontmatter` struct (`github.go:414-439`), and `slugify` (`github.go:551`) are the only code in the repo that *writes* an OKF entry file. Import needs exactly that serializer, and the GitLab forge (roadmap M2) will too — extract it once. `okf.Render` takes an explicit `Meta` (timestamp/status/last_validated) instead of stamping `time.Now()` internally, and does **not** sanitize the body: the GitHub forge keeps calling `neutralizeImages` on LLM-authored bodies before rendering (that concern is forge-specific — an imported human runbook's `![](diagram.png)` must survive verbatim).

**Files:**
- Create: `internal/okf/okf.go`
- Test: `internal/okf/okf_test.go`
- Modify: `internal/forge/github/github.go`

**Steps:**

- [ ] Write the failing test `internal/okf/okf_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package okf

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestRenderFrontmatterAndBody(t *testing.T) {
	out := Render(providers.KBEntry{
		Type: "Playbook", Title: "Redis failover", Description: "how to fail over redis",
		Tags: []string{"imported", "playbook"}, Body: "# Redis failover\n\nsteps",
	}, Meta{Timestamp: "2024-03-01"})
	for _, want := range []string{
		"---\n", "type: Playbook\n", "title: Redis failover\n",
		"timestamp: \"2024-03-01\"", "tags:\n", "- imported\n",
		"# Redis failover\n\nsteps\n",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("Render missing %q:\n%s", want, out)
		}
	}
}

func TestRenderOmitsEmptyMeta(t *testing.T) {
	out := Render(providers.KBEntry{Type: "Playbook", Title: "T", Description: "d"}, Meta{})
	for _, absent := range []string{"timestamp:", "status:", "last_validated:", "fingerprint:", "resource:"} {
		if strings.Contains(out, absent) {
			t.Fatalf("empty %s must be omitted:\n%s", absent, out)
		}
	}
}

func TestRenderYAMLInjectionSafeTitle(t *testing.T) {
	// Marshaled (not string-formatted), so a colon-bearing title can't inject keys.
	out := Render(providers.KBEntry{Type: "Playbook", Title: "a: b\nresource: evil", Description: "d"}, Meta{})
	if strings.Contains(out, "\nresource: evil\n") {
		t.Fatalf("newline title must not inject a frontmatter key:\n%s", out)
	}
}

func TestSlugify(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Redis failover — March 2024!", "redis-failover-march-2024"},
		{"  KubePodCrashLooping  ", "kubepodcrashlooping"},
		{"---", ""},
	}
	for _, c := range cases {
		if got := Slugify(c.in); got != c.want {
			t.Errorf("Slugify(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
```

- [ ] Run it: `go test ./internal/okf/ -v` — expected FAIL (package does not exist / `Render` undefined).
- [ ] Create `internal/okf/okf.go` — the moved code, verbatim where possible:

```go
// SPDX-License-Identifier: Apache-2.0

// Package okf serializes providers.KBEntry values as OKF markdown files
// (YAML frontmatter + body). It is the single write-side counterpart of
// catalog.Load: the GitHub forge and `lore kb import` both render through it,
// so every entry RunLore writes parses back identically.
package okf

import (
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/providers"
)

// Meta carries the file-level frontmatter that is not part of a drafted
// KBEntry: the forge stamps Timestamp at render time; import preserves the
// source document's own timestamp/status/last_validated instead.
type Meta struct {
	Timestamp     string // OKF-recommended; RFC3339 or bare date
	Status        string // lifecycle: "", active, retired, draft
	LastValidated string // date a human last confirmed the entry works
}

// frontmatter is the YAML frontmatter of an OKF entry. Marshaled (not string-
// formatted) so a newline-bearing title/description from LLM output can't
// inject extra frontmatter keys. Keys mirror catalog.entryMeta (the loader).
type frontmatter struct {
	Type          string   `yaml:"type"`
	Title         string   `yaml:"title"`
	Description   string   `yaml:"description"`
	Resource      string   `yaml:"resource,omitempty"`
	AlertResource string   `yaml:"alert_resource,omitempty"`
	Tags          []string `yaml:"tags,omitempty"`
	Timestamp     string   `yaml:"timestamp,omitempty"`
	Status        string   `yaml:"status,omitempty"`
	LastValidated string   `yaml:"last_validated,omitempty"`
	Fingerprint   string   `yaml:"fingerprint,omitempty"`
	Confidence    float64  `yaml:"confidence,omitempty"`
	Provenance    []string `yaml:"provenance,omitempty"`
}

// Render serializes a KBEntry as OKF markdown. The body is written verbatim —
// callers that render untrusted (LLM-authored) bodies sanitize BEFORE calling
// (the GitHub forge neutralizes image markdown); a human runbook being
// imported must survive byte-for-byte.
func Render(e providers.KBEntry, m Meta) string {
	fm, _ := yaml.Marshal(frontmatter{
		Type: e.Type, Title: e.Title, Description: e.Description, Resource: e.Resource,
		AlertResource: e.AlertResource, Tags: e.Tags,
		Timestamp: m.Timestamp, Status: m.Status, LastValidated: m.LastValidated,
		Fingerprint: e.Fingerprint, Confidence: e.Confidence, Provenance: e.Provenance,
	})
	var b strings.Builder
	b.WriteString("---\n")
	b.Write(fm)
	b.WriteString("---\n\n")
	b.WriteString(e.Body)
	b.WriteString("\n")
	return b.String()
}

// Slugify lowercases s and collapses every non-[a-z0-9] run into one dash.
// (Moved verbatim from internal/forge/github.)
func Slugify(s string) string {
	s = strings.ToLower(s)
	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		default:
			if !prevDash {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.Trim(b.String(), "-")
}
```

- [ ] Run `go test ./internal/okf/ -v` — expected PASS. (Note: `timestamp: "2024-03-01"` — yaml.v3 quotes date-like strings; if the marshaler emits it unquoted, relax that one assertion to `timestamp: `.)
- [ ] Delegate the forge: in `internal/forge/github/github.go`, delete the `kbFrontmatter` struct (lines 411-439) and the bodies of `renderEntry`/`slugify`, keeping thin wrappers so all existing tests (`github_test.go:361-430`, `alert_resource_roundtrip_test.go`) pass unchanged:

```go
// renderEntry serializes a KBEntry as OKF markdown (frontmatter + body).
// The timestamp is stamped at render time (RFC3339 UTC, matching the seed
// entries); last_validated stays unset — that field claims human confirmation.
// The body is neutralized here (LLM/alert-authored, GitHub auto-renders it);
// okf.Render itself writes bodies verbatim.
func renderEntry(e providers.KBEntry) string {
	e.Body = neutralizeImages(e.Body)
	return okf.Render(e, okf.Meta{Timestamp: time.Now().UTC().Format(time.RFC3339)})
}

func slugify(s string) string { return okf.Slugify(s) }
```

  Add the `github.com/Smana/runlore/internal/okf` import; remove `gopkg.in/yaml.v3` from the import block if it is now unused in that file.
- [ ] Run `go build ./... && go test ./internal/forge/github/ ./internal/okf/ -count=1` — expected PASS (the round-trip test `alert_resource_roundtrip_test.go` is the safety net for the extraction).
- [ ] Commit: `refactor(okf): extract OKF entry serializer from the github forge`

### Task 2: Export the four shared primitives import reuses

Four unexported helpers become the import command's building blocks. Pure renames + one wrapper; the failing "test" for each is a new test file that calls the exported name (compile failure = RED).

**Files:**
- Modify: `internal/catalog/load.go` (+ call sites in package), `internal/kbvalidate/kbvalidate.go` (+ call sites), `internal/curator/draft.go` (+ call sites incl. `draft_test.go`), `internal/curate/dedup.go`
- Test: `internal/catalog/load_test.go`, `internal/kbvalidate/kbvalidate_test.go`, `internal/curator/draft_test.go`, `internal/curate/dedup_test.go` (append to existing files)

**Steps:**

- [ ] Append the pinning tests (each fails to compile until the rename lands):

```go
// internal/catalog/load_test.go — append
func TestSplitFrontmatterExported(t *testing.T) {
	fm, body := SplitFrontmatter([]byte("---\ntitle: t\n---\nbody\n"))
	if string(fm) != "title: t" || string(body) != "body\n" {
		t.Fatalf("got fm=%q body=%q", fm, body)
	}
	fm, body = SplitFrontmatter([]byte("no frontmatter"))
	if fm != nil || string(body) != "no frontmatter" {
		t.Fatalf("frontmatterless input must pass through, got fm=%q body=%q", fm, body)
	}
}
```

```go
// internal/kbvalidate/kbvalidate_test.go — append
func TestSectionsExported(t *testing.T) {
	secs := Sections("## Symptom\n\nx\n\n## Cause\n\ny\n")
	if secs["symptom"] != "x" || secs["cause"] != "y" {
		t.Fatalf("got %#v", secs)
	}
}
```

```go
// internal/curator/draft_test.go — append
func TestCapTitleExported(t *testing.T) {
	if got := CapTitle("a\nb\tc"); got != "a b c" {
		t.Fatalf("whitespace collapse: got %q", got)
	}
	long := strings.Repeat("word ", 40)
	if capped := CapTitle(long); len(capped) > 120 {
		t.Fatalf("must cap at 120 bytes, got %d", len(capped))
	}
}
```

```go
// internal/curate/dedup_test.go — append
func TestTitleJaccardExported(t *testing.T) {
	if s := TitleJaccard("redis failover runbook", "redis failover runbook"); s != 1 {
		t.Fatalf("identical titles must score 1, got %v", s)
	}
	if s := TitleJaccard("redis failover", "postgres vacuum tuning"); s >= 0.6 {
		t.Fatalf("unrelated titles must stay under threshold, got %v", s)
	}
}
```

- [ ] Run `go test ./internal/catalog/ ./internal/kbvalidate/ ./internal/curator/ ./internal/curate/ -run 'Exported' -v` — expected FAIL (undefined: `SplitFrontmatter`, `Sections`, `CapTitle`, `TitleJaccard`).
- [ ] Rename `splitFrontmatter` → `SplitFrontmatter` in `internal/catalog/load.go:99` (update the call at `load.go:80`); keep its doc comment, note it is now shared with `kbimport`.
- [ ] Rename `sections` → `Sections` in `internal/kbvalidate/kbvalidate.go:191` (update the call at `kbvalidate.go:172` and any test references).
- [ ] Rename `capTitle` → `CapTitle` in `internal/curator/draft.go:221` (update the call at `draft.go:100` and every `capTitle` reference in `internal/curator/*_test.go`).
- [ ] Add to `internal/curate/dedup.go`:

```go
// TitleJaccard scores two entry titles by Jaccard similarity over their
// noise-filtered token sets — the same fuzzy-duplicate primitive Dedup uses
// for markerless PRs, shared with `lore kb import` so "duplicate" means the
// same thing at import time and at curation time. ≥0.6 is the house threshold.
func TitleJaccard(a, b string) float64 {
	return jaccard(titleTokens(a), titleTokens(b))
}
```

- [ ] Run `go build ./... && go test ./internal/catalog/ ./internal/kbvalidate/ ./internal/curator/ ./internal/curate/ -count=1` — expected PASS.
- [ ] Commit: `refactor: export SplitFrontmatter, Sections, CapTitle, TitleJaccard for kb import`

### Task 3: `internal/kbimport` — deterministic frontmatter inference

The pure core. `Infer` turns one source document into a `Result`: a `providers.KBEntry` + preserved `okf.Meta` + a computed destination path + warnings. Heuristics (all deterministic, no I/O, no clock):

- **type** — a source frontmatter `type` already in the validator vocabulary (`{Incident, Playbook, Concept}`, `internal/kbvalidate/kbvalidate.go:42`) passes through; otherwise `Incident` iff the body already carries non-empty `Symptom`+`Cause`+`Resolution` sections (via `kbvalidate.Sections`) **and** a resource was found (Incident requires one, `kbvalidate.go:135-137`); else `Playbook` (validation is intentionally relaxed for free-form runbooks, `kbvalidate.go:170`). `Postmortem` etc. therefore map to Incident when well-formed — never emitted raw (it would fail the gate, same reasoning as `internal/curator/draft.go:176-179`).
- **title** — frontmatter `title` → first `# `/`## ` heading → humanized filename stem; capped by `curator.CapTitle` (single line, ≤120 bytes — the gate at `kbvalidate.go:122`).
- **description** — frontmatter `description` → first non-blank, non-heading, non-code-fence body line (leading list markers stripped), whitespace-collapsed, capped at 240 runes → falls back to the title (the gate requires non-empty, `kbvalidate.go:126`).
- **tags** — `imported` + lowercased type (mirrors `curator.entryTags`, `draft.go:195`) + the source's own tags (lowercased, deduped) + detected **alert patterns**: CamelCase tokens with ≥2 humps (`KubePodCrashLooping`, `TargetDown`) found in headings or in lines mentioning "alert" — tags feed the BM25 corpus, so each detected alert name is exactly the recall signal that lets a future alert find the imported runbook. Capped at 10 total.
- **resource** — frontmatter `resource` passthrough only, and only when whitespace-free (the gate, `kbvalidate.go:139`); never guessed.
- **timestamp** — frontmatter `timestamp` or `date`, kept **raw** when `catalog.ParseEntryDate` (`internal/catalog/entry.go:13`) accepts it, dropped with a warning otherwise; `status`/`last_validated` pass through raw (the validator treats odd values as advisory warnings, `kbvalidate.go:151-162`).
- **dest path** — `<type-lowercase>s/<slug>.md` (same type-directory convention as the forge's `entryPath`, `github.go:540-548`) with **no** fingerprint suffix: imported entries carry no fingerprint (hand-written, per `internal/catalog/entry.go:39`), and a stable path makes re-running import idempotent (second run dedups on "destination exists").
- **body** — verbatim. Existing frontmatter is *replaced* by the normalized set (its recognized keys were absorbed; unrecognized keys are reported as a warning, not silently dropped).

**Files:**
- Create: `internal/kbimport/infer.go`
- Test: `internal/kbimport/infer_test.go`

**Steps:**

- [ ] Write the failing table-driven test `internal/kbimport/infer_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package kbimport

import (
	"reflect"
	"strings"
	"testing"
)

func TestInfer(t *testing.T) {
	cases := []struct {
		name, source, data                    string
		wantType, wantTitle, wantDesc, wantRes string
		wantTags                              []string
		wantDest, wantTimestamp               string
	}{
		{
			name:   "bare runbook: title from H1, description from first paragraph, Playbook",
			source: "runbooks/redis-failover.md",
			data:   "# Redis failover\n\nHow to fail over the redis primary.\n\n## Steps\n\n- step one\n",
			wantType: "Playbook", wantTitle: "Redis failover",
			wantDesc: "How to fail over the redis primary.",
			wantTags: []string{"imported", "playbook"},
			wantDest: "playbooks/redis-failover.md",
		},
		{
			name:   "no heading: title humanized from filename",
			source: "notes/pg_vacuum-tuning.md",
			data:   "Tune autovacuum before it falls behind.\n",
			wantType: "Playbook", wantTitle: "pg vacuum tuning",
			wantDesc: "Tune autovacuum before it falls behind.",
			wantTags: []string{"imported", "playbook"},
			wantDest: "playbooks/pg-vacuum-tuning.md",
		},
		{
			name:   "postmortem with OKF sections + resource becomes Incident, date preserved",
			source: "postmortems/2024-03-payments.md",
			data: "---\ntitle: Payments API outage\ndate: 2024-03-14\ntags: [payments]\nresource: payments/api\ntype: postmortem\n---\n" +
				"## Symptom\n\n5xx spike\n\n## Cause\n\nbad deploy\n\n## Resolution\n\nrollback\n",
			wantType: "Incident", wantTitle: "Payments API outage",
			wantDesc: "5xx spike", wantRes: "payments/api",
			wantTags:      []string{"imported", "incident", "payments"},
			wantDest:      "incidents/payments-api-outage.md",
			wantTimestamp: "2024-03-14",
		},
		{
			name:   "alert names in headings/alert lines become tags",
			source: "runbooks/oom.md",
			data:   "# KubeContainerOOMKilled\n\nAlert: fires alongside KubePodCrashLooping sometimes.\n",
			wantType: "Playbook", wantTitle: "KubeContainerOOMKilled",
			wantDesc: "Alert: fires alongside KubePodCrashLooping sometimes.",
			wantTags: []string{"imported", "playbook", "kubecontaineroomkilled", "kubepodcrashlooping"},
			wantDest: "playbooks/kubecontaineroomkilled.md",
		},
		{
			name:   "valid existing type passes through untouched",
			source: "concepts/slo.md",
			data:   "---\ntype: Concept\ntitle: SLO policy\ndescription: how we set SLOs\n---\nbody\n",
			wantType: "Concept", wantTitle: "SLO policy", wantDesc: "how we set SLOs",
			wantTags: []string{"imported", "concept"},
			wantDest: "concepts/slo-policy.md",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Infer([]byte(c.data), c.source)
			if r.Entry.Type != c.wantType {
				t.Errorf("type = %q, want %q", r.Entry.Type, c.wantType)
			}
			if r.Entry.Title != c.wantTitle {
				t.Errorf("title = %q, want %q", r.Entry.Title, c.wantTitle)
			}
			if r.Entry.Description != c.wantDesc {
				t.Errorf("description = %q, want %q", r.Entry.Description, c.wantDesc)
			}
			if r.Entry.Resource != c.wantRes {
				t.Errorf("resource = %q, want %q", r.Entry.Resource, c.wantRes)
			}
			if !reflect.DeepEqual(r.Entry.Tags, c.wantTags) {
				t.Errorf("tags = %v, want %v", r.Entry.Tags, c.wantTags)
			}
			if r.DestPath != c.wantDest {
				t.Errorf("dest = %q, want %q", r.DestPath, c.wantDest)
			}
			if r.Meta.Timestamp != c.wantTimestamp {
				t.Errorf("timestamp = %q, want %q", r.Meta.Timestamp, c.wantTimestamp)
			}
			if r.Source != c.source {
				t.Errorf("source = %q, want %q", r.Source, c.source)
			}
		})
	}
}

func TestInferLongTitleCapped(t *testing.T) {
	r := Infer([]byte("# "+strings.Repeat("verylongword ", 30)+"\n\nbody\n"), "x.md")
	if len(r.Entry.Title) > 120 {
		t.Fatalf("title must satisfy the 120-byte merge gate, got %d bytes", len(r.Entry.Title))
	}
}

func TestInferUnparseableDateDropsWithWarning(t *testing.T) {
	r := Infer([]byte("---\ntitle: t\ndate: last tuesday\n---\nbody\n"), "x.md")
	if r.Meta.Timestamp != "" {
		t.Fatalf("unparseable date must be dropped, got %q", r.Meta.Timestamp)
	}
	if len(r.Warnings) == 0 {
		t.Fatal("dropping the date must warn")
	}
}
```

- [ ] Run `go test ./internal/kbimport/ -run TestInfer -v` — expected FAIL (package does not exist).
- [ ] Create `internal/kbimport/infer.go`:

```go
// SPDX-License-Identifier: Apache-2.0

// Package kbimport converts existing markdown runbooks/postmortems into
// OKF-compatible catalog entries — the cold-start answer: deterministic
// frontmatter inference (Infer), dedup against the live catalog (Plan), and
// an optional LLM refinement (Enrich). It is pure (no I/O, no clock); the
// command layer in internal/app does the walking, validating, and writing.
package kbimport

import (
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/kbvalidate"
	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

// Result is one source document converted to an importable entry.
type Result struct {
	Entry    providers.KBEntry // reuses the curator/forge entry struct — no parallel format
	Meta     okf.Meta          // preserved source timestamp/status/last_validated
	DestPath string            // bundle-relative destination, e.g. playbooks/redis-failover.md
	Source   string            // source path, for reporting
	Warnings []string
}

// sourceMeta is the tolerant superset of frontmatter keys recognized in a
// source document. Unknown keys are collected via the raw map for a warning.
type sourceMeta struct {
	Type          string   `yaml:"type"`
	Title         string   `yaml:"title"`
	Description   string   `yaml:"description"`
	Resource      string   `yaml:"resource"`
	Tags          []string `yaml:"tags"`
	Timestamp     string   `yaml:"timestamp"`
	Date          string   `yaml:"date"` // common in postmortems; folded into timestamp
	Status        string   `yaml:"status"`
	LastValidated string   `yaml:"last_validated"`
}

var validTypes = map[string]bool{"Incident": true, "Playbook": true, "Concept": true}

// Infer derives normalized OKF frontmatter for one markdown document, purely
// from its existing frontmatter, headings, and content. Deterministic by
// construction — same input, same entry — so imports are reviewable diffs,
// not model output. It never fabricates: resource is passthrough-only, and a
// document is an Incident only when it already carries the OKF evidence
// sections the merge gate demands.
func Infer(data []byte, source string) Result {
	r := Result{Source: source}
	fm, body := catalog.SplitFrontmatter(data)
	var meta sourceMeta
	if len(fm) > 0 {
		if err := yaml.Unmarshal(fm, &meta); err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("unparseable frontmatter ignored: %v", err))
		} else if extra := unknownKeys(fm); len(extra) > 0 {
			r.Warnings = append(r.Warnings, "frontmatter keys not carried over: "+strings.Join(extra, ", "))
		}
	}
	b := string(body)

	title := inferTitle(meta.Title, b, source)
	resource := strings.TrimSpace(meta.Resource)
	if strings.ContainsAny(resource, " \t\r\n") {
		r.Warnings = append(r.Warnings, fmt.Sprintf("resource %q dropped: must be whitespace-free (namespace/name)", resource))
		resource = ""
	}
	typ := inferType(meta.Type, b, resource)

	r.Entry = providers.KBEntry{
		Type:        typ,
		Title:       title,
		Description: inferDescription(meta.Description, b, title),
		Resource:    resource,
		Tags:        inferTags(meta.Tags, b, typ),
		Body:        b,
	}
	r.Meta = okf.Meta{Status: meta.Status, LastValidated: meta.LastValidated}
	if ts := firstNonEmpty(meta.Timestamp, meta.Date); ts != "" {
		if _, ok := catalog.ParseEntryDate(ts); ok {
			r.Meta.Timestamp = ts
		} else {
			r.Warnings = append(r.Warnings, fmt.Sprintf("unparseable date %q dropped (want RFC3339 or 2006-01-02)", ts))
		}
	}
	r.DestPath = fmt.Sprintf("%ss/%s.md", strings.ToLower(typ), okf.Slugify(title))
	return r
}

// inferType: a valid declared type wins; else Incident iff the body already
// carries the gate's required sections (kbvalidate.requiredIncidentSections)
// AND a resource (Incident requires one); else Playbook — the relaxed,
// free-form-runbook type.
func inferType(declared, body, resource string) string {
	if validTypes[strings.TrimSpace(declared)] {
		return strings.TrimSpace(declared)
	}
	secs := kbvalidate.Sections(body)
	if resource != "" && secs["symptom"] != "" && secs["cause"] != "" && secs["resolution"] != "" {
		return "Incident"
	}
	return "Playbook"
}

// inferTitle: frontmatter title → first #/## heading → humanized filename
// stem. Always capped to the merge gate's single-line 120-byte budget.
func inferTitle(declared, body, source string) string {
	if t := curator.CapTitle(declared); t != "" {
		return t
	}
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		for _, p := range []string{"# ", "## "} {
			if strings.HasPrefix(t, p) {
				if h := curator.CapTitle(strings.TrimPrefix(t, p)); h != "" {
					return h
				}
			}
		}
	}
	stem := strings.TrimSuffix(filepath.Base(source), ".md")
	return curator.CapTitle(strings.NewReplacer("-", " ", "_", " ").Replace(stem))
}

// descriptionMaxRunes bounds the inferred description; it only has to be
// non-empty (the gate) and scannable in `lore kb search` output.
const descriptionMaxRunes = 240

// inferDescription: frontmatter description → first prose line of the body
// (headings, code fences, and blank lines skipped; list/quote markers
// stripped) → the title, so the gate's non-empty requirement always holds.
func inferDescription(declared, body, title string) string {
	if d := strings.TrimSpace(declared); d != "" {
		return capRunes(strings.Join(strings.Fields(d), " "), descriptionMaxRunes)
	}
	inFence := false
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "```") {
			inFence = !inFence
			continue
		}
		if inFence || t == "" || strings.HasPrefix(t, "#") || t == "---" {
			continue
		}
		t = strings.TrimSpace(strings.TrimLeft(t, "-*> "))
		if t != "" {
			return capRunes(strings.Join(strings.Fields(t), " "), descriptionMaxRunes)
		}
	}
	return title
}

// maxTags bounds the tag list; tags feed the BM25 corpus, and past ~10 the
// extra ones are noise, not recall signal.
const maxTags = 10

// alertNameRe matches CamelCase tokens with ≥2 humps — the Prometheus
// alert-name shape (KubePodCrashLooping, TargetDown). Only headings and
// alert-mentioning lines are scanned, which keeps ordinary CamelCase prose
// (product names) from flooding the tags.
var alertNameRe = regexp.MustCompile(`\b[A-Z][a-z0-9]+(?:[A-Z][a-z0-9]+)+\b`)

// inferTags mirrors curator.entryTags' shape (constant pair first, derived
// signal after, deduped, lowercased): imported + type, then the source's own
// tags, then detected alert-name patterns.
func inferTags(declared []string, body, typ string) []string {
	tags := []string{"imported", strings.ToLower(typ)}
	seen := map[string]bool{tags[0]: true, tags[1]: true}
	add := func(t string) {
		t = strings.ToLower(strings.TrimSpace(t))
		if t != "" && !seen[t] && len(tags) < maxTags {
			seen[t] = true
			tags = append(tags, t)
		}
	}
	for _, t := range declared {
		add(t)
	}
	for _, line := range strings.Split(body, "\n") {
		t := strings.TrimSpace(line)
		if !strings.HasPrefix(t, "#") && !strings.Contains(strings.ToLower(t), "alert") {
			continue
		}
		for _, m := range alertNameRe.FindAllString(t, -1) {
			add(m)
		}
	}
	return tags
}

// unknownKeys lists top-level frontmatter keys Infer does not carry over,
// so the import report tells the user what was left behind.
func unknownKeys(fm []byte) []string {
	known := map[string]bool{
		"type": true, "title": true, "description": true, "resource": true,
		"tags": true, "timestamp": true, "date": true, "status": true, "last_validated": true,
	}
	var raw map[string]any
	if yaml.Unmarshal(fm, &raw) != nil {
		return nil
	}
	var out []string
	for k := range raw {
		if !known[k] {
			out = append(out, k)
		}
	}
	sortStrings(out)
	return out
}

func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}

func capRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return strings.TrimRight(string(r[:n]), " ") + "…"
}

func firstNonEmpty(a, b string) string {
	if strings.TrimSpace(a) != "" {
		return strings.TrimSpace(a)
	}
	return strings.TrimSpace(b)
}
```

  (If `sortStrings` trips the linter, use `sort.Strings` from stdlib instead — either is fine; prefer `sort.Strings`.)
- [ ] Run `go test ./internal/kbimport/ -run TestInfer -v` — expected PASS. Check the import cycle is clean: `kbimport → {catalog, curator, kbvalidate, okf, providers}`; none of those import `kbimport` (verify with `go build ./...`).
- [ ] Commit: `feat(kbimport): deterministic OKF frontmatter inference for existing runbooks`

### Task 4: `internal/kbimport` — dedup plan against the existing catalog

`Plan` decides, per inferred result and **in input order**, import vs skip. Skip reasons, checked in order:

1. `source marked retired` — the source's own `status: retired` is respected (importing it as-is would add dead weight; the warning tells the user why).
2. `destination exists: <path>` — an existing catalog entry already occupies `DestPath` (also what makes a re-run of the same import a clean no-op).
3. `duplicate of <path>` — exact normalized-title match or `curate.TitleJaccard ≥ 0.6` (the same threshold `curate.Dedup` uses, `internal/curate/dedup.go:20`) against any existing entry title.
4. `destination collides with <source> in this batch` — two sources slugging to the same path; first wins.

**Files:**
- Create: `internal/kbimport/plan.go`
- Test: `internal/kbimport/plan_test.go`

**Steps:**

- [ ] Write the failing test `internal/kbimport/plan_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package kbimport

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

func res(title, dest, source string) Result {
	return Result{
		Entry:    providers.KBEntry{Type: "Playbook", Title: title, Description: title, Body: "body"},
		DestPath: dest, Source: source,
	}
}

func TestPlanDedup(t *testing.T) {
	existing := []catalog.Entry{
		{Path: "playbooks/redis-failover.md", Title: "Redis failover"},
		{Path: "incidents/payments-outage.md", Title: "Payments API outage March 2024"},
	}
	retired := res("Old thing", "playbooks/old-thing.md", "old.md")
	retired.Meta = okf.Meta{Status: "retired"}
	in := []Result{
		res("Postgres vacuum tuning", "playbooks/postgres-vacuum-tuning.md", "a.md"), // novel → import
		res("Anything", "playbooks/redis-failover.md", "b.md"),                        // path taken → skip
		res("Payments API outage — March 2024", "playbooks/payments-api-outage-march-2024.md", "c.md"), // fuzzy title dup → skip
		retired, // retired at source → skip
		res("Postgres vacuum tuning", "playbooks/postgres-vacuum-tuning.md", "e.md"), // batch collision → skip
	}
	got := Plan(in, existing)
	if len(got) != len(in) {
		t.Fatalf("Plan must return one action per result, got %d", len(got))
	}
	wantSkip := []struct {
		skip   bool
		reason string
	}{
		{false, ""},
		{true, "destination exists"},
		{true, "duplicate of incidents/payments-outage.md"},
		{true, "retired"},
		{true, "collides"},
	}
	for i, w := range wantSkip {
		if got[i].Skip != w.skip {
			t.Errorf("action %d (%s): skip = %v, want %v (reason %q)", i, in[i].Source, got[i].Skip, w.skip, got[i].Reason)
		}
		if w.reason != "" && !strings.Contains(got[i].Reason, w.reason) {
			t.Errorf("action %d: reason %q must mention %q", i, got[i].Reason, w.reason)
		}
	}
}
```

- [ ] Run `go test ./internal/kbimport/ -run TestPlanDedup -v` — expected FAIL (`Plan` undefined).
- [ ] Create `internal/kbimport/plan.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package kbimport

import (
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/curate"
)

// Action is Plan's verdict on one inferred result.
type Action struct {
	Result
	Skip   bool
	Reason string // set when Skip
}

// dupThreshold matches curate.Dedup's default title-Jaccard threshold: a
// duplicate at import time is the same notion as a duplicate at curation
// time. Conservative both ways — a missed dup lands in review as a visible
// extra file; a wrong skip loses knowledge silently, so we require ≥0.6.
const dupThreshold = 0.6

// Plan dedups inferred results against the existing catalog and against each
// other, preserving input order. It only decides — the caller reports/writes.
func Plan(results []Result, existing []catalog.Entry) []Action {
	takenPath := map[string]string{} // dest path -> occupant (existing entry path or batch source)
	for _, e := range existing {
		takenPath[e.Path] = e.Path
	}
	out := make([]Action, 0, len(results))
	for _, r := range results {
		a := Action{Result: r}
		switch {
		case strings.EqualFold(strings.TrimSpace(r.Meta.Status), "retired"):
			a.Skip, a.Reason = true, "source marked retired"
		case existingPathTaken(takenPath, r.DestPath, existing):
			a.Skip, a.Reason = true, fmt.Sprintf("destination exists: %s", r.DestPath)
		default:
			if dup, ok := duplicateOf(r.Entry.Title, existing); ok {
				a.Skip, a.Reason = true, "duplicate of "+dup
			} else if occ, taken := takenPath[r.DestPath]; taken {
				a.Skip, a.Reason = true, fmt.Sprintf("destination %s collides with %s in this batch", r.DestPath, occ)
			} else {
				takenPath[r.DestPath] = r.Source
			}
		}
		out = append(out, a)
	}
	return out
}

// existingPathTaken reports whether dest is already an EXISTING catalog
// entry's path (batch collisions are reported separately, with a clearer
// message naming the other source).
func existingPathTaken(taken map[string]string, dest string, existing []catalog.Entry) bool {
	occ, ok := taken[dest]
	return ok && occ == dest && isExisting(dest, existing)
}

func isExisting(path string, existing []catalog.Entry) bool {
	for _, e := range existing {
		if e.Path == path {
			return true
		}
	}
	return false
}

// duplicateOf finds an existing entry whose title matches: exact after
// whitespace/case normalization, or fuzzy at curate's own Jaccard threshold.
func duplicateOf(title string, existing []catalog.Entry) (string, bool) {
	norm := strings.ToLower(strings.Join(strings.Fields(title), " "))
	for _, e := range existing {
		if strings.ToLower(strings.Join(strings.Fields(e.Title), " ")) == norm {
			return e.Path, true
		}
		if curate.TitleJaccard(title, e.Title) >= dupThreshold {
			return e.Path, true
		}
	}
	return "", false
}
```

- [ ] Run `go test ./internal/kbimport/ -run TestPlanDedup -v` — expected PASS. (Simplification note for the implementer: if `existingPathTaken`'s double bookkeeping reads awkwardly, an equally correct shape is two maps — `existingPaths map[string]bool` and `batchPaths map[string]string` — checked in that order. Keep whichever version the test passes with and reads cleaner.)
- [ ] Commit: `feat(kbimport): dedup import plan against the existing catalog`

### Task 5: `internal/kbimport` — optional `--model` frontmatter enrichment

`Enrich` mirrors `kbvalidate.ReviewSemantic` (`internal/kbvalidate/semantic.go:67`) exactly in spirit: a single forced tool call (`ToolChoice`, `providers.CompletionRequest` at `internal/providers/providers.go:782-793`), and it **never gates** — nil model, model error, missing tool call, or junk output all return the deterministic result untouched (plus a warning). The model may refine `title`, `description`, `tags`, and `type`; it may **not** invent `resource` (passthrough-only stays the rule) and the returned title is re-capped with `curator.CapTitle`. `DestPath` is recomputed from the (possibly new) type+title.

**Files:**
- Create: `internal/kbimport/enrich.go`
- Test: `internal/kbimport/enrich_test.go`

**Steps:**

- [ ] Write the failing test `internal/kbimport/enrich_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package kbimport

import (
	"context"
	"errors"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeModel returns a canned response (or error) for the single Complete call.
type fakeModel struct {
	resp providers.CompletionResponse
	err  error
}

func (f fakeModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	return f.resp, f.err
}

func TestEnrichAppliesModelFields(t *testing.T) {
	r := res("pg vacuum tuning", "playbooks/pg-vacuum-tuning.md", "a.md")
	m := fakeModel{resp: providers.CompletionResponse{ToolCalls: []providers.ToolCall{{
		Name: "submit_frontmatter",
		Args: `{"title":"Postgres autovacuum tuning","description":"tune autovacuum thresholds before bloat","tags":["postgres","autovacuum"],"type":"Playbook"}`,
	}}}}
	got := Enrich(context.Background(), r, m)
	if got.Entry.Title != "Postgres autovacuum tuning" {
		t.Fatalf("title = %q", got.Entry.Title)
	}
	if got.Entry.Description != "tune autovacuum thresholds before bloat" {
		t.Fatalf("description = %q", got.Entry.Description)
	}
	if got.DestPath != "playbooks/postgres-autovacuum-tuning.md" {
		t.Fatalf("DestPath must follow the new title, got %q", got.DestPath)
	}
	want := map[string]bool{"imported": true, "playbook": true, "postgres": true, "autovacuum": true}
	for _, tag := range got.Entry.Tags {
		if !want[tag] {
			t.Fatalf("unexpected tag %q in %v", tag, got.Entry.Tags)
		}
	}
}

func TestEnrichNeverGates(t *testing.T) {
	r := res("pg vacuum tuning", "playbooks/pg-vacuum-tuning.md", "a.md")
	for name, m := range map[string]providers.ModelProvider{
		"nil model":    nil,
		"model error":  fakeModel{err: errors.New("boom")},
		"no tool call": fakeModel{resp: providers.CompletionResponse{Text: "prose"}},
		"bad json":     fakeModel{resp: providers.CompletionResponse{ToolCalls: []providers.ToolCall{{Name: "submit_frontmatter", Args: "{"}}}},
		"invalid type": fakeModel{resp: providers.CompletionResponse{ToolCalls: []providers.ToolCall{{Name: "submit_frontmatter", Args: `{"type":"Postmortem"}`}}}},
	} {
		got := Enrich(context.Background(), r, m)
		if got.Entry.Title != r.Entry.Title || got.Entry.Type != r.Entry.Type || got.DestPath != r.DestPath {
			t.Fatalf("%s: enrichment must fall back to the deterministic result, got %+v", name, got.Entry)
		}
	}
}

func TestEnrichNeverInventsResource(t *testing.T) {
	r := res("t", "playbooks/t.md", "a.md")
	m := fakeModel{resp: providers.CompletionResponse{ToolCalls: []providers.ToolCall{{
		Name: "submit_frontmatter", Args: `{"resource":"prod/db"}`,
	}}}}
	if got := Enrich(context.Background(), r, m); got.Entry.Resource != "" {
		t.Fatalf("model must not set resource, got %q", got.Entry.Resource)
	}
}
```

- [ ] Run `go test ./internal/kbimport/ -run TestEnrich -v` — expected FAIL (`Enrich` undefined).
- [ ] Create `internal/kbimport/enrich.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package kbimport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/providers"
)

const submitFrontmatterName = "submit_frontmatter"

func submitFrontmatterSpec() providers.ToolSpec {
	return providers.ToolSpec{
		Name:        submitFrontmatterName,
		Description: "Submit refined OKF frontmatter for the runbook being imported.",
		Schema: `{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "concise, specific, one line"},
    "description": {"type": "string", "description": "one-sentence summary of when this entry applies"},
    "tags": {"type": "array", "items": {"type": "string"}, "description": "lowercase recall keywords incl. alert names"},
    "type": {"type": "string", "enum": ["Incident", "Playbook", "Concept"]}
  }
}`,
	}
}

const enrichSystemPrompt = `You refine YAML frontmatter for an SRE runbook being imported into a knowledge
catalog. You are given the deterministic draft frontmatter and the document body.
Improve title/description/tags only where the draft is weak; keep them faithful
to the document — never invent facts. Answer ONLY via submit_frontmatter.`

// Enrich optionally refines a Result's frontmatter with the configured model.
// It NEVER gates (mirrors kbvalidate.ReviewSemantic): nil model, a model
// error, a missing tool call, or unusable output all return r unchanged apart
// from a warning — the deterministic Infer output is always a valid fallback.
// The model cannot set resource (passthrough-only), the title is re-capped to
// the merge gate's budget, and DestPath follows the refined type+title.
func Enrich(ctx context.Context, r Result, m providers.ModelProvider) Result {
	if m == nil {
		return r
	}
	prompt := fmt.Sprintf("Draft frontmatter:\ntype: %s\ntitle: %s\ndescription: %s\ntags: %s\n\nDocument body:\n\n%s",
		r.Entry.Type, r.Entry.Title, r.Entry.Description, strings.Join(r.Entry.Tags, ", "), r.Entry.Body)
	resp, err := m.Complete(ctx, providers.CompletionRequest{
		System:     enrichSystemPrompt,
		Messages:   []providers.Message{{Role: "user", Content: prompt}},
		Tools:      []providers.ToolSpec{submitFrontmatterSpec()},
		ToolChoice: submitFrontmatterName,
	})
	if err != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("model enrichment skipped: %v", err))
		return r
	}
	for _, tc := range resp.ToolCalls {
		if tc.Name != submitFrontmatterName {
			continue
		}
		var raw struct {
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Tags        []string `json:"tags"`
			Type        string   `json:"type"`
		}
		if err := json.Unmarshal([]byte(tc.Args), &raw); err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("model enrichment skipped: unparseable output: %v", err))
			return r
		}
		return apply(r, raw.Title, raw.Description, raw.Tags, raw.Type)
	}
	r.Warnings = append(r.Warnings, "model enrichment skipped: model answered without calling submit_frontmatter")
	return r
}

// apply merges model output over the deterministic result field by field,
// keeping every invariant Infer established.
func apply(r Result, title, description string, tags []string, typ string) Result {
	if t := curator.CapTitle(title); t != "" {
		r.Entry.Title = t
	}
	if d := strings.TrimSpace(description); d != "" {
		r.Entry.Description = capRunes(strings.Join(strings.Fields(d), " "), descriptionMaxRunes)
	}
	if validTypes[strings.TrimSpace(typ)] {
		// An Incident still needs a resource; without one, keep the inferred type.
		if strings.TrimSpace(typ) != "Incident" || r.Entry.Resource != "" {
			r.Entry.Type = strings.TrimSpace(typ)
		}
	}
	if len(tags) > 0 {
		r.Entry.Tags = inferTags(tags, "", r.Entry.Type) // rebuild: constant pair + model tags, deduped/capped
	}
	r.DestPath = fmt.Sprintf("%ss/%s.md", strings.ToLower(r.Entry.Type), slugOf(r.Entry.Title))
	return r
}
```

  and, in `infer.go`, factor the two `DestPath` computations through one helper so they can't drift:

```go
// slugOf is the single dest-path slug rule (Infer and Enrich share it).
func slugOf(title string) string { return okf.Slugify(title) }
```

  (Update `Infer` to use `fmt.Sprintf("%ss/%s.md", strings.ToLower(typ), slugOf(title))`.)
- [ ] Run `go test ./internal/kbimport/ -v` — expected PASS (all three test files).
- [ ] Commit: `feat(kbimport): optional model enrichment of inferred frontmatter, never gating`

### Task 6: `lore kb import` command — wiring, validation, dry-run, writes

The command layer in `internal/app`: walk the source dir (same skip rules as `catalog.Load`, `internal/catalog/load.go:29-43` — hidden files/dirs, non-`.md`, `index.md`/`log.md`/`README.md`), Infer (+ Enrich when `--model`), Plan against `catalog.Load(--into)` (NOT `loadKBCatalog` — that helper errors on an *empty* catalog at `kb_cmd.go:57-59`, and a brand-new KB checkout legitimately has zero entries), then run each survivor through `kbvalidate.ValidateStructural` (skip-with-reason on any `SeverityError` — never write a file that would fail the repo's own merge gate), and finally either print the plan (`--dry-run`) or write files via `okf.Render`. Dispatch follows the existing pattern: a new `case "import"` in `app.RunKB` (`internal/app/kb_cmd.go:29-35`) — **stdlib flag, not cobra**, matching every other subcommand.

**Files:**
- Create: `internal/app/kb_import.go`
- Test: `internal/app/kb_import_test.go` (unit level; the fixture e2e lands in Task 7)
- Modify: `internal/app/kb_cmd.go` (dispatch + usage string), `cmd/lore/main.go` (usage const)

**Steps:**

- [ ] Write the failing unit test `internal/app/kb_import_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestKBImportRequiresSrcAndDest(t *testing.T) {
	var buf bytes.Buffer
	if err := runKBImport([]string{}, &buf); err == nil {
		t.Fatal("missing source dir must error")
	}
	src := t.TempDir()
	writeFile(t, src, "a.md", "# A\n\nbody\n")
	if err := runKBImport([]string{src, "--config", filepath.Join(t.TempDir(), "nope.yaml")}, &buf); err == nil {
		t.Fatal("no --into and no config catalog.dir must error")
	}
}

func TestKBImportDryRunWritesNothing(t *testing.T) {
	src, kb := t.TempDir(), t.TempDir()
	writeFile(t, src, "redis-failover.md", "# Redis failover\n\nHow to fail over redis.\n")
	var buf bytes.Buffer
	if err := runKBImport([]string{src, "--into", kb, "--dry-run"}, &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(kb, "playbooks", "redis-failover.md")); !os.IsNotExist(err) {
		t.Fatal("--dry-run must not write files")
	}
	out := buf.String()
	for _, want := range []string{"import", "playbooks/redis-failover.md", "Redis failover", "dry-run"} {
		if !strings.Contains(out, want) {
			t.Fatalf("dry-run output missing %q:\n%s", want, out)
		}
	}
}

func TestKBImportSkipsReservedAndInvalid(t *testing.T) {
	src, kb := t.TempDir(), t.TempDir()
	writeFile(t, src, "README.md", "# not knowledge\n")
	writeFile(t, src, "notes.txt", "not markdown")
	// Declares Incident but has no resource/sections → fails the merge gate → skipped.
	writeFile(t, src, "broken.md", "---\ntype: Incident\ntitle: broken\n---\nno sections\n")
	writeFile(t, src, "ok.md", "# OK runbook\n\nA fine playbook.\n")
	var buf bytes.Buffer
	if err := runKBImport([]string{src, "--into", kb}, &buf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(kb, "playbooks", "ok-runbook.md")); err != nil {
		t.Fatalf("valid entry must be written: %v", err)
	}
	if _, err := os.Stat(filepath.Join(kb, "incidents", "broken.md")); !os.IsNotExist(err) {
		t.Fatal("gate-failing entry must not be written")
	}
	out := buf.String()
	if !strings.Contains(out, "fails validation") {
		t.Fatalf("skip reason must be reported:\n%s", out)
	}
	if strings.Contains(out, "README.md") {
		t.Fatalf("reserved files must be silently ignored:\n%s", out)
	}
}
```

- [ ] Run `go test ./internal/app/ -run TestKBImport -v` — expected FAIL (`runKBImport` undefined).
- [ ] Create `internal/app/kb_import.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/kbimport"
	"github.com/Smana/runlore/internal/kbvalidate"
	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

// runKBImport is `lore kb import <src-dir>`: the cold-start seeder. It
// converts existing markdown runbooks/postmortems into OKF entries in a local
// KB checkout — inferred frontmatter, merge-gate validation, dedup against
// the live catalog — and writes files for the HUMAN to review and commit.
// Nothing is committed or pushed; the review stays in Git, like every KB PR.
func runKBImport(args []string, w io.Writer) error {
	fset := flag.NewFlagSet("kb import", flag.ContinueOnError)
	cfgPath := fset.String("config", "runlore.yaml", "path to config file")
	into := fset.String("into", "", "local KB checkout to write into (default: config catalog.dir)")
	dryRun := fset.Bool("dry-run", false, "print the import plan; write nothing")
	useModel := fset.Bool("model", false, "refine frontmatter with the configured model (optional; import is deterministic without it)")
	rest, err := parseInterleaved(fset, args)
	if err != nil {
		return err
	}
	const usage = "usage: lore kb import <src-dir> [--into <kb-dir>] [--dry-run] [--model] [--config <path>]"
	if len(rest) != 1 {
		return fmt.Errorf("%s", usage)
	}
	src := rest[0]

	dest := *into
	if dest == "" {
		cfg, cerr := config.Load(*cfgPath)
		if cerr != nil {
			return fmt.Errorf("load config: %w (or pass --into <kb-dir>)", cerr)
		}
		dest = cfg.Catalog.Dir
		if dest == "" {
			return fmt.Errorf("no destination (set catalog.dir or pass --into <kb-dir>)")
		}
	}
	if st, serr := os.Stat(dest); serr != nil || !st.IsDir() {
		return fmt.Errorf("KB dir %s is not a directory (clone your KB repo checkout first): %v", dest, serr)
	}

	// Optional model — same opt-in shape as `lore validate-kb --semantic`:
	// no usable model config degrades to deterministic-only with a warning.
	var model providers.ModelProvider
	if *useModel {
		if cfg, cerr := config.Load(*cfgPath); cerr == nil && ModelConfigured(cfg) {
			model = BuildModel(cfg, os.Getenv(cfg.Model.APIKeyEnv))
		} else {
			fmt.Fprintln(os.Stderr, "kb import: --model set but no usable model in config; running deterministic only")
		}
	}

	// Existing catalog, loaded tolerantly: an EMPTY checkout is fine (that is
	// the cold start this command exists for), and one malformed existing
	// entry must not block seeding (catalog.Load already skip-warns).
	existing, skippedLoad, err := catalog.Load(dest)
	if err != nil {
		return fmt.Errorf("load existing catalog %s: %w", dest, err)
	}
	for _, s := range skippedLoad {
		fmt.Fprintf(os.Stderr, "warning: existing entry skipped during dedup scan: %s\n", s)
	}

	sources, err := collectSources(src)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("no importable .md files under %s", src)
	}

	results := make([]kbimport.Result, 0, len(sources))
	for _, p := range sources {
		data, rerr := os.ReadFile(p) //nolint:gosec // G304: user-passed import directory
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(src, p)
		r := kbimport.Infer(data, rel)
		if model != nil {
			r = kbimport.Enrich(context.Background(), r, model)
		}
		results = append(results, r)
	}

	actions := kbimport.Plan(results, existing)

	// Merge-gate validation: never write an entry `lore validate-kb` would
	// reject. Errors demote the action to a skip; warnings are reported only.
	for i := range actions {
		if actions[i].Skip {
			continue
		}
		issues := kbvalidate.ValidateStructural(toCatalogEntry(actions[i].Result))
		for _, iss := range issues {
			if iss.Severity == kbvalidate.SeverityWarning {
				actions[i].Warnings = append(actions[i].Warnings, fmt.Sprintf("%s: %s", iss.Field, iss.Message))
			}
		}
		if kbvalidate.HasErrors(issues) {
			first := firstError(issues)
			actions[i].Skip = true
			actions[i].Reason = fmt.Sprintf("fails validation: %s: %s", first.Field, first.Message)
		}
	}

	imported, skipped := 0, 0
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTION\tSOURCE\tDEST\tTYPE\tTITLE / REASON")
	for _, a := range actions {
		for _, warn := range a.Warnings {
			fmt.Fprintf(os.Stderr, "warning: %s: %s\n", a.Source, warn)
		}
		if a.Skip {
			skipped++
			fmt.Fprintf(tw, "skip\t%s\t-\t-\t%s\n", a.Source, a.Reason)
			continue
		}
		imported++
		fmt.Fprintf(tw, "import\t%s\t%s\t%s\t%s\n", a.Source, a.DestPath, a.Entry.Type, truncateCell(a.Entry.Title, 60))
		if *dryRun {
			continue
		}
		out := filepath.Join(dest, filepath.FromSlash(a.DestPath))
		if merr := os.MkdirAll(filepath.Dir(out), 0o755); merr != nil {
			return merr
		}
		if werr := os.WriteFile(out, []byte(okf.Render(a.Entry, a.Meta)), 0o644); werr != nil { //nolint:gosec // G306: catalog files are world-readable docs
			return werr
		}
	}
	_ = tw.Flush()
	if *dryRun {
		fmt.Fprintf(w, "\ndry-run: would import %d, skip %d (of %d sources); nothing written\n", imported, skipped, len(actions))
		return nil
	}
	fmt.Fprintf(w, "\nimported %d, skipped %d (of %d sources) into %s — review the diff, then commit and push\n",
		imported, skipped, len(actions), dest)
	return nil
}

// collectSources walks src for importable markdown, mirroring catalog.Load's
// skip rules (internal/catalog/load.go): hidden files/dirs, non-.md, and the
// reserved index.md / log.md / README.md are not knowledge entries.
func collectSources(src string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		base := d.Name()
		if d.IsDir() {
			if path != src && strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(base, ".") || !strings.HasSuffix(base, ".md") {
			return nil
		}
		if base == "index.md" || base == "log.md" || strings.EqualFold(base, "readme.md") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out, err
}

// toCatalogEntry adapts an import result to the validator's input type —
// the SAME struct the loader produces, so "valid at import" and "valid at
// merge" are one predicate.
func toCatalogEntry(r kbimport.Result) catalog.Entry {
	return catalog.Entry{
		Type: r.Entry.Type, Title: r.Entry.Title, Description: r.Entry.Description,
		Resource: r.Entry.Resource, Tags: r.Entry.Tags, Body: r.Entry.Body,
		Timestamp: r.Meta.Timestamp, Status: r.Meta.Status, LastValidated: r.Meta.LastValidated,
		Path: r.DestPath,
	}
}

func firstError(issues []kbvalidate.Issue) kbvalidate.Issue {
	for _, iss := range issues {
		if iss.Severity == kbvalidate.SeverityError {
			return iss
		}
	}
	return kbvalidate.Issue{Field: "unknown", Message: "validation error"}
}
```

- [ ] Wire the dispatcher — in `internal/app/kb_cmd.go`, extend `RunKB` (line 24):

```go
	const usage = "usage: lore kb search <query> [flags] | lore kb show <entry> [flags] | lore kb import <src-dir> [flags]"
	...
	case "import":
		return runKBImport(args[1:], os.Stdout)
```

- [ ] Update the usage const in `cmd/lore/main.go` (after the `kb show` line, main.go:31):

```
  lore kb import <src-dir> [--into <kb-dir>] [--dry-run] [--model]   convert existing runbooks/postmortems into OKF entries (cold-start seeding)
```

- [ ] Run `go build ./... && go test ./internal/app/ -run TestKBImport -v` — expected PASS. Note the `dry-run` assertion needs the summary line to contain the literal `dry-run` — it does.
- [ ] Run the full suite touched so far: `go test ./internal/... ./cmd/... -count=1` — expected PASS.
- [ ] Commit: `feat(cli): lore kb import — seed the catalog from existing markdown runbooks`

### Task 7: Fixture-based end-to-end test of the command

Real fixture files under `internal/app/testdata/kbimport/`, exercised through `runKBImport` exactly as a user would: first run imports + dedups, output loads back through `catalog.Load` with **zero** `kbvalidate.WarnInvalid` findings (the round-trip guarantee), second run is a clean no-op (idempotence).

**Files:**
- Create: `internal/app/testdata/kbimport/redis-failover.md`, `internal/app/testdata/kbimport/2024-03-payments-outage.md`, `internal/app/testdata/kbimport/redis-failover-copy.md`, `internal/app/testdata/kbimport/README.md`
- Test: append to `internal/app/kb_import_test.go`

**Steps:**

- [ ] Create the fixtures:

`internal/app/testdata/kbimport/redis-failover.md`:

```markdown
# Redis failover

How to fail over the redis primary when RedisDown fires.

## Steps

1. Confirm the primary is unreachable: `redis-cli -h redis-0 ping`.
2. Promote the replica: `redis-cli -h redis-1 replicaof no one`.
3. Repoint the service selector to the new primary.
```

`internal/app/testdata/kbimport/2024-03-payments-outage.md`:

```markdown
---
title: Payments API outage
date: 2024-03-14
tags: [payments, sev1]
resource: payments/api
type: postmortem
---

## Symptom

5xx spike on payments/api starting 09:12 UTC; PaymentsHighErrorRate alert fired.

## Investigate

- deploy 4a1f2c rolled out at 09:10
- error logs: connection refused to payments-db

## Cause

1. deploy 4a1f2c dropped the DB connection-pool env var

## Resolution

- rollback to the previous release; re-add the env var before re-deploying
```

`internal/app/testdata/kbimport/redis-failover-copy.md` (dedup victim — near-identical title):

```markdown
# Redis failover runbook

Older copy of the failover doc kept in another folder.
```

`internal/app/testdata/kbimport/README.md`:

```markdown
Team runbooks. Not itself a runbook.
```

- [ ] Append the e2e test to `internal/app/kb_import_test.go`:

```go
func TestKBImportEndToEnd(t *testing.T) {
	kb := t.TempDir()
	var buf bytes.Buffer
	if err := runKBImport([]string{"testdata/kbimport", "--into", kb}, &buf); err != nil {
		t.Fatal(err)
	}

	// The bare runbook became a Playbook; the postmortem an Incident.
	playbook, err := os.ReadFile(filepath.Join(kb, "playbooks", "redis-failover.md"))
	if err != nil {
		t.Fatalf("playbook not written: %v", err)
	}
	for _, want := range []string{"type: Playbook", "title: Redis failover", "- redisdown", "## Steps"} {
		if !strings.Contains(string(playbook), want) {
			t.Errorf("playbook missing %q:\n%s", want, playbook)
		}
	}
	incident, err := os.ReadFile(filepath.Join(kb, "incidents", "payments-api-outage.md"))
	if err != nil {
		t.Fatalf("incident not written: %v", err)
	}
	for _, want := range []string{"type: Incident", "resource: payments/api", "2024-03-14", "- payments", "## Cause"} {
		if !strings.Contains(string(incident), want) {
			t.Errorf("incident missing %q:\n%s", want, incident)
		}
	}

	// The near-duplicate title was skipped, the README ignored.
	if !strings.Contains(buf.String(), "duplicate of") && !strings.Contains(buf.String(), "collides") {
		t.Fatalf("redis-failover-copy.md must be skipped as a duplicate:\n%s", buf.String())
	}

	// Round-trip guarantee: everything written loads back and passes the gate.
	entries, skipped, err := catalog.Load(kb)
	if err != nil || len(skipped) > 0 {
		t.Fatalf("written entries must parse: err=%v skipped=%v", err, skipped)
	}
	if n := kbvalidate.WarnInvalid(entries, func(p string, errs []kbvalidate.Issue) {
		t.Errorf("invalid written entry %s: %v", p, errs)
	}); n != 0 {
		t.Fatalf("%d written entries fail the merge gate", n)
	}

	// Idempotence: a second run imports nothing.
	var buf2 bytes.Buffer
	if err := runKBImport([]string{"testdata/kbimport", "--into", kb}, &buf2); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf2.String(), "imported 0") {
		t.Fatalf("re-run must be a no-op:\n%s", buf2.String())
	}
}
```

  Add `"github.com/Smana/runlore/internal/catalog"` and `"github.com/Smana/runlore/internal/kbvalidate"` to the test file's imports.
- [ ] Run `go test ./internal/app/ -run TestKBImportEndToEnd -v` — first run expected FAIL only if any earlier task left a gap; drive it to PASS. Two assertions carry the design: the alert tag `- redisdown` (heuristic: "RedisDown" appears in a line mentioning "fires"… — note it appears in the *first paragraph* which also contains no "alert" keyword and is not a heading; if the tag is absent, the fixture's first line legitimately doesn't match the heuristic — change the fixture line to `How to fail over the redis primary when the RedisDown alert fires.` so the "alert"-mentioning-line rule applies) and `title: Payments API outage` map `date:` → `timestamp:` preservation.
- [ ] Run everything: `go test ./... -count=1` — expected PASS.
- [ ] Commit: `test(kbimport): fixture end-to-end coverage for lore kb import`

### Task 8: Documentation

One real docs addition where evaluators actually land: a Step 1b in `docs/getting-started.md` (between Step 1 "Create the knowledge-catalog repo", line 61, and Step 2, line 128). The CLI usage line was already added in Task 6.

**Files:**
- Modify: `docs/getting-started.md`

**Steps:**

- [ ] Insert this section immediately before the `## Step 2 — GitHub App for curation (optional)` heading:

```markdown
## Step 1b — Seed the catalog from your existing runbooks (optional)

You don't have to start empty. If your team already keeps markdown runbooks or
postmortems anywhere, import them and get recall value on day one:

​```bash
# preview what would be written — nothing is touched
lore kb import ./our-runbooks --into ./kb --dry-run

# convert + validate + dedup, then write into your local KB checkout
lore kb import ./our-runbooks --into ./kb
cd kb && git add . && git commit -m "seed catalog from existing runbooks" && git push
​```

What `import` does, deterministically (no model, no config needed beyond the
directory paths):

- **Adds/normalizes OKF frontmatter** — title from the existing frontmatter or
  the first heading (filename as last resort), description from the first
  paragraph, tags from the document's own tags **plus detected alert names**
  (`KubePodCrashLooping`-style tokens in headings and alert-mentioning lines —
  exactly the recall signal that lets a future alert find the runbook).
- **Classifies** — a document that already carries `Symptom`/`Cause`/`Resolution`
  sections *and* names a `resource` becomes an `Incident`; everything else is a
  `Playbook` (free-form runbooks validate relaxed, same as hand-written entries).
- **Validates** every entry with the same merge gate as `lore validate-kb` —
  nothing is written that the gate would later reject.
- **Dedups** against what the catalog already holds (exact and fuzzy title, same
  rule the curator uses for duplicate PRs) and skips it with a printed reason.

Nothing is committed for you: you review the diff and push, the same
human-in-the-loop bar as every RunLore KB PR. With `--model`, the LLM already
configured in your `runlore.yaml` refines titles/descriptions/tags (purely
optional — a model failure falls back to the deterministic result). Re-running
the same import is a no-op.
​```
```

  (Strip the zero-width escapes around the inner code fences when editing — they are only here to keep this plan's own markdown intact. The final inserted section ends after "no-op.", with a blank line before `## Step 2`.)
- [ ] Proofread the rendered section (`grep -n "Step 1b" docs/getting-started.md`), confirm the Step numbering flow reads naturally (1 → 1b → 2).
- [ ] Commit: `docs: getting-started Step 1b — seed the catalog with lore kb import`

---

## Acceptance criteria

- [ ] `lore kb import <src-dir> --into <kb-dir>` converts a directory of plain markdown runbooks/postmortems into OKF entries under `<type>s/<slug>.md` in the checkout, ready to `git add` — the command never commits, pushes, or touches a forge.
- [ ] Frontmatter inference is deterministic: same input, same output; title/description/tags/type derived from headings + content per Task 3's rules; `resource` is passthrough-only (never guessed); source `date:`/`timestamp:` preserved when parseable by `catalog.ParseEntryDate`.
- [ ] Every written entry passes `kbvalidate.ValidateStructural` with zero errors and parses back through `catalog.Load` (round-trip pinned by `TestKBImportEndToEnd`); gate-failing sources are skipped with a printed reason, never written.
- [ ] Dedup: exact/fuzzy title match (via the exported `curate.TitleJaccard`, threshold 0.6), existing-path and in-batch path collisions, and `status: retired` sources are all skipped with reasons; re-running the same import reports `imported 0`.
- [ ] `--dry-run` prints the full ACTION/SOURCE/DEST/TYPE table and writes nothing.
- [ ] `--model` is optional and never gates: nil/unconfigured model, model error, or unusable output degrade to the deterministic result with a warning; the model can never set `resource` or an out-of-vocabulary `type`.
- [ ] No new **required** config: destination falls back to the existing `catalog.dir`; the model comes from the existing `model:` block; both are overridable by flags only.
- [ ] No parallel entry format: the command flows through `providers.KBEntry`, the extracted `okf.Render` (which the GitHub forge now also delegates to, existing forge tests green), `kbvalidate.ValidateStructural`, and `catalog.Load`.
- [ ] `cmd/lore/main.go` usage and `docs/getting-started.md` Step 1b document the command; `go test ./... -count=1` and `go build ./...` pass on the branch.
- [ ] Every commit uses a conventional message with **no** Co-Authored-By line.
