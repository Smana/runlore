# Notification Overhaul Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Rework investigation notifications so an on-call can answer "do I need to do anything?" in one glance: an explicit verdict, a metadata field block, recurrence awareness, ruled-out/data-gap separation, Slack threading (compact channel message + full detail in a thread reply), and 👍/👎 feedback buttons feeding the outcome ledger.

**Architecture:** Three new model-populated fields (`verdict`, `ruled_out`, `data_gaps`) flow through the `submit_findings` schema → `findings` struct → `providers.Investigation`. Alert metadata (severity, cluster, tenant, alertname, startsAt) is stamped from the `investigate.Request` at loop completion. The outcome ledger gains a TriggerKey index so `OnComplete` can stamp occurrence counts before delivery. The Slack notifier splits its blocks into a compact summary (channel) and a detail message (thread reply via `chat.postMessage` `ts`); the shared `notify.Format` and webhook payload gain the new sections so Matrix/webhook stay comprehensive.

**Tech Stack:** Go 1.26, stdlib only (no new deps). Tests: `go test -race ./...`, table-driven + `httptest`. Lint: `golangci-lint run ./...` (v2 config), `gofmt`.

## Global Constraints

- Go 1.26 (go.mod); CI runs `go build ./...`, `go vet ./...`, `test -z "$(gofmt -l .)"`, `go test -race ./...`.
- Never add AI attribution to commits or PRs. Commit messages use conventional-commit prefixes (`feat:`, `fix:`, `test:`, `docs:`) matching repo history.
- **mrkdwn-escape invariant** (`slack.go:176-182` + `TestSlackMessageFallbackEscaped`): the Slack fallback `text` is `escapeMrkdwn(Format(inv))` — any new scaffolding added to `Format` must contain NONE of `&`, `<`, `>`. Never put Slack `<!date^…>` tokens in `Format` output (they'd be escaped/corrupted); those are Slack-blocks-only.
- All untrusted (model/alert/tool) strings interpolated into Slack mrkdwn blocks go through `escapeMrkdwn`. Header blocks are `plain_text` — never escape those.
- Slack section text caps at 3000 chars — keep using `truncate(s, 2900)`. `fields` array: max 10 items, each ≤2000 chars.
- New `outcome.Event` fields must be `omitempty` and new event kinds must be ignored by old replay switches (append-only file format stays backward/forward compatible).
- Keep comment density/style of surrounding code (this repo comments the *why* heavily).

## Verdict vocabulary (used across tasks)

| enum value | emoji | label |
|---|---|---|
| `no_action` | ✅ | No action needed |
| `action_suggested` | 🛠 | Action suggested |
| `action_required` | 🔥 | Action required |
| `inconclusive` | ❓ | Inconclusive |

Empty verdict (old investigations, recall entries, non-conforming model) ⇒ omit verdict rendering everywhere; never invent one.

**Descoped (record in PR description as follow-ups):** live "still firing / resolved" status at send time (needs resolve-state race handling in the ledger); splitting next-steps into remediation vs alert-tuning (needs model-side classification of suggestions); regenerating the README screenshot (human task).

---

### Task 1: Verdict/RuledOut/DataGaps through the model contract

**Files:**
- Modify: `internal/providers/providers.go` (~line 388 `Investigation`, and add `Verdict` type above it)
- Modify: `internal/investigate/tools.go` (schema `:72-85`, `findings` `:105-133`, `buildInvestigation` `:185-214`)
- Modify: `internal/investigate/loop.go` (systemPrompt `:22-74`)
- Modify: `internal/investigate/budget.go` (synthetic results at `:16,31,46`)
- Modify: `internal/eval/score.go` (`investigationText` `:86-94`)
- Test: `internal/investigate/tools_test.go`, `internal/eval/score_test.go` (or wherever `investigationText` is covered)

**Interfaces:**
- Produces: `providers.Verdict` (string type) + constants `providers.VerdictNoAction|VerdictActionSuggested|VerdictActionRequired|VerdictInconclusive`; `Investigation.Verdict providers.Verdict`, `Investigation.RuledOut []string`, `Investigation.DataGaps []string`. All later tasks rely on these exact names.

- [ ] **Step 1: Write the failing test** — extend `TestParseFindings`-style coverage in `tools_test.go`:

```go
func TestParseFindingsVerdictRuledOutDataGaps(t *testing.T) {
	args := `{"title":"t","verdict":"no_action",
		"ruled_out":["plan deleted — plans still discovered in aws_backup_info"],
		"data_gaps":["CloudTrail truncated at 25 rows by SSM noise"],
		"root_causes":[{"summary":"s"}]}`
	inv, err := parseFindings(args)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if inv.Verdict != providers.VerdictNoAction {
		t.Fatalf("Verdict = %q, want no_action", inv.Verdict)
	}
	if len(inv.RuledOut) != 1 || len(inv.DataGaps) != 1 {
		t.Fatalf("RuledOut/DataGaps not mapped: %+v", inv)
	}
}

func TestParseFindingsUnknownVerdictNormalized(t *testing.T) {
	inv, err := parseFindings(`{"verdict":"looks_fine","root_causes":[{"summary":"s"}]}`)
	if err != nil {
		t.Fatalf("parseFindings: %v", err)
	}
	if inv.Verdict != "" {
		t.Fatalf("unknown verdict must map to empty, got %q", inv.Verdict)
	}
}
```

- [ ] **Step 2: Run to verify failure** — `go test ./internal/investigate/ -run TestParseFindingsVerdict -v` → FAIL (unknown fields / undefined `providers.VerdictNoAction`).

- [ ] **Step 3: Implement.** In `providers.go`, immediately above `Investigation`:

```go
// Verdict classifies an investigation's actionability for the humans reading the
// notification — the "do I need to do anything?" answer, separate from confidence
// (how sure the model is) and severity (how the alert was labelled).
type Verdict string

const (
	VerdictNoAction        Verdict = "no_action"        // benign / self-healed / synthetic; nothing to do
	VerdictActionSuggested Verdict = "action_suggested" // a human should follow the suggested next steps
	VerdictActionRequired  Verdict = "action_required"  // live impact; act promptly
	VerdictInconclusive    Verdict = "inconclusive"     // could not be determined with available data
)

// ValidVerdict reports whether v is one of the model-facing enum values; the
// parser normalizes anything else to "" so formatters can safely omit it.
func ValidVerdict(v Verdict) bool {
	switch v {
	case VerdictNoAction, VerdictActionSuggested, VerdictActionRequired, VerdictInconclusive:
		return true
	}
	return false
}
```

Add to `Investigation` (after `Unresolved`):

```go
	Verdict  Verdict  // model-classified actionability; "" when the model omitted it (rendered nowhere)
	RuledOut []string // hypotheses considered and rejected, one line each with the disproving evidence
	DataGaps []string // signals that could not be obtained (tool errors, missing metrics, truncation) — a data limitation, not a question for a human
```

In `tools.go` `findings` struct add `Verdict string \`json:"verdict"\``, `RuledOut []string \`json:"ruled_out"\``, `DataGaps []string \`json:"data_gaps"\``. In `buildInvestigation` map them, normalizing the verdict:

```go
	inv := providers.Investigation{Title: f.Title, Confidence: clamp01(f.Confidence),
		Unresolved: f.Unresolved, RuledOut: f.RuledOut, DataGaps: f.DataGaps}
	if v := providers.Verdict(f.Verdict); providers.ValidVerdict(v) {
		inv.Verdict = v
	}
```

In `submitFindingsSpec()` schema add, after `"unresolved"`:

```
"verdict":{"type":"string","enum":["no_action","action_suggested","action_required","inconclusive"],"description":"actionability for the on-call: no_action (benign/self-healed/synthetic), action_suggested (a human should follow the next steps), action_required (live impact needing prompt action), inconclusive (could not determine)"},
"ruled_out":{"type":"array","items":{"type":"string"},"description":"hypotheses you considered and REJECTED, one line each naming the disproving evidence"},
"data_gaps":{"type":"array","items":{"type":"string"},"description":"signals you could not obtain (tool errors, missing metrics, truncated output) that limited the investigation - data limitations, NOT questions for a human"},
```

and change the `"unresolved"` description to `"genuine open questions only a human can answer - put tool or data limitations in data_gaps instead"` (add a description key to it). Extend `"required"` to `["root_causes","verdict"]`.

In `loop.go` `systemPrompt`, append after the RIGOR block:

```
CLASSIFY the outcome in submit_findings "verdict": no_action (benign, self-healed, synthetic test,
or noise), action_suggested (a human should follow your next steps), action_required (live impact
needing prompt action), inconclusive. Separate honesty channels: "unresolved" is ONLY for questions a
human must answer; a tool error, missing metric, or truncated output goes in "data_gaps"; a hypothesis
you checked and disproved goes in "ruled_out" with the disproving evidence.
```

In `budget.go`, each synthetic Investigation gets `Verdict: providers.VerdictInconclusive` (match the existing struct-literal style at `:16,31,46`).

In `eval/score.go` `investigationText`, append RuledOut and DataGaps lines exactly like the existing Unresolved handling at `:92` (recall text only; leave `claimText` untouched).

- [ ] **Step 4: Run** — `go test ./internal/investigate/... ./internal/eval/... ./internal/providers/...` → PASS. `go vet ./...`.
- [ ] **Step 5: Commit** — `feat(investigate): add verdict, ruled_out and data_gaps to the findings contract`

---

### Task 2: Verify pass routes rejected hypotheses to RuledOut

**Files:**
- Modify: `internal/investigate/verify.go` (`applyVerdicts` `:111-158`)
- Test: `internal/investigate/verify_test.go`

**Interfaces:**
- Consumes: `Investigation.RuledOut`, `providers.VerdictInconclusive` (Task 1).
- Produces: rejected hypotheses land in `inv.RuledOut` (formatted `"<summary> — <reason>"`), no longer in `inv.Unresolved`; when the verify pass rejects every hypothesis, `inv.Verdict` becomes `VerdictInconclusive`.

- [ ] **Step 1: Failing test** in `verify_test.go` (follow the existing fixture style there — read the file first):

```go
// A rejected hypothesis is honesty about what was ruled out, not an open
// question for a human: it must land in RuledOut, and rejecting everything
// downgrades the verdict to inconclusive.
func TestApplyVerdictsRejectedGoesToRuledOut(t *testing.T) { /* build an inv with one
	hypothesis + a reject verdict via the existing test harness; assert:
	len(inv.RuledOut)==1 && strings.Contains(inv.RuledOut[0], "<summary>");
	no "Rejected hypothesis" entry in inv.Unresolved;
	inv.Verdict == providers.VerdictInconclusive when it was VerdictNoAction before */ }
```

- [ ] **Step 2: Run** — `go test ./internal/investigate/ -run TestApplyVerdicts -v` → FAIL.
- [ ] **Step 3: Implement** in `applyVerdicts`: replace the `:137` append-to-Unresolved (`"Rejected hypothesis: %s — %s"`) with `inv.RuledOut = append(inv.RuledOut, fmt.Sprintf("%s — %s", h.Summary, reason))` (keep whatever reason variable exists there), and where `inv.Verified = len(kept) > 0` is set (`:142`) add:

```go
	if len(kept) == 0 && inv.Verdict != "" {
		// Everything the model concluded was refuted by the adversarial pass — an
		// actionable verdict would be a confident claim with no surviving support.
		inv.Verdict = providers.VerdictInconclusive
	}
```

Update any existing verify tests asserting the old Unresolved wording.
- [ ] **Step 4: Run** — `go test ./internal/investigate/...` → PASS.
- [ ] **Step 5: Commit** — `feat(investigate): verify pass records rejected hypotheses in ruled_out`

---

### Task 3: Stamp alert metadata onto the Investigation

**Files:**
- Modify: `internal/providers/providers.go` (`Investigation`)
- Modify: `internal/investigate/loop.go` (stamping block `:491-501`)
- Modify: `internal/investigate/recall.go` (recall short-circuit ~`:231` — same stamping)
- Test: `internal/investigate/loop_test.go` (or `investigate_test.go`, wherever a completed-loop investigation is asserted)

**Interfaces:**
- Consumes: `investigate.Request` fields (`Severity`, `Environment`, `Labels`, `At`).
- Produces: `Investigation.Severity`, `.Environment`, `.Cluster`, `.Tenant`, `.AlertName string`, `.StartedAt time.Time` — Task 6/7 render these; Task 5 does not depend on them.

- [ ] **Step 1: Failing test**: drive the existing fake-model loop harness with a Request carrying `Severity: "warning"`, `Environment: "prod"`, `At: start`, `Labels: map[string]string{"alertname":"BackupJobsMissing","cluster":"sanofi-003","tenant":"sanofi-003"}`; assert the delivered investigation carries all six fields.
- [ ] **Step 2: Run** → FAIL (fields don't exist).
- [ ] **Step 3: Implement.** `Investigation` gains (grouped after `Resource`, with a doc comment noting they are trigger-time facts for notification rendering, empty for sources that lack them):

```go
	Severity    string    // alert severity label at trigger time
	Environment string    // deployment environment (prod/staging/…)
	Cluster     string    // alert "cluster" label
	Tenant      string    // alert "tenant" label
	AlertName   string    // triggering alert name (labels["alertname"]); "" for non-alert sources
	StartedAt   time.Time // incident start (alert startsAt / failure time)
```

In `loop.go` after the `inv.TriggerKey = req.TriggerKey` line:

```go
	// Trigger-time facts for the notification's metadata block: the model never
	// sees or sets these; they come verbatim from the alert.
	inv.Severity = req.Severity
	inv.Environment = req.Environment
	inv.Cluster = req.Labels["cluster"]
	inv.Tenant = req.Labels["tenant"]
	inv.AlertName = req.Labels["alertname"]
	inv.StartedAt = req.At
```

Apply the same stamping to the recall short-circuit result in `recall.go` (find where the recalled Investigation gets Fingerprint/TriggerKey and mirror it — factor a small `stampRequestFacts(inv *providers.Investigation, req Request)` helper in `loop.go` and call it from both sites so they cannot drift).
- [ ] **Step 4: Run** — `go test ./internal/investigate/...` → PASS.
- [ ] **Step 5: Commit** — `feat(investigate): carry alert metadata onto the investigation for notification rendering`

---

### Task 4: Ledger — TriggerKey occurrences index + feedback events

**Files:**
- Modify: `internal/outcome/ledger.go`
- Test: `internal/outcome/ledger_test.go`

**Interfaces:**
- Produces (Task 5/9 consume — exact signatures):
  - `Event` gains `TriggerKey string \`json:"trigger_key,omitempty"\``, `CuratedURL string \`json:"curated_url,omitempty"\``, `Verdict string \`json:"verdict,omitempty"\``.
  - `func (l *Ledger) Occurrences(triggerKey string) (n int, last time.Time, lastCuratedURL string)` — count of prior "open" events with that TriggerKey, the newest one's At and CuratedURL. Disabled ledger or empty key ⇒ `(0, time.Time{}, "")`.
  - `func (l *Ledger) Feedback(triggerKey, fingerprint, rating string, at time.Time) error` — appends `{"event":"feedback","trigger_key":…,"fingerprint":…,"kind":rating,"at":…}`; rating is `"up"` or `"down"` (validate, reject others).

- [ ] **Step 1: Failing tests** in `ledger_test.go` (follow existing temp-file test style):

```go
func TestOccurrencesByTriggerKey(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "ledger.jsonl"))
	t1 := time.Date(2026, 7, 1, 10, 0, 0, 0, time.UTC)
	t2 := t1.Add(6 * time.Hour)
	_ = l.Open(Event{Fingerprint: "f1", TriggerKey: "k", CuratedURL: "https://kb/1", At: t1})
	_ = l.Open(Event{Fingerprint: "f2", TriggerKey: "k", CuratedURL: "https://kb/2", At: t2})
	_ = l.Open(Event{Fingerprint: "f3", TriggerKey: "other", At: t2})
	n, last, url := l.Occurrences("k")
	if n != 2 || !last.Equal(t2) || url != "https://kb/2" {
		t.Fatalf("Occurrences = %d %v %q", n, last, url)
	}
	// index survives a replay (restart)
	l2, _ := New(l.path)
	if n, _, _ := l2.Occurrences("k"); n != 2 {
		t.Fatalf("after replay: %d", n)
	}
}

func TestFeedbackAppendsAndIsIgnoredByReplay(t *testing.T) {
	// Feedback must not disturb open/resolve pairing or OpenCounts.
	// Append open + feedback + resolve; assert Episodes() still pairs 1 episode
	// and Feedback("k","f","sideways",…) returns an error.
}
```

(`l.path` is unexported — inside the same package the test may use it, matching existing tests; check and mirror.)
- [ ] **Step 2: Run** — `go test ./internal/outcome/ -v` → FAIL.
- [ ] **Step 3: Implement.** Add fields to `Event`. Add to `Ledger` struct:

```go
	// byTrigger indexes "open" events per TriggerKey so delivery can render
	// "Nth occurrence — previous: <KB link>" without replaying the file. Maintained
	// in lockstep with the durable write, rebuilt on load, like agg.
	byTrigger map[string]triggerAgg
```

```go
// triggerAgg is the per-TriggerKey occurrence roll-up backing Occurrences.
type triggerAgg struct {
	count      int
	last       time.Time
	curatedURL string // CuratedURL of the newest open
}
```

In `loadLocked`: reset `l.byTrigger = map[string]triggerAgg{}` and fold `case "open"` through a new `applyTriggerLocked(e)`; call the same from `Open` after `applyOpenLocked(e)`:

```go
// applyTriggerLocked folds one open into the per-TriggerKey occurrence index.
func (l *Ledger) applyTriggerLocked(e Event) {
	if e.TriggerKey == "" {
		return
	}
	a := l.byTrigger[e.TriggerKey]
	a.count++
	if !e.At.Before(a.last) {
		a.last = e.At
		a.curatedURL = e.CuratedURL
	}
	l.byTrigger[e.TriggerKey] = a
}
```

```go
// Occurrences reports how many investigations have been recorded for a
// TriggerKey, when the most recent one happened, and its KB link — the
// recurrence facts the notifier renders. Zero values for a disabled ledger,
// an empty key, or a never-seen key.
func (l *Ledger) Occurrences(triggerKey string) (int, time.Time, string) {
	if !l.enabled() || triggerKey == "" {
		return 0, time.Time{}, ""
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	a := l.byTrigger[triggerKey]
	return a.count, a.last, a.curatedURL
}
```

```go
// Feedback appends a human 👍/👎 verdict on a delivered investigation. It is a
// pure append: replay ignores unknown event kinds, so feedback never disturbs
// open/resolve pairing or the recall aggregate.
func (l *Ledger) Feedback(triggerKey, fingerprint, rating string, at time.Time) error {
	if !l.enabled() {
		return nil
	}
	if rating != "up" && rating != "down" {
		return fmt.Errorf("feedback rating %q: want up or down", rating)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.appendLocked(Event{Event: "feedback", TriggerKey: triggerKey, Fingerprint: fingerprint, Kind: rating, At: at})
}
```

(`fmt` import.) The `loadLocked`/`Episodes` switches already ignore unknown event kinds — verify, don't change.
- [ ] **Step 4: Run** — `go test ./internal/outcome/... -race` → PASS.
- [ ] **Step 5: Commit** — `feat(outcome): index opens by trigger key and record human feedback events`

---

### Task 5: OnComplete — stamp recurrence, reorder curate before ledger open

**Files:**
- Modify: `internal/providers/providers.go` (`Investigation`: recurrence fields)
- Modify: `internal/app/investigate.go` (`OnComplete` closure `:291-365`)
- Test: `internal/app/` (find the existing test that exercises `BuildInvestigator`'s OnComplete — if none covers ordering, add `internal/app/oncomplete_test.go` with a fake notifier + temp-file ledger)

**Interfaces:**
- Consumes: `Ledger.Occurrences` (Task 4), `Investigation.TriggerKey`.
- Produces: `Investigation.Occurrences int` (1 = first time, N = this is the Nth), `Investigation.LastOccurrence time.Time`, `Investigation.PrevCuratedURL string`. Open events now carry `TriggerKey`, `CuratedURL`, `Verdict`.

- [ ] **Step 1: Failing test**: with a real temp-file ledger pre-seeded with one open for TriggerKey "k" (CuratedURL "https://kb/prev", At = 4h ago), run the OnComplete closure with an investigation carrying TriggerKey "k"; assert the notifier received `Occurrences == 2`, `PrevCuratedURL == "https://kb/prev"`, and the ledger's newly appended open event has `TriggerKey == "k"` and the fresh `CuratedURL`.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** `Investigation` gains:

```go
	Occurrences    int       // Nth recorded investigation of this TriggerKey (1 = first); 0 = unknown/ledger disabled
	LastOccurrence time.Time // when the previous occurrence was investigated
	PrevCuratedURL string    // the previous occurrence's KB link, for the "same conclusion as before" pointer
```

Rework `OnComplete` order to: **(1) query recurrence → (2) curate → (3) ledger opens → (4) actions → (5) deliver**. Concretely:

```go
	// Recurrence facts BEFORE recording this run's own open, so the count and
	// "previous" pointer describe prior investigations only.
	if n, last, url := ledger.Occurrences(found.TriggerKey); n > 0 {
		found.Occurrences = n + 1
		found.LastOccurrence = last
		found.PrevCuratedURL = url
	} else if found.TriggerKey != "" {
		found.Occurrences = 1
	}
```

Move the existing `cur.Curate` block **above** the ledger-open loop (updating its "Curate first so the delivered message can link to the KB issue/PR" comment to also say the open event stores the link for recurrence pointers), then extend the open event literal with:

```go
			TriggerKey: found.TriggerKey,
			CuratedURL: found.CuratedURL,
			Verdict:    string(found.Verdict),
```

Keep the action-handling switch where it is (before deliver). Note in the moved comment why curate now precedes the outcome open: the open event is the durable record of *this* investigation and must carry the KB link that recurrence pointers and the learning loop read.
- [ ] **Step 4: Run** — `go test ./internal/app/... -race` → PASS.
- [ ] **Step 5: Commit** — `feat(app): stamp recurrence facts and persist trigger key + KB link on outcome opens`

---

### Task 6: Shared Format() + webhook payload — verdict, metadata, ruled-out, data-gaps, recurrence

**Files:**
- Modify: `internal/notify/format.go`
- Modify: `internal/notify/webhook/webhook.go` (`payload` `:44-51`, `Deliver`)
- Test: `internal/notify/format_test.go`, `internal/notify/webhook/webhook_test.go`, `internal/notify/matrix_test.go` (only if its fixtures break)

**Interfaces:**
- Produces: `verdictBadge(v providers.Verdict) (emoji, label string)` in `format.go` (package `notify`, exported to nothing — same-package Slack code uses it in Task 7). Format output gains lines: `Verdict: <label>`, `Alert: … · severity … · cluster … · tenant …`, `Started: <RFC3339>`, `Occurrence: #N (last …; previous …)`, `*Ruled out:*`, `*Data gaps:*` sections.

- [ ] **Step 1: Failing tests** in `format_test.go` — extend `sampleInvestigation()` with the new fields and assert each new line renders, plus omission cases (zero verdict ⇒ no "Verdict:" line; Occurrences ≤ 1 ⇒ no "Occurrence:" line; empty slices ⇒ no sections). **Constraint check test**: assert the composed output of a fully-populated investigation contains none of `& < >` beyond what evidence strings inject (guards the fallback-escape invariant).
- [ ] **Step 2: Run** — `go test ./internal/notify/ -run TestFormat -v` → FAIL.
- [ ] **Step 3: Implement** in `format.go`:

```go
// verdictBadge maps a model verdict to its emoji + human label. Empty/unknown
// verdicts return ("", "") and are rendered nowhere — never invent a verdict.
func verdictBadge(v providers.Verdict) (emoji, label string) {
	switch v {
	case providers.VerdictNoAction:
		return "✅", "No action needed"
	case providers.VerdictActionSuggested:
		return "🛠", "Action suggested"
	case providers.VerdictActionRequired:
		return "🔥", "Action required"
	case providers.VerdictInconclusive:
		return "❓", "Inconclusive"
	}
	return "", ""
}
```

In `Format`, after the confidence line: verdict line (`fmt.Fprintf(&b, "%s Verdict: %s\n", emoji, label)` when label ≠ ""); after the Resource line, a compact metadata line assembled from non-empty parts joined with " · " (`Alert: <AlertName>`, `severity <Severity>`, `env <Environment>`, `cluster <Cluster>`, `tenant <Tenant>`); `Started: <StartedAt RFC3339>` when non-zero; recurrence line when `Occurrences > 1`:

```go
		fmt.Fprintf(&b, "Occurrence: #%d — last investigated %s\n", inv.Occurrences, inv.LastOccurrence.UTC().Format(time.RFC3339))
		if inv.PrevCuratedURL != "" {
			fmt.Fprintf(&b, "Previous conclusion: %s\n", inv.PrevCuratedURL)
		}
```

After the Unresolved section, two new sections mirroring its shape: `*Ruled out:*` over `inv.RuledOut` and `*Data gaps:*` over `inv.DataGaps`. (Plain `·`/`•` and `*` only — no `&<>`.)

In `webhook.go`, extend `payload`:

```go
	Verdict        string   `json:"verdict,omitempty"`
	Severity       string   `json:"severity,omitempty"`
	Cluster        string   `json:"cluster,omitempty"`
	Tenant         string   `json:"tenant,omitempty"`
	AlertName      string   `json:"alert_name,omitempty"`
	StartedAt      string   `json:"started_at,omitempty"` // RFC3339; "" when unknown
	Occurrences    int      `json:"occurrences,omitempty"`
	PrevCuratedURL string   `json:"prev_curated_url,omitempty"`
	RuledOut       []string `json:"ruled_out,omitempty"`
	DataGaps       []string `json:"data_gaps,omitempty"`
```

populated in `Deliver` (`StartedAt` via `inv.StartedAt.UTC().Format(time.RFC3339)` guarded by `!inv.StartedAt.IsZero()`). Extend `TestDeliverPOSTsJSON` for the new keys.
- [ ] **Step 4: Run** — `go test ./internal/notify/... -race` → PASS (fix matrix fixtures if the richer `sampleInvestigation` breaks assertions).
- [ ] **Step 5: Commit** — `feat(notify): render verdict, alert metadata, recurrence, ruled-out and data-gaps in the shared format`

---

### Task 7: Slack layout overhaul — summary/detail blocks, fields, compact fallback

**Files:**
- Modify: `internal/notify/slack.go` (`slackMessage` `:175-183`, `slackBlocks` `:231-317`, add `summaryBlocks`/`detailBlocks`/`metadataFields`/`slackDate`)
- Test: `internal/notify/slack_test.go`

**Interfaces:**
- Consumes: `verdictBadge` (Task 6), all Investigation fields (Tasks 1-5).
- Produces (Task 8 consumes): `summaryBlocks(inv providers.Investigation) []map[string]any` and `detailBlocks(inv providers.Investigation) []map[string]any` (detail returns nil when there is nothing beyond the summary). `slackMessage(inv)` becomes the **single-message** composition `append(summaryBlocks, detailBlocks...)` used by the webhook path. `fallbackText(inv providers.Investigation) string` — one line: `🔍 <title> — <verdict label> (<pct>% confidence)`.

**Summary layout (top-down triage order):**
1. `header` (plain_text): `🔍 ` + `inv.AlertName` when set (else Title), truncated 150. When AlertName is set, append ` — ` + first non-empty of Tenant/Cluster + `/` + `Resource.Ref()`.
2. Verdict `section` (mrkdwn): `{emoji} *{label}* — {escapeMrkdwn(Title)}` truncated 2900 (only when verdict non-empty; when empty, fall back to the current confidence context line as the second block).
3. `section` with `"fields"` (only non-empty entries, ≤10): `*Alert:*\n<name> (<severity>)`, `*Cluster:*\n<tenant · cluster>`, `*Resource:*\n<Kind Ref()>`, `*Started:*\n<slackDate(StartedAt)>`, `*What changed:*\n<top root cause ChangeRef, escaped, truncated 200, or "none">`, `*Recurrence:*\n🔁 #<N> · last <slackDate(LastOccurrence)>` (only when Occurrences > 1).
4. Top root cause `section`: `*Why:* {escaped summary}` + up to 3 evidence bullets (escaped). When >1 root causes: trailing `_…full analysis in thread_` context hint (in Task 8 wording; for now `_…{n-1} more hypotheses below_`).
5. Next-steps section — reuse the existing `nextSteps(inv)`, cap at 3 with `_…N more_`.
6. `❌ *Ruled out:*` one section, items joined with `\n• ` (cap 3, `_…N more_`) — only when non-empty.
7. `context`: `⚠️ Data gaps: ` + items joined `" · "`, escaped, truncated (only when non-empty).
8. Recurrence pointer `context` when `PrevCuratedURL != ""`: `🔁 Previously investigated — <{escapeMrkdwn(PrevCuratedURL)}|previous conclusion>`.
9. Footer `context`: `{confEmoji} {level} confidence · {pct}%` + ` · ✓ verified` when `inv.Verified` + ` · 🤖 RunLore SRE agent` + KB link (`📚 <url|view entry>`) + usage one-liner (`usageFooter`), joined with `  ·  `, truncated 2900. **This replaces** the old top confidence context line, the separate KB context block, and the fallback's usage footer position — confidence moves to the bottom, verdict owns the top.
10. Approve/Reject actions blocks (existing code, unchanged) + feedback buttons (Task 9 adds them here).

**Detail layout** (`detailBlocks`): header `section` `*Full analysis*`; every root cause rendered as today's per-RC section but with **all** evidence (still `truncate(…, 2900)` per section); full `*❓ Open questions*` section (all items); full `*⚠️ Data gaps:*` section (all items, bullets); full `*❌ Ruled out:*` (all items). Returns nil when `len(inv.RootCauses) ≤ 1 && Unresolved/DataGaps/RuledOut` all fit the summary caps (keep the rule simple: nil only when all three slices are empty and ≤1 root cause with ≤3 evidence — otherwise emit).

**Helpers:**

```go
// slackDate renders t as a Slack date token that displays in the reader's local
// timezone, with the RFC3339 UTC form as the no-JS fallback. Slack-blocks-only:
// the token uses raw <>, so it must never enter the escaped fallback text.
func slackDate(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return fmt.Sprintf("<!date^%d^{date_short_pretty} {time}|%s>", t.Unix(), t.UTC().Format(time.RFC3339))
}
```

`fallbackText`: verdict label from `verdictBadge`; omit the ` — <label>` part when empty; pct from `confidenceBadge`.

```go
func fallbackText(inv providers.Investigation) string {
	title := inv.Title
	if title == "" {
		title = "Investigation"
	}
	_, level, pct := confidenceBadge(inv)
	s := "🔍 " + title
	if _, label := verdictBadge(inv.Verdict); label != "" {
		s += " — " + label
	}
	return escapeMrkdwn(fmt.Sprintf("%s (%s confidence · %d%%)", s, level, pct))
}
```

`slackMessage` becomes `{"text": fallbackText(inv), "blocks": append(summaryBlocks(inv), detailBlocks(inv)...)}`.

- [ ] **Step 1: Failing tests** (extend `slack_test.go`; use the `mrkdwnTexts` helper):
  - `TestSlackSummaryLayout`: fully-populated investigation → first block header contains AlertName; second block contains `*No action needed*`; a fields section contains `*Cluster:*` and `*Recurrence:*`; footer context contains `confidence` and `view entry`; verify the OLD top-of-message confidence context line is gone (block[1] is not a context block).
  - `TestSlackFallbackOneLine`: `slackMessage(inv)["text"]` has no `\n` and contains the verdict label; hostile title still escaped (`&lt;`).
  - `TestSlackDetailBlocksFullEvidence`: 6-evidence root cause → summary shows 3 + `…`, detail blocks contain all 6.
  - `TestSlackNoVerdictFallsBack`: verdict "" → no `*No action needed*` anywhere, layout still renders confidence.
  - Keep/extend `TestSlackBlocksEscapeUntrustedText` with the new untrusted surfaces (ChangeRef in fields, DataGaps, RuledOut, PrevCuratedURL).
  - Update `TestSlackBlocksLayout`, `TestSlackMessageFallbackEscaped` (fallback is now `fallbackText`, not `Format` — the test's invariant shifts: hostile **title** must be escaped) and `TestSlackDeliver`/`TestSlackBotDeliver` content expectations.
- [ ] **Step 2: Run** — `go test ./internal/notify/ -v` → FAIL.
- [ ] **Step 3: Implement** per the layout above. Update the `slackMessage`/`slackBlocks` doc comments (the escape-invariant comment at `:176-182` now describes `fallbackText`'s per-field escaping instead of whole-Format escaping).
- [ ] **Step 4: Run** — `go test ./internal/notify/... -race` → PASS. `golangci-lint run ./internal/notify/...`.
- [ ] **Step 5: Commit** — `feat(notify): verdict-first slack layout with metadata fields and compact fallback`

---

### Task 8: Slack threading — detail as a thread reply on the bot path

**Files:**
- Modify: `internal/notify/slack.go` (`SlackBot.post` `:124-162`, `SlackBot.Deliver` `:112-114`)
- Test: `internal/notify/slack_test.go`

**Interfaces:**
- Consumes: `summaryBlocks`/`detailBlocks`/`fallbackText` (Task 7).
- Produces: `func (s *SlackBot) post(ctx context.Context, msg map[string]any) (string, error)` — returns the posted message's `ts` ("" for empty-body 2xx). Webhook `Slack.Deliver` keeps the single combined message (unchanged from Task 7).

- [ ] **Step 1: Failing test**:

```go
func TestSlackBotDeliverThreadsDetail(t *testing.T) {
	// httptest server: first POST returns {"ok":true,"ts":"111.222"}; record each
	// request body. Deliver a multi-hypothesis investigation.
	// Assert: 2 POSTs; first has no "thread_ts" and its blocks contain the verdict
	// summary; second has "thread_ts":"111.222" and its blocks contain "Full analysis";
	// second's fallback text is short ("Full analysis" + title).
}

func TestSlackBotDeliverNoThreadWhenNoDetail(t *testing.T) {
	// minimal investigation (detailBlocks nil) → exactly 1 POST.
}

func TestSlackBotDeliverDetailBestEffort(t *testing.T) {
	// second POST returns {"ok":false,"error":"ratelimited"} → Deliver still
	// returns nil (summary delivered = notification delivered); or assert the
	// error is returned — DECISION: best-effort, return nil, matching the
	// progress-ping precedent that a secondary message never fails delivery.
}
```

- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.** Change `SlackBot.post` to return `(string, error)`: extend the decoded result struct with `TS string \`json:"ts"\``, return `result.TS, nil` on success ("" on the empty-body path). Update `DeliverProgress` call site (`_, err := s.post(…)`). Rewrite `SlackBot.Deliver`:

```go
// Deliver posts the compact summary to the channel, then the full analysis as a
// thread reply so the channel stays a scannable triage feed. The thread post is
// best-effort: the summary IS the notification; a failed detail reply is logged
// by the caller's Multi wrapper, never surfaced as a delivery failure.
func (s *SlackBot) Deliver(ctx context.Context, inv providers.Investigation) error {
	ts, err := s.post(ctx, map[string]any{"text": fallbackText(inv), "blocks": summaryBlocks(inv)})
	if err != nil {
		return err
	}
	detail := detailBlocks(inv)
	if ts == "" || len(detail) == 0 {
		return nil
	}
	msg := map[string]any{"text": "Full analysis: " + escapeMrkdwn(truncate(inv.Title, 120)), "blocks": detail, "thread_ts": ts}
	if _, err := s.post(ctx, msg); err != nil {
		return fmt.Errorf("slack detail thread (summary delivered): %w", err)
	}
	return nil
}
```

**Decision locked in:** a detail-thread failure returns a wrapped error (so `Multi` logs it) — the wrapped message makes clear the summary landed. The webhook `Slack.Deliver` stays `s.post(ctx, slackMessage(inv))` (combined blocks).
- [ ] **Step 4: Run** — `go test ./internal/notify/... -race` → PASS.
- [ ] **Step 5: Commit** — `feat(notify): thread the full analysis under a compact slack summary`

---

### Task 9: 👍/👎 feedback buttons → outcome ledger

**Files:**
- Modify: `internal/notify/slack.go` (constants `:164-168`, feedback actions block in `summaryBlocks`)
- Modify: `internal/server/server.go` (`handleSlackInteraction` `:239-304`, `Server` struct + constructor)
- Modify: `internal/app/serve.go` (wire the ledger into the server, ~`:155-160` where the ledger is built)
- Test: `internal/notify/slack_test.go`, `internal/server/server_test.go` (find the existing slack-interaction signature-test helpers and reuse them)

**Interfaces:**
- Consumes: `Ledger.Feedback` (Task 4).
- Produces: action_ids `runlore_feedback_up` / `runlore_feedback_down`; button `value` = `inv.TriggerKey`, falling back to `inv.Fingerprint` (buttons omitted when both empty). Server gains a `feedback` field of interface type `FeedbackRecorder`:

```go
// FeedbackRecorder persists a human 👍/👎 on a delivered investigation
// (implemented by *outcome.Ledger).
type FeedbackRecorder interface {
	Feedback(triggerKey, fingerprint, rating string, at time.Time) error
}
```

- [ ] **Step 1: Failing tests**:
  - notify: `TestSlackFeedbackButtons` — investigation with TriggerKey "k" → summary blocks contain an actions block with both feedback action_ids and `"value":"k"`; investigation with neither TriggerKey nor Fingerprint → no feedback block. Feedback buttons render even when no ApprovalID actions exist.
  - server: `TestSlackFeedbackInteraction` — signed interaction payload with `action_id: "runlore_feedback_up"`, value "k" → 200, recorder called with `("k", "", "up")`, works with `approvals == nil`; user NOT in the approver allowlist still succeeds (feedback is unprivileged); `runlore_approve` with `approvals == nil` returns a "not enabled" message rather than 404.
- [ ] **Step 2: Run** → FAIL.
- [ ] **Step 3: Implement.**
  - `slack.go`: constants `feedbackUpActionID = "runlore_feedback_up"`, `feedbackDownActionID = "runlore_feedback_down"`. At the end of `summaryBlocks`:

```go
	// Human feedback on the verdict — ground truth for the learning loop. Keyed by
	// TriggerKey (incident identity) so ratings survive re-worded re-investigations.
	if key := cmp.Or(inv.TriggerKey, inv.Fingerprint); key != "" {
		blocks = append(blocks, map[string]any{"type": "actions", "elements": []map[string]any{
			{"type": "button", "action_id": feedbackUpActionID, "value": key,
				"text": map[string]any{"type": "plain_text", "text": "👍 Accurate", "emoji": true}},
			{"type": "button", "action_id": feedbackDownActionID, "value": key,
				"text": map[string]any{"type": "plain_text", "text": "👎 Off-base", "emoji": true}},
		}})
	}
```

  - `server.go`: relax the guard to `if (s.approvals == nil && s.feedback == nil) || s.slackSecret == ""`; in the approve/reject cases, guard `s.approvals == nil` → `msg = "❌ approvals not enabled"`. New cases:

```go
	case "runlore_feedback_up", "runlore_feedback_down":
		if s.feedback == nil {
			msg = "⚠️ feedback recording not enabled (no outcome ledger configured)"
			break
		}
		rating := "up"
		if act.ActionID == "runlore_feedback_down" {
			rating = "down"
		}
		if ferr := s.feedback.Feedback(act.Value, "", rating, time.Now()); ferr != nil {
			msg = "⚠️ recording feedback failed: " + ferr.Error()
			s.log.Warn("slack feedback failed", "key", act.Value, "err", ferr)
		} else {
			msg = fmt.Sprintf("🙏 feedback recorded (%s) — thanks @%s", rating, p.User.Username)
			s.log.Info("slack feedback recorded", "key", act.Value, "rating", rating, "user", p.User.Username)
		}
```

    **Important:** feedback must NOT `replace_original` (that would wipe the investigation message). `updateSlack` hardcodes `replace_original: true` — add a boolean param (or a sibling `appendSlack`) posting `{"replace_original": false, "response_url" …, "text": msg}` for the feedback cases; approve/reject keep replacing.
  - Wire: follow how `approvals`/`slackSecret` reach the `Server` (constructor/opts in `server.go` + call site in `internal/app/serve.go`) and pass the built `*outcome.Ledger` the same way. A nil ledger stays a nil interface — guard with a typed-nil check at the wiring site (`if ledger != nil { opts.Feedback = ledger }`).
- [ ] **Step 4: Run** — `go test ./internal/server/... ./internal/notify/... ./internal/app/... -race` → PASS.
- [ ] **Step 5: Commit** — `feat(feedback): slack 👍/👎 buttons record investigation feedback in the outcome ledger`

---

### Task 10: Docs

**Files:**
- Modify: `README.md` (`:39-44` layout caption, `:107` notifier table)
- Modify: `docs/configuration.md` (`:171-173` notify section)
- Modify: `docs/getting-started.md` (`:282-293` notify YAML, `:466,502-503` interactions notes)
- Modify: `docs/learning-loop.md` (outcome ledger: new feedback events + trigger-key index)
- Check: `hack/demo.config.yaml` (no schema change expected — confirm)

**Interfaces:** none (prose only).

- [ ] **Step 1:** Update README caption to describe the new layout (verdict-first summary + threaded full analysis + feedback buttons) and add a note that the screenshot predates the layout (regenerating it is a human follow-up). Update the notifier table: Slack bot = threaded summary/detail; webhook Slack + Matrix + generic webhook = single comprehensive message.
- [ ] **Step 2:** configuration.md/getting-started.md: document that feedback buttons require `signing_secret_env` + the `/slack/interactions` Request URL (same setup as approvals) and that ratings land in the outcome ledger (`outcome.ledger_path`). Document the new webhook JSON payload fields (verdict, severity, cluster, tenant, alert_name, started_at, occurrences, prev_curated_url, ruled_out, data_gaps).
- [ ] **Step 3:** learning-loop.md: describe `feedback` ledger events and the TriggerKey occurrence index.
- [ ] **Step 4:** `go build ./... && go test -race ./... && test -z "$(gofmt -l .)" && go vet ./...` — full suite green.
- [ ] **Step 5: Commit** — `docs: document verdict-first notifications, threading and feedback buttons`

---

## Self-Review Notes

- Spec coverage: verdict line (T1/6/7), confidence demotion (T7 footer), threading (T8), open-questions split into data gaps (T1/6/7), metadata fields block (T3/6/7), recurrence (T4/5/6/7), ruled-out (T1/2/6/7), fallback one-liner (T7), ChangeRef out of backticks (T7 fields render), copyable commands — covered by keeping mrkdwn code-block passthrough? NO: commands arrive as prose inside suggestions; wrapping them is model-output-dependent — dropped as unimplementable formatter-side, noted in PR description instead. Timestamps `<!date^` (T7), feedback buttons (T9), docs (T10).
- Type consistency: `Verdict` is `providers.Verdict` on Investigation but `string` in `outcome.Event` (deliberate: the ledger is a serialization boundary). `post` returns `(string, error)` — `DeliverProgress` call sites updated in T8.
- Ordering hazard: T5 reorders curate before ledger-open — the failing test must pin the new order (curated URL present on the open event).
