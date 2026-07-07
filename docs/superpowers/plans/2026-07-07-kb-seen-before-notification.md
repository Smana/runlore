# "Seen before" Notification Block Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** When a known incident recurs, the notification quotes the merged KB entry's cause and human-reviewed resolution inline (plus its recall track record), so the on-call reads the previous answer with zero clicks. Spec: `docs/superpowers/specs/2026-07-07-kb-human-surfaces-design.md`, Feature 1.

**Architecture:** At completion (`onInvestigationComplete`), a recurring fresh investigation looks up the merged catalog entry by its `DupFingerprint` (already stored in entry frontmatter), extracts `## Cause` / `## Resolution` excerpts via a new `catalog.Entry.Section()`, joins the entry's recall/resolve aggregate from the outcome ledger, and stamps a new `providers.PriorKnowledge` onto the `Investigation`. `notify.Format` (Matrix/webhook/fallback) and the Slack summary blocks render it as a "📚 Seen before" block placed before the current root cause. Everything is best-effort: any miss degrades to today's counter+link.

**Tech Stack:** Go 1.26, stdlib only. Tests: `go test -race ./...`, table-driven. Lint: `golangci-lint run ./...`, `gofmt`.

## Global Constraints

- Go 1.26 (go.mod); CI runs `go build ./...`, `go vet ./...`, `test -z "$(gofmt -l .)"`, `go test -race ./...`.
- Never add AI attribution to commits or PRs. Conventional-commit prefixes (`feat:`, `fix:`, `test:`, `docs:`).
- **mrkdwn-escape invariant** (`internal/notify/slack.go` + `TestFormatScaffoldingHasNoMrkdwnMeta`): any scaffolding (literals) added to `notify.Format` must contain NONE of `&`, `<`, `>`. Slack `<!date^…>` tokens are Slack-blocks-only, never in `Format`.
- All untrusted strings (model prose, KB entry bodies, URLs) interpolated into Slack mrkdwn blocks go through `escapeMrkdwn`. Slack section text: `truncate(s, 2900)`.
- Prior-knowledge stamping and rendering are best-effort: a lookup/parse failure must never fail or delay delivery.
- Keep the comment density/style of surrounding code (this repo comments the *why* heavily).
- Work on a dedicated branch (e.g. `feat/kb-seen-before`), one PR for this whole plan.

---

### Task 1: `catalog.Entry.Section` — quotable OKF section excerpts

**Files:**
- Create: `internal/catalog/section.go`
- Test: `internal/catalog/section_test.go`

**Interfaces:**
- Produces: `func (e Entry) Section(name string) string` — first paragraph of the `## <name>` markdown section, flattened to one line, `**` stripped, truncated to 300 runes with `…`. `""` when absent/empty. Tasks 2 depends on this exact behavior.

- [ ] **Step 1: Write the failing test**

```go
package catalog

import (
	"strings"
	"testing"
	"unicode/utf8"
)

// Section quotes an entry's Cause/Resolution into chat + PR bodies: first
// paragraph only, one line, bold markers stripped, hard length cap.
func TestEntrySection(t *testing.T) {
	body := `## Decision

- **why keep:** x

## Cause

1. **ConfigMap truncated after kustomize bump** (85%) — change: flux/apps
2. **DNS flake** (10%)

## Resolution

- revert the patch and pin kustomize 5.3.2 (reversible=true)

## Citations

