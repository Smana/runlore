# RunLore Outcome Capture — Design

| | |
|---|---|
| **Status** | Design `v0.1` — approved for implementation planning |
| **Date** | 2026-06-23 |
| **Scope** | Slice **A1** of the learning-loop "outcome loop": ingest the resolved-alert signal, attribute it to the answer that was used (recall vs fresh), and record it in a persistent outcome ledger. Makes learning *measurable*. No feedback/decay (A2) or dormant-pass wiring (A3) yet. |
| **Author** | Smana (brainstormed with Claude) |
| **Related** | The "accumulates ≠ learns" critique (problem #1, fix #4); slice 1 recall trustworthiness (#68); `internal/trigger/incident.go` (`ParseAlertmanager`), `internal/investigate` (`Request`/loop/`recall`), `internal/curate` (dormant `ResolutionChecker`/`Queue` — future A3 consumers) |

---

## 1. Why this exists

The critique's #1, the make-or-break: *"accumulates ≠ learns — no outcome signal anywhere."* RunLore curates and recalls knowledge but never records whether a recalled/curated answer was followed by the incident actually **resolving**. So nothing can validate a recall, decay a stale entry, or compound. This slice closes the **first half** of the loop: capture the outcome signal + attribute it to the answer used. (A2 feeds it back into recall ranking; A3 wires the dormant curate lifecycle.)

It builds directly on slice 1: recall now records *which* entry it matched and emits `recall_hits{result}` / `recall_rejections`. This slice adds the **resolution side** — when the same alert that triggered an investigation resolves, record it against the entry/answer used. Alone, it already kills critique #1 by making "did it actually work?" a queryable fact.

## 2. Decisions locked during brainstorming

| # | Decision | Rationale |
|---|---|---|
| D1 | **First slice = outcome CAPTURE** (signal + attribution + record); feedback/decay = A2; dormant-pass wiring = A3 | The outcome loop is too big for one spec. Capture is the foundation everything else reads, and on its own it makes "did it work?" visible — killing critique #1. |
| D2 | **Resolution signal = Alertmanager `resolved` webhook** (currently discarded) | Event-driven, low cost, and the AM `fingerprint` is **stable firing↔resolved** → clean attribution. A cluster-state `ResolutionChecker` is A3. |
| D3 | **Persistent append-only JSONL event ledger**, keyed by AM fingerprint, at an explicit configurable path (`outcome.ledger_path`; empty disables — the default) | Survives restart + leader failover; cheap per-event writes; full history. A2 reconstructs episodes; git-versioned surfacing onto entries is A2. **Explicit opt-in** — NOT auto-derived from `catalog.dir`, which may be a read-only ConfigMap mount; operators point it at a writable path such as the git-sync mirror PV. |
| D4 | **Attribute recalls cleanly** (`kind=recall` + entry path); **fresh investigations = `kind=fresh`, no entry link** | Recall validation is the high-value, clean case; linking a fresh finding to its eventual curated entry is murkier and belongs in A2. |
| D5 | **Recurrence is *inferable from the ledger*** (multiple opens per fingerprint), not a separate A1 mechanism | Keep A1 to event capture; the dormant Recurrence pass / A2 interpret it. |

## 3. Design

### 3.1 The outcome ledger (`internal/outcome`, NEW)

One small package. An append-only **JSONL event log** on disk plus an in-memory **open-index** (fingerprint → latest unresolved open) rebuilt by replaying the file on startup, so a `resolve` can be matched to its `open` (for duration + kind) and survive failover.

```go
type Event struct {
	Event       string    `json:"event"`             // "open" | "resolve"
	Fingerprint string    `json:"fingerprint"`        // Alertmanager fingerprint (stable firing↔resolved)
	Kind        string    `json:"kind,omitempty"`     // open: "recall" | "fresh"
	Entry       string    `json:"entry,omitempty"`    // open+recall: the recalled entry Path
	Title       string    `json:"title,omitempty"`
	Resource    string    `json:"resource,omitempty"`
	At          time.Time `json:"at"`
}

type Ledger struct { /* path string; mu sync.Mutex; open map[string]Event */ }

func New(path string) (*Ledger, error)                       // replays the file → open-index; nil-safe when path == ""
func (l *Ledger) Open(ctx context.Context, e Event) error    // append "open"; index[fp] = e
func (l *Ledger) Resolve(ctx context.Context, fp string, at time.Time) (Episode, bool, error) // append "resolve"; match+drop index[fp]
func (l *Ledger) Episodes() []Episode                         // (A2) reconstructed open→resolve pairs
```

- **Append** = `O_APPEND` open, write one JSON line + `\n`, close (mutex-guarded). Cheap; full history.
- **`Resolve`** finds `open[fp]`, returns the matched `Episode{Kind, Entry, OpenedAt, ResolvedAt, Duration}` + `ok`, appends the `resolve` line, drops the index entry. No matching open → append anyway (a resolve we never saw fire) and return `ok=false`.
- **Path** configurable (`outcome.ledger_path`; default `<catalog mirror dir>/outcomes.jsonl`). Empty ⇒ a no-op ledger (feature off).

### 3.2 Resolved-webhook ingestion (`incident.go`, `server.go`)

- `ParseAlertmanager` today drops non-firing: `if a.Status != "" && a.Status != "firing" { continue }`. Change it to **return both** firing and resolved alerts, tagging each with its status — add `Status string` to `config.Incident` (already carries `Fingerprint`).
- `handleAlertmanager` routes per incident: **firing** → the existing `Decide` → investigate/coalesce path (unchanged); **resolved** → `ledger.Resolve(inc.Fingerprint, inc.At)` — no `Decide`, no coalesce, no investigation. Reuses the existing webhook bearer-token auth.

### 3.3 Attribution (`Request`/`Investigation` threading)

The fingerprint must reach the point where the outcome is recorded (`OnComplete`):
- `config.Incident.Fingerprint` exists → add `Fingerprint` to `investigate.Request` (set in `FromIncident`) and to `providers.Investigation` (carried through so `OnComplete` can read it).
- Add `RecalledEntry string` to `providers.Investigation`, set in `recalledInvestigation` to the matched entry's `Path` (empty for fresh investigations). `Recalled bool` already exists (slice 1).
- At **`OnComplete`** (where curation already hooks), when the ledger is enabled, append `ledger.Open({Fingerprint, Kind: recall|fresh, Entry: RecalledEntry, Title, Resource, At: now})`. `Kind = "recall"` if `inv.Recalled` else `"fresh"`.

### 3.4 Metrics (`telemetry.Metrics`)

- `outcomes_opened_total{kind}` — at `OnComplete`.
- `incidents_resolved_total` + `incident_resolution_seconds` (histogram) — at `Resolve`, when it matched an open (duration = resolved − opened).
- `recall_outcome_total{result="resolved"}` — at `Resolve`, when the matched open's `kind == "recall"`. (`unresolved` is the absence of a resolve — inferred from the ledger by A2, not emitted here.)

### 3.5 Config + Helm

`outcome.ledger_path` — an **explicit** path; **empty disables** the feature (the default). Not auto-derived from `catalog.dir`, because that path may be a read-only ConfigMap mount; operators set it to a writable location (e.g. the git-sync mirror PV). Documented in `deploy/helm/runlore/values.yaml`.

Note: `OnComplete` only records an `open` when the investigation carries an alert `Fingerprint` — non-alert sources (GitOps-failure watch, reinvestigate poller) are skipped, since a resolved-alert webhook could never match them.

## 4. Components / seams

| Change | Location |
|---|---|
| Ledger package (append-only JSONL + replayed open-index) | `internal/outcome/` (`ledger.go`, `ledger_test.go`) |
| Parse + route resolved alerts | `internal/trigger/incident.go`, `internal/server/server.go` |
| Thread fingerprint + recalled-entry | `internal/config/config.go` (`Incident.Status`), `internal/investigate` (`Request.Fingerprint`, `FromIncident`, `recalledInvestigation`), `internal/providers/providers.go` (`Investigation.Fingerprint`, `.RecalledEntry`) |
| Record `Open` at OnComplete | `cmd/lore/main.go` (OnComplete wiring) |
| Metrics | `internal/telemetry/metrics.go` |
| Config + Helm | `internal/config/config.go`, `deploy/helm/runlore/values.yaml` |

## 5. Trade-offs accepted in v1

- **Correlation, not causation** — a resolved alert means the incident *ended*, not that *our answer fixed it*. A1 records the signal; A2 interprets it skeptically across many incidents (an entry whose recalls consistently don't resolve / recur is the demotion evidence).
- **Fresh-investigation outcomes have no entry link** (`kind=fresh`) — recall validation is the priority; fresh→curated linkage is A2.
- **Single-writer** — only the leader investigates and handles webhooks, so the leader owns the ledger; the path must be on leader-stable / shared storage (the mirror PV). A failover replays the JSONL to rebuild the open-index.
- **Unbounded JSONL** — grows with incidents; periodic compaction (drop resolved episodes older than N) is a deferred follow-up.

## 6. Testing

- **`outcome` unit**: `Open`/`Resolve` write the correct JSONL; startup replay rebuilds the open-index; `Resolve` matches the latest open + returns the right `Episode` + duration; resolve with no open → `ok=false`; empty path ⇒ no-op (no file, no panic).
- **`ParseAlertmanager`**: returns resolved alerts tagged `Status="resolved"`; the handler routes resolved → `Resolve` (and does NOT investigate/coalesce them).
- **Attribution**: `OnComplete` with a recalled investigation → an `open` event `kind=recall` + entry path; a fresh investigation → `kind=fresh`, no entry.
- **Metrics**: a matched resolve emits `incidents_resolved` + `incident_resolution_seconds`, and `recall_outcome{resolved}` when the open was a recall.

## 7. Out of scope (A2, A3)

- **A2** — feedback to recall: decay/demote, deriving recall confidence/gating from an entry's track record, and surfacing git-versioned aggregates (`recall_count`/`resolved_count`/`last_confirmed`) onto entry frontmatter.
- **A3** — wiring the dormant `Queue`/`Lifecycle`/`Recurrence` curate passes + a concrete cluster-state `ResolutionChecker` + a `RecurrenceStore`.
- **Causal attribution** (did our *action* fix it) — beyond the loop's scope; the resolved-alert proxy is the signal.

This slice is deliberately the *measurement layer*: it makes the outcome a recorded fact without yet acting on it.