[1] flux/apps
`
	e := Entry{Body: body}
	cases := []struct{ name, want string }{
		{"Cause", "1. ConfigMap truncated after kustomize bump (85%) — change: flux/apps 2. DNS flake (10%)"},
		{"Resolution", "- revert the patch and pin kustomize 5.3.2 (reversible=true)"},
		{"resolution", "- revert the patch and pin kustomize 5.3.2 (reversible=true)"}, // case-insensitive
		{"Symptom", ""}, // absent section
	}
	for _, c := range cases {
		if got := e.Section(c.name); got != c.want {
			t.Errorf("Section(%q) = %q, want %q", c.name, got, c.want)
		}
	}
}

// The first BLANK line after content ends the excerpt: a second paragraph in
// the same section is not quoted.
func TestEntrySectionFirstParagraphOnly(t *testing.T) {
	e := Entry{Body: "## Cause\n\nfirst para line one\nline two\n\nsecond para\n\n## Next\n\nx\n"}
	if got := e.Section("Cause"); got != "first para line one line two" {
		t.Errorf("Section(Cause) = %q, want first paragraph only", got)
	}
}

func TestEntrySectionTruncates(t *testing.T) {
	e := Entry{Body: "## Cause\n\n" + strings.Repeat("word ", 200) + "\n"}
	got := e.Section("Cause")
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("long section not truncated: %q", got)
	}
	if n := utf8.RuneCountInString(got); n > 301 {
		t.Fatalf("excerpt is %d runes, want ≤ 301 (300 + ellipsis)", n)
	}
}

func TestEntrySectionMalformed(t *testing.T) {
	cases := []struct{ label, body string }{
		{"empty body", ""},
		{"heading only", "## Cause\n"},
		{"heading then next heading", "## Cause\n\n## Resolution\n\n- x\n"},
	}
	for _, c := range cases {
		if got := (Entry{Body: c.body}).Section("Cause"); got != "" {
			t.Errorf("%s: Section(Cause) = %q, want \"\"", c.label, got)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/catalog/ -run TestEntrySection -v`
Expected: FAIL — `e.Section undefined`.

- [ ] **Step 3: Implement `internal/catalog/section.go`**

```go
package catalog

import (
	"strings"
	"unicode/utf8"
)

// sectionMaxRunes caps a Section excerpt: enough to quote an entry's cause or
// resolution in a chat notification / PR body without reproducing the document.
const sectionMaxRunes = 300

// Section returns the first paragraph of the entry body's "## <name>" markdown
// section, flattened to a single line — the quotable essence of what the entry
// says. Matching is case-insensitive and accepts any ATX heading level. Bold
// markers (**) are stripped: the excerpt is interpolated into Slack mrkdwn and
// PR bodies, where a literal ** renders as stray asterisks. Returns "" when the
// section is absent or empty — callers must treat that as "nothing to quote"
// and never render an empty block.
func (e Entry) Section(name string) string {
	want := strings.TrimSpace(name)
	var para []string
	in := false
	for _, ln := range strings.Split(e.Body, "\n") {
		trimmed := strings.TrimSpace(ln)
		if h := headingText(trimmed); h != "" {
			if in {
				break // next section starts: the excerpt is done
			}
			in = strings.EqualFold(h, want)
			continue
		}
		if !in {
			continue
		}
		if trimmed == "" {
			if len(para) > 0 {
				break // blank line after content: first paragraph is done
			}
			continue // leading blank between the heading and its content
		}
		para = append(para, trimmed)
	}
	s := strings.ReplaceAll(strings.Join(para, " "), "**", "")
	return truncateRunes(s, sectionMaxRunes)
}

// headingText returns the text of an ATX markdown heading line ("## Cause" →
// "Cause"), or "" when the line is not a heading.
func headingText(line string) string {
	i := 0
	for i < len(line) && line[i] == '#' {
		i++
	}
	if i == 0 || i > 6 || i >= len(line) || line[i] != ' ' {
		return ""
	}
	return strings.TrimSpace(line[i:])
}

// truncateRunes caps s at max runes, appending … when cut — rune-aware so a
// multibyte character is never split.
func truncateRunes(s string, max int) string {
	if utf8.RuneCountInString(s) <= max {
		return s
	}
	r := []rune(s)
	return strings.TrimRight(string(r[:max]), " ") + "…"
}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/catalog/ -run TestEntrySection -v`
Expected: PASS (all four tests).

- [ ] **Step 5: Commit**

```bash
git add internal/catalog/section.go internal/catalog/section_test.go
git commit -m "feat(catalog): add Entry.Section for quotable OKF section excerpts"
```

---

### Task 2: `providers.PriorKnowledge` + stamping in `onInvestigationComplete`

**Files:**
- Modify: `internal/providers/providers.go` (Investigation struct, after `PrevCuratedURL` ~line 443)
- Modify: `internal/app/investigate.go` (`onInvestigationComplete` ~line 319, its closure call site ~line 299, and the `dupFP` computation ~line 360)
- Test: `internal/app/oncomplete_test.go`

**Interfaces:**
- Consumes: `catalog.Entry.Section(name string) string` (Task 1); existing `Catalog.FindFingerprint(fp) (Entry, bool)`; `curator.DupFingerprint(inv)`; `ledger.OpenCounts() (map[string]outcome.Aggregate, error)`.
- Produces: `providers.PriorKnowledge{Cause, Resolution, EntryPath string; Recalls, Resolved int}`; `Investigation.Prior *PriorKnowledge`; new `onInvestigationComplete` signature with a `prior priorEntryFinder` param inserted after `ledger`. Tasks 3–5 rely on `Investigation.Prior` and its field names exactly.

- [ ] **Step 1: Write the failing tests** — append to `internal/app/oncomplete_test.go` (imports to add: `github.com/Smana/runlore/internal/catalog`):

```go
// fakePrior stubs the catalog's exact-identity lookup so the completion
// pipeline can be tested without building a bleve index.
type fakePrior struct {
	e  catalog.Entry
	ok bool
}

func (f fakePrior) FindFingerprint(string) (catalog.Entry, bool) { return f.e, f.ok }

// TestOnCompleteStampsPriorKnowledge: a recurring fresh investigation whose
// merged KB entry is findable by dup-fingerprint must deliver Prior with the
// entry's Cause/Resolution excerpts and its recall track record.
func TestOnCompleteStampsPriorKnowledge(t *testing.T) {
	path := filepath.Join(t.TempDir(), "outcomes.jsonl")
	ledger, err := outcome.New(path)
	if err != nil {
		t.Fatalf("new ledger: %v", err)
	}
	// Prior open for the same trigger key ⇒ this run is occurrence #2.
	if err := ledger.Open(outcome.Event{
		Fingerprint: "fp0", TriggerKey: "k", CuratedURL: "https://kb/prev",
		At: time.Now().Add(-4 * time.Hour),
	}); err != nil {
		t.Fatalf("seed open: %v", err)
	}
	// Track record for the merged entry: one resolvable recall that resolved.
	rv := true
	if err := ledger.Open(outcome.Event{
		Fingerprint: "fpr", Kind: "recall", Entry: "incidents/e.md",
		Resolvable: &rv, At: time.Now().Add(-3 * time.Hour),
	}); err != nil {
		t.Fatalf("seed recall open: %v", err)
	}
	if _, _, err := ledger.Resolve("fpr", time.Now().Add(-2*time.Hour)); err != nil {
		t.Fatalf("seed resolve: %v", err)
	}

	entry := catalog.Entry{
		Path: "incidents/e.md",
		Body: "## Cause\n\n1. **bad kustomize bump** (85%)\n\n## Resolution\n\n- revert and pin 5.3.2\n",
	}
	sink := &captureNotifier{}
	notifier := notify.NewMulti(discardLog(), sink)
	found := providers.Investigation{Title: "disk pressure", Fingerprint: "fp1", TriggerKey: "k"}
	onInvestigationComplete(context.Background(), found, ledger, fakePrior{e: entry, ok: true}, nil, notifier, nil, nil, nil, discardLog())

	p := sink.got.Prior
	if p == nil {
		t.Fatal("Prior not stamped on a recurring fresh investigation")
	}
	if p.Cause != "1. bad kustomize bump (85%)" {
		t.Errorf("Prior.Cause = %q", p.Cause)
	}
	if p.Resolution != "- revert and pin 5.3.2" {
		t.Errorf("Prior.Resolution = %q", p.Resolution)
	}
	if p.EntryPath != "incidents/e.md" {
		t.Errorf("Prior.EntryPath = %q", p.EntryPath)
	}
	if p.Recalls != 1 || p.Resolved != 1 {
		t.Errorf("Prior track record = %d/%d, want 1/1", p.Resolved, p.Recalls)
	}
}

// Recalled investigations must NOT get Prior (the recalled entry IS the
// delivered answer), and a first sighting or a fingerprint miss leaves it nil.
func TestOnCompletePriorKnowledgeSkips(t *testing.T) {
	entry := catalog.Entry{Path: "incidents/e.md", Body: "## Cause\n\nc\n\n## Resolution\n\nr\n"}
	cases := []struct {
		label    string
		seed     bool // seed a prior open (⇒ Occurrences 2)
		recalled bool
		found    bool // FindFingerprint hit
	}{
		{"recall path", true, true, true},
		{"first sighting", false, false, true},
		{"no merged entry", true, false, false},
	}
	for _, c := range cases {
		path := filepath.Join(t.TempDir(), "outcomes.jsonl")
		ledger, err := outcome.New(path)
		if err != nil {
			t.Fatalf("%s: new ledger: %v", c.label, err)
		}
		if c.seed {
			if err := ledger.Open(outcome.Event{Fingerprint: "fp0", TriggerKey: "k", At: time.Now().Add(-time.Hour)}); err != nil {
				t.Fatalf("%s: seed: %v", c.label, err)
			}
		}
		sink := &captureNotifier{}
		notifier := notify.NewMulti(discardLog(), sink)
		found := providers.Investigation{Title: "t", Fingerprint: "fp1", TriggerKey: "k", Recalled: c.recalled}
		onInvestigationComplete(context.Background(), found, ledger, fakePrior{e: entry, ok: c.found}, nil, notifier, nil, nil, nil, discardLog())
		if sink.got.Prior != nil {
			t.Errorf("%s: Prior = %+v, want nil", c.label, sink.got.Prior)
		}
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/app/ -run TestOnCompletePrior -v`
Expected: compile FAIL — wrong argument count for `onInvestigationComplete`, `Prior` undefined.

- [ ] **Step 3: Implement.** In `internal/providers/providers.go`, immediately after the `PrevCuratedURL` field of `Investigation` (~line 443), add the field, and the type right after the `Investigation` struct:

```go
	Prior          *PriorKnowledge // what the merged KB entry already says about this recurring incident; nil when unknown (see PriorKnowledge)
```

```go
// PriorKnowledge is what the knowledge base already says about a recurring
// incident: excerpts of the merged entry's Cause and (human-reviewed)
// Resolution sections, plus the entry's recall track record from the outcome
// ledger. Stamped at completion — never seen by the model — and only on FRESH
// investigations of a recurring TriggerKey whose merged entry is findable by
// dup-fingerprint; nil otherwise, so notifiers fall back to the counter+link.
type PriorKnowledge struct {
	Cause      string // excerpt of the merged entry's "## Cause" section
	Resolution string // excerpt of "## Resolution" — carries the human's review edits, the payoff of curation
	EntryPath  string // catalog path of the merged entry
	Recalls    int    // times the entry answered an incident via instant recall
	Resolved   int    // recalls followed by an incident-resolved signal
}
```

In `internal/app/investigate.go`:

1. Add the narrow interface next to `investigationCurator` (~line 308):

```go
// priorEntryFinder is the catalog's exact-identity lookup used to quote the
// merged KB entry on a recurring incident (implemented by *catalog.Catalog).
// Narrowed to an interface so the completion pipeline is testable without an index.
type priorEntryFinder interface {
	FindFingerprint(fp string) (catalog.Entry, bool)
}
```

2. Change the signature (insert `prior` after `ledger`):

```go
func onInvestigationComplete(ctx context.Context, found providers.Investigation, ledger *outcome.Ledger, prior priorEntryFinder, cur investigationCurator, notifier *notify.Multi, auto *action.Auto, approvals *action.Approvals, metrics *telemetry.Metrics, log *slog.Logger) {
```

3. Directly after the existing recurrence-facts block (after `found.Occurrences = 1` ~line 330), hoist the dup fingerprint and stamp Prior:

```go
	// The dedup fingerprint is this incident's deterministic identity: the merged
	// KB entry carries it in frontmatter and the ledger opens below stamp it —
	// computed once for both uses.
	dupFP := curator.DupFingerprint(found)
	// Prior knowledge: on a RECURRING fresh investigation, quote what the merged
	// KB entry already says (cause + human-reviewed resolution + recall track
	// record) so the on-call reads the previous answer in the notification
	// instead of clicking through to the forge. Recalls are excluded — the
	// recalled entry IS the answer being delivered. Best-effort by construction:
	// no merged entry, empty sections, or a ledger error leave Prior nil and the
	// notification falls back to the counter+link it already carries.
	if found.Occurrences > 1 && !found.Recalled && prior != nil {
		if e, ok := prior.FindFingerprint(dupFP); ok {
			cause, resolution := e.Section("Cause"), e.Section("Resolution")
			if cause != "" || resolution != "" {
				pk := &providers.PriorKnowledge{Cause: cause, Resolution: resolution, EntryPath: e.Path}
				if counts, err := ledger.OpenCounts(); err == nil {
					agg := counts[e.Path]
					pk.Recalls, pk.Resolved = agg.Recalls, agg.Resolved
				}
				found.Prior = pk
			}
		}
	}
```

4. Delete the now-duplicate `dupFP := curator.DupFingerprint(found)` inside the `if len(fps) > 0` block (~line 360) — the comment above it moves up with the hoisted computation.

5. Update the `OnComplete` closure (~line 299). A nil `*catalog.Catalog` must stay a nil **interface** — a typed-nil would pass the `!= nil` guard and panic on the first lookup:

```go
		var prior priorEntryFinder
		if cat != nil {
			prior = cat
		}
```
(place just before the `return &investigate.LoopInvestigator{...}`), and in the closure:

```go
			OnComplete: func(found providers.Investigation) {
				onInvestigationComplete(ctx, found, ledger, prior, curOrNil, notifier, auto, approvals, metrics, log)
			},
```

6. Update every existing `onInvestigationComplete(` call in `internal/app/oncomplete_test.go` to pass `nil` as the new 4th argument (after `ledger`).

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/app/ -race -v -run TestOnComplete`
Expected: PASS — the two new tests and all pre-existing `TestOnComplete*` tests.

- [ ] **Step 5: Commit**

```bash
git add internal/providers/providers.go internal/app/investigate.go internal/app/oncomplete_test.go
git commit -m "feat(app): stamp prior KB knowledge on recurring fresh investigations"
```

---

### Task 3: render Prior in `notify.Format` (Matrix/webhook text/Slack fallback)

**Files:**
- Modify: `internal/notify/format.go` (~lines 58–65, the `inv.Occurrences > 1` block)
- Test: `internal/notify/format_test.go`

**Interfaces:**
- Consumes: `Investigation.Prior` (Task 2).
- Produces: `Format` output containing `📚 Seen before: ×N`, `Prior cause:`, `Prior resolution:`, `Resolve rate: R/N recalls resolved`, `Previous conclusion: <url>` lines. Scaffolding stays free of `&` `<` `>`.

- [ ] **Step 1: Write the failing test** — add to `format_test.go`:

```go
func TestFormatPriorKnowledge(t *testing.T) {
	inv := providers.Investigation{
		Title: "t", Confidence: 0.8,
		Occurrences:    3,
		LastOccurrence: time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
		PrevCuratedURL: "https://kb/pr/12",
		Prior: &providers.PriorKnowledge{
			Cause: "ConfigMap truncated after kustomize bump", Resolution: "revert the patch and pin 5.3.2",
			Recalls: 3, Resolved: 3,
		},
	}
	out := Format(inv)
	for _, want := range []string{
		"📚 Seen before: ×3 — last investigated 2026-06-25T10:00:00Z",
		"Prior cause: ConfigMap truncated after kustomize bump",
		"Prior resolution: revert the patch and pin 5.3.2",
		"Resolve rate: 3/3 recalls resolved",
		"Previous conclusion: https://kb/pr/12",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Format missing %q\n---\n%s", want, out)
		}
	}
}

// Without Prior the block keeps today's counter+link shape (no empty labels).
func TestFormatSeenBeforeWithoutPrior(t *testing.T) {
	inv := providers.Investigation{
		Title: "t", Confidence: 0.8,
		Occurrences: 2, LastOccurrence: time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
		PrevCuratedURL: "https://kb/pr/12",
	}
	out := Format(inv)
	if !strings.Contains(out, "📚 Seen before: ×2") {
		t.Errorf("missing seen-before counter:\n%s", out)
	}
	for _, absent := range []string{"Prior cause:", "Prior resolution:", "Resolve rate:"} {
		if strings.Contains(out, absent) {
			t.Errorf("Format must omit %q when Prior is nil:\n%s", absent, out)
		}
	}
}
```

Also update the two existing expectations:
- In `TestFormatVerdictMetadataRecurrence` (~line 68): replace the expected substring `"Occurrence: #` (old wording) with `"📚 Seen before: ×` (keep the same occurrence number the test already uses).
- In `TestFormatScaffoldingHasNoMrkdwnMeta` (~line 131): extend the fixture Investigation with `Occurrences: 2, LastOccurrence: time.Now(), PrevCuratedURL: "https://kb/pr/1", Prior: &providers.PriorKnowledge{Cause: "c", Resolution: "r", Recalls: 1, Resolved: 1}` so the invariant covers the new scaffolding.

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/notify/ -run TestFormat -v`
Expected: FAIL — new tests missing output; `TestFormatVerdictMetadataRecurrence` still passes until the impl changes (that's fine).

- [ ] **Step 3: Implement** — replace the `inv.Occurrences > 1` block in `Format` (format.go:58-65) with:

```go
	// Seen-before block: only when this is a repeat of a known incident (a first
	// sighting — Occurrences ≤ 1, or 0 = ledger disabled — prints nothing). When
	// the completion pipeline found the merged KB entry for this incident
	// (Prior), the previous cause and human-reviewed resolution are quoted
	// inline — the zero-click payoff of the knowledge base; otherwise the
	// counter + link still tell the reader this is not new.
	if inv.Occurrences > 1 {
		fmt.Fprintf(&b, "📚 Seen before: ×%d — last investigated %s\n", inv.Occurrences, inv.LastOccurrence.UTC().Format(time.RFC3339))
		if p := inv.Prior; p != nil {
			if p.Cause != "" {
				fmt.Fprintf(&b, "   Prior cause: %s\n", p.Cause)
			}
			if p.Resolution != "" {
				fmt.Fprintf(&b, "   Prior resolution: %s\n", p.Resolution)
			}
			if p.Recalls > 0 {
				fmt.Fprintf(&b, "   Resolve rate: %d/%d recalls resolved\n", p.Resolved, p.Recalls)
			}
		}
		if inv.PrevCuratedURL != "" {
			fmt.Fprintf(&b, "Previous conclusion: %s\n", inv.PrevCuratedURL)
		}
	}
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/notify/ -race -run TestFormat -v`
Expected: PASS — including the updated `TestFormatVerdictMetadataRecurrence` and `TestFormatScaffoldingHasNoMrkdwnMeta`.

- [ ] **Step 5: Commit**

```bash
git add internal/notify/format.go internal/notify/format_test.go
git commit -m "feat(notify): quote prior KB cause/resolution in the shared format"
```

---

### Task 4: render Prior in the Slack summary blocks

**Files:**
- Modify: `internal/notify/slack.go` (`summaryBlocks` — insert block 3b after the metadata-fields block ~line 341; `metadataFields` ~line 487; block 8 ~line 408)
- Test: `internal/notify/slack_test.go`

**Interfaces:**
- Consumes: `Investigation.Prior` (Task 2).
- Produces: a Slack `section` block `📚 *Seen before ×N* — last <date> …` placed between metadata fields and the top root cause; when `Prior != nil` the old `Recurrence` metadata field and the block-8 context pointer are suppressed (the new block carries count + link).

- [ ] **Step 1: Write the failing test** — add to `slack_test.go`:

```go
// blocksText flattens summaryBlocks into one string for containment asserts.
func blocksText(t *testing.T, blocks []map[string]any) string {
	t.Helper()
	b, err := json.Marshal(blocks)
	if err != nil {
		t.Fatalf("marshal blocks: %v", err)
	}
	return string(b)
}

func TestSlackSummaryBlocksPriorKnowledge(t *testing.T) {
	inv := providers.Investigation{
		Title: "CrashLoopBackOff", Confidence: 0.86,
		Occurrences:    3,
		LastOccurrence: time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
		PrevCuratedURL: "https://kb/pr/12",
		Prior: &providers.PriorKnowledge{
			Cause: "ConfigMap truncated <v5.4>", Resolution: "revert & pin 5.3.2",
			Recalls: 3, Resolved: 3,
		},
	}
	txt := blocksText(t, summaryBlocks(inv))
	for _, want := range []string{
		"📚 *Seen before ×3*",
		"*Prior cause:* ConfigMap truncated &lt;v5.4&gt;", // untrusted entry text is escaped
		"*Prior resolution:* revert &amp; pin 5.3.2",
		"previous entry",     // link label
		"resolve rate 3/3",   // track record
	} {
		if !strings.Contains(txt, want) {
			t.Errorf("summary blocks missing %q\n%s", want, txt)
		}
	}
	// The new block replaces the old pointers: no duplicate recurrence renders.
	for _, absent := range []string{"Previously investigated", "*Recurrence:*"} {
		if strings.Contains(txt, absent) {
			t.Errorf("summary blocks must not still render %q when Prior is set\n%s", absent, txt)
		}
	}
}

// Without Prior, the legacy counter field + context pointer stay untouched.
func TestSlackSummaryBlocksRecurrenceWithoutPrior(t *testing.T) {
	inv := providers.Investigation{
		Title: "CrashLoopBackOff", Confidence: 0.86,
		Occurrences: 2, LastOccurrence: time.Date(2026, 6, 25, 10, 0, 0, 0, time.UTC),
		PrevCuratedURL: "https://kb/pr/12",
	}
	txt := blocksText(t, summaryBlocks(inv))
	if !strings.Contains(txt, "Previously investigated") {
		t.Errorf("legacy recurrence pointer missing without Prior\n%s", txt)
	}
	if strings.Contains(txt, "Seen before") {
		t.Errorf("Seen-before block must not render without Prior\n%s", txt)
	}
}
```

(Add `"encoding/json"` and `"time"` to the test imports if absent.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/notify/ -run TestSlackSummaryBlocks -v`
Expected: FAIL — no Seen-before block yet.

- [ ] **Step 3: Implement.** In `summaryBlocks`, insert after the metadata-fields block (after ~line 341) :

```go
	// 3b. Prior knowledge — on a recurring incident with a merged KB entry, quote
	// what the KB already says (cause + human-reviewed resolution + track record)
	// before the current analysis: history frames how the on-call reads what
	// follows, with zero clicks. The entry excerpts are untrusted (model prose,
	// human edits) and escaped; when this block renders, the legacy Recurrence
	// field and the previously-investigated context pointer are suppressed —
	// count, date and link all live here.
	if p := inv.Prior; p != nil {
		var s strings.Builder
		fmt.Fprintf(&s, "📚 *Seen before ×%d* — last %s", inv.Occurrences, slackDate(inv.LastOccurrence))
		if p.Cause != "" {
			fmt.Fprintf(&s, "\n*Prior cause:* %s", escapeMrkdwn(p.Cause))
		}
		if p.Resolution != "" {
			fmt.Fprintf(&s, "\n*Prior resolution:* %s", escapeMrkdwn(p.Resolution))
		}
		foot := make([]string, 0, 2)
		if inv.PrevCuratedURL != "" {
			foot = append(foot, fmt.Sprintf("<%s|previous entry>", escapeMrkdwn(inv.PrevCuratedURL)))
		}
		if p.Recalls > 0 {
			foot = append(foot, fmt.Sprintf("resolve rate %d/%d", p.Resolved, p.Recalls))
		}
		if len(foot) > 0 {
			fmt.Fprintf(&s, "\n%s", strings.Join(foot, " · "))
		}
		blocks = append(blocks, map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": truncate(s.String(), 2900)}})
	}
```

In `metadataFields` (~line 487), suppress the redundant field when the block renders:

```go
	if inv.Occurrences > 1 && inv.Prior == nil {
		add("Recurrence", fmt.Sprintf("🔁 #%d · last %s", inv.Occurrences, slackDate(inv.LastOccurrence)))
	}
```

In block 8 (~line 408), same suppression:

```go
	if inv.PrevCuratedURL != "" && inv.Prior == nil {
```

- [ ] **Step 4: Run to verify pass**

Run: `go test ./internal/notify/ -race -v`
Expected: PASS — new tests plus every pre-existing Slack test (fallback-escape invariant included).

- [ ] **Step 5: Commit**

```bash
git add internal/notify/slack.go internal/notify/slack_test.go
git commit -m "feat(notify): Slack seen-before block with prior cause and resolution"
```

---

### Task 5: webhook payload, docs, full verification

**Files:**
- Modify: `internal/notify/webhook/webhook.go` (payload struct ~line 44, `Deliver` ~line 65)
- Modify: `docs/learning-loop.md` (§4, the paragraph on recurrence facts)
- Test: `internal/notify/webhook/webhook_test.go`

**Interfaces:**
- Consumes: `Investigation.Prior` (Task 2).
- Produces: webhook JSON gains `"prior": {"cause", "resolution", "entry_path", "recalls", "resolved"}` (omitted when nil).

- [ ] **Step 1: Write the failing test** — in `webhook_test.go`, add a case asserting the JSON body (follow the file's existing `httptest` pattern):

```go
func TestWebhookDeliverPriorKnowledge(t *testing.T) {
	var got payload
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode: %v", err)
		}
	}))
	defer srv.Close()
	n := New(srv.URL)
	inv := providers.Investigation{
		Title: "t", Confidence: 0.8, Occurrences: 3,
		Prior: &providers.PriorKnowledge{Cause: "c", Resolution: "r", EntryPath: "incidents/e.md", Recalls: 3, Resolved: 2},
	}
	if err := n.Deliver(context.Background(), inv); err != nil {
		t.Fatalf("deliver: %v", err)
	}
	if got.Prior == nil || got.Prior.Cause != "c" || got.Prior.Resolution != "r" ||
		got.Prior.EntryPath != "incidents/e.md" || got.Prior.Recalls != 3 || got.Prior.Resolved != 2 {
		t.Errorf("prior payload = %+v", got.Prior)
	}
}
```

(`webhook.New(url string) *Notifier` is the actual constructor — the call above matches it. Add `net/http`, `net/http/httptest`, `encoding/json`, `context` to the test imports if absent.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/notify/webhook/ -run TestWebhookDeliverPrior -v`
Expected: FAIL — `got.Prior` undefined.

- [ ] **Step 3: Implement.** In `webhook.go` add below `payload`:

```go
// priorPayload mirrors providers.PriorKnowledge for webhook consumers: what the
// merged KB entry said last time this incident fired.
type priorPayload struct {
	Cause      string `json:"cause,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	EntryPath  string `json:"entry_path,omitempty"`
	Recalls    int    `json:"recalls,omitempty"`
	Resolved   int    `json:"resolved,omitempty"`
}
```

Add to `payload`: `Prior *priorPayload \`json:"prior,omitempty"\`` and in `Deliver`, before marshalling:

```go
	var prior *priorPayload
	if p := inv.Prior; p != nil {
		prior = &priorPayload{Cause: p.Cause, Resolution: p.Resolution, EntryPath: p.EntryPath, Recalls: p.Recalls, Resolved: p.Resolved}
	}
```
and set `Prior: prior,` in the `payload{...}` literal.

- [ ] **Step 4: Docs.** In `docs/learning-loop.md` §4, extend the paragraph that ends "…so a repeat alert is visibly flagged as recurring rather than looking brand-new." with one sentence:

> When the recurring incident's merged entry is findable by dup-fingerprint, the notification also quotes the entry's **cause and human-reviewed resolution** inline (with its recall resolve-rate), so the previous answer is readable without leaving chat.

- [ ] **Step 5: Full verification**

Run: `go build ./... && go vet ./... && test -z "$(gofmt -l .)" && go test -race ./... && golangci-lint run ./...`
Expected: all green.

- [ ] **Step 6: Commit**

```bash
git add internal/notify/webhook/ docs/learning-loop.md
git commit -m "feat(notify): carry prior KB knowledge in the webhook payload"
```
