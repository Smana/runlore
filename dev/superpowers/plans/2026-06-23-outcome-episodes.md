# Outcome Episodes Read API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add a read-only API over the append-only outcome ledger — `Episodes()` (recurrence-aware replay) and a per-entry `OpenCounts()` aggregate — the A1→A2 seam the recall-decay edge will consume.

**Architecture:** All changes live in `internal/outcome/ledger.go`. A factored `readEvents()` helper replays the JSONL (reused by `New()`); `Episodes()` reconstructs every open into an `Episode`, LIFO-pairing resolves so recurrence is preserved; `OpenCounts()` rolls recall episodes up per entry. Pure addition — `Open`/`Resolve` recording is unchanged (only a behavior-preserving `Resolved bool` is added to `Episode`).

**Tech Stack:** Go 1.26, stdlib only (`bufio`/`encoding/json`/`os`/`sync`/`time`), stdlib `testing` (no testify). Tests follow `internal/outcome/ledger_test.go` (`t.TempDir()`, `time.Unix`).

**Spec:** `dev/superpowers/specs/2026-06-23-outcome-episodes-design.md`

**Branch:** `feat/outcome-episodes` (already checked out; the spec is already committed there).

---

## File structure

| File | Responsibility | Change |
|---|---|---|
| `internal/outcome/ledger.go` | Ledger types + record + read API | Add `readEvents()`; refactor `New()` to use it; add `Resolved bool` to `Episode` (+ set in `Resolve()`); add `Episodes()`; add `Aggregate` + `OpenCounts()` |
| `internal/outcome/ledger_test.go` | Ledger tests | Add tests for each of the above |

Task order: Task 1 (`readEvents` + `New` refactor) → Task 2 (`Resolved` field) → Task 3 (`Episodes`, needs 1+2) → Task 4 (`OpenCounts`, needs 3). No imports change in either file.

---

### Task 1: Factor `readEvents()` and refactor `New()` through it

**Files:**
- Modify: `internal/outcome/ledger.go` (`New` at `:45-73`)
- Test: `internal/outcome/ledger_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/outcome/ledger_test.go`:

```go
func TestReadEventsReturnsAllInOrder(t *testing.T) {
	p := filepath.Join(t.TempDir(), "o.jsonl")
	l, _ := New(p)
	t0 := time.Unix(3000, 0)
	_ = l.Open(Event{Fingerprint: "a", Kind: "fresh", At: t0})
	_ = l.Open(Event{Fingerprint: "b", Kind: "recall", Entry: "x.md", At: t0.Add(time.Second)})
	_, _, _ = l.Resolve("a", t0.Add(2*time.Second))
	events, err := l.readEvents()
	if err != nil {
		t.Fatalf("readEvents: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("want 3 events, got %d: %+v", len(events), events)
	}
	if events[0].Event != "open" || events[0].Fingerprint != "a" {
		t.Fatalf("event[0] = %+v", events[0])
	}
	if events[2].Event != "resolve" || events[2].Fingerprint != "a" {
		t.Fatalf("event[2] = %+v", events[2])
	}
}

func TestReadEventsDisabledOrAbsent(t *testing.T) {
	dis, _ := New("")
	if ev, err := dis.readEvents(); err != nil || ev != nil {
		t.Fatalf("disabled: want nil,nil; got %v,%v", ev, err)
	}
	absent, _ := New(filepath.Join(t.TempDir(), "missing.jsonl"))
	if ev, err := absent.readEvents(); err != nil || ev != nil {
		t.Fatalf("absent file: want nil,nil; got %v,%v", ev, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/outcome/ -run TestReadEvents`
Expected: FAIL — compile error `l.readEvents undefined`.

- [ ] **Step 3: Add `readEvents()` and refactor `New()`**

In `internal/outcome/ledger.go`, replace the entire `New` function (`:45-73`) with the refactored `New` plus the new helper:

```go
// New opens (replaying) the ledger at path. An empty path returns a disabled,
// no-op ledger (the feature is off).
func New(path string) (*Ledger, error) {
	l := &Ledger{path: path, open: map[string]Event{}}
	events, err := l.readEvents()
	if err != nil {
		return nil, err
	}
	for _, e := range events {
		switch e.Event {
		case "open":
			l.open[e.Fingerprint] = e
		case "resolve":
			delete(l.open, e.Fingerprint)
		}
	}
	return l, nil
}

// readEvents replays the ledger file in order, skipping corrupt lines. It returns
// an empty slice when the ledger is disabled (path=="") or the file is absent.
func (l *Ledger) readEvents() ([]Event, error) {
	if l.path == "" {
		return nil, nil
	}
	f, err := os.Open(l.path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	var events []Event
	for sc.Scan() {
		var e Event
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue // skip a corrupt line rather than fail
		}
		events = append(events, e)
	}
	return events, sc.Err()
}
```

(No import changes: `bufio`/`encoding/json`/`errors`/`io/fs`/`os` are all still used by `readEvents`.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/outcome/`
Expected: PASS — the two new tests AND all pre-existing ledger tests (`New` behavior is unchanged: disabled/absent → empty open-index, corrupt lines skipped, real open error propagated).

- [ ] **Step 5: Commit**

```bash
git add internal/outcome/ledger.go internal/outcome/ledger_test.go
git commit -m "refactor(outcome): factor readEvents; New replays via it"
```

---

### Task 2: Add `Resolved bool` to `Episode`

**Files:**
- Modify: `internal/outcome/ledger.go` (`Episode` at `:30-34`; `Resolve` return at `:122-125`)
- Test: `internal/outcome/ledger_test.go`

- [ ] **Step 1: Write the failing test**

Add to `internal/outcome/ledger_test.go`:

```go
func TestResolveMarksEpisodeResolved(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(4000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "e.md", At: t0})
	ep, ok, err := l.Resolve("fp", t0.Add(time.Minute))
	if err != nil || !ok {
		t.Fatalf("Resolve: ok=%v err=%v", ok, err)
	}
	if !ep.Resolved {
		t.Fatalf("a matched resolve must set Resolved=true: %+v", ep)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/outcome/ -run TestResolveMarksEpisodeResolved`
Expected: FAIL — compile error `ep.Resolved undefined`.

- [ ] **Step 3: Add the field and set it in `Resolve()`**

In `internal/outcome/ledger.go`, change the `Episode` struct (`:30-34`) to add the field:

```go
// Episode is a matched open→resolve pair (or, from Episodes(), an unresolved open
// when Resolved is false).
type Episode struct {
	Kind, Entry, Title, Resource string
	OpenedAt, ResolvedAt         time.Time
	Duration                     time.Duration
	Resolved                     bool
}
```

In `Resolve()`, change the returned `Episode` literal (`:122-125`) to set `Resolved: true`:

```go
	return Episode{
		Kind: o.Kind, Entry: o.Entry, Title: o.Title, Resource: o.Resource,
		OpenedAt: o.At, ResolvedAt: at, Duration: at.Sub(o.At), Resolved: true,
	}, true, nil
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/outcome/`
Expected: PASS — the new test plus all pre-existing tests (adding a field + setting it is behavior-preserving; no existing test asserts `Resolved`).

- [ ] **Step 5: Commit**

```bash
git add internal/outcome/ledger.go internal/outcome/ledger_test.go
git commit -m "feat(outcome): add Resolved flag to Episode"
```

---

### Task 3: `Episodes()` — recurrence-aware replay

**Files:**
- Modify: `internal/outcome/ledger.go` (add `Episodes` method)
- Test: `internal/outcome/ledger_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/outcome/ledger_test.go`:

```go
func TestEpisodesReconstructsRecurrence(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(5000, 0)
	for i := 0; i < 3; i++ {
		_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0.Add(time.Duration(i) * time.Second)})
	}
	_, _, _ = l.Resolve("fp", t0.Add(10*time.Second))
	eps, err := l.Episodes()
	if err != nil {
		t.Fatalf("Episodes: %v", err)
	}
	if len(eps) != 3 {
		t.Fatalf("want 3 episodes (recurrence preserved), got %d", len(eps))
	}
	resolved := 0
	for _, e := range eps {
		if e.Resolved {
			resolved++
		}
	}
	if resolved != 1 {
		t.Fatalf("want exactly 1 resolved episode, got %d", resolved)
	}
}

func TestEpisodesResolvedPairingAndDuration(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(6000, 0)
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "first.md", At: t0})
	_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "second.md", At: t0.Add(time.Second)})
	_, _, _ = l.Resolve("fp", t0.Add(5*time.Second))
	eps, _ := l.Episodes()
	var second Episode
	for _, e := range eps {
		if e.Entry == "first.md" && e.Resolved {
			t.Fatal("LIFO: the earlier open must remain unresolved")
		}
		if e.Entry == "second.md" {
			second = e
		}
	}
	if !second.Resolved || second.Duration != 4*time.Second {
		t.Fatalf("most-recent open should resolve with 4s duration: %+v", second)
	}
}

func TestEpisodesEmptyAndDisabled(t *testing.T) {
	dis, _ := New("")
	if eps, err := dis.Episodes(); err != nil || eps != nil {
		t.Fatalf("disabled: want nil,nil; got %v,%v", eps, err)
	}
	empty, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	if eps, err := empty.Episodes(); err != nil || len(eps) != 0 {
		t.Fatalf("empty ledger: want 0 episodes; got %v,%v", eps, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/outcome/ -run TestEpisodes`
Expected: FAIL — compile error `l.Episodes undefined`.

- [ ] **Step 3: Implement `Episodes()`**

In `internal/outcome/ledger.go`, add (after `Resolve`):

```go
// Episodes replays the full ledger and turns every open into an Episode, pairing
// each resolve with the most-recent (LIFO) unresolved open for the same
// fingerprint — so recurrence is preserved (N opens + 1 resolve ⇒ N episodes, 1
// resolved). Episodes are returned in open order; all kinds are included. A
// disabled/empty ledger yields nil.
func (l *Ledger) Episodes() ([]Episode, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	events, err := l.readEvents()
	if err != nil {
		return nil, err
	}
	var out []Episode
	pending := map[string][]int{} // fingerprint → stack of indices into out
	for _, e := range events {
		switch e.Event {
		case "open":
			out = append(out, Episode{
				Kind: e.Kind, Entry: e.Entry, Title: e.Title, Resource: e.Resource,
				OpenedAt: e.At,
			})
			pending[e.Fingerprint] = append(pending[e.Fingerprint], len(out)-1)
		case "resolve":
			stack := pending[e.Fingerprint]
			if len(stack) == 0 {
				continue // a resolve with no pending open (mirrors live ok=false)
			}
			i := stack[len(stack)-1]
			pending[e.Fingerprint] = stack[:len(stack)-1]
			out[i].ResolvedAt = e.At
			out[i].Duration = e.At.Sub(out[i].OpenedAt)
			out[i].Resolved = true
		}
	}
	return out, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/outcome/`
Expected: PASS — the three new tests plus all pre-existing tests.

- [ ] **Step 5: Commit**

```bash
git add internal/outcome/ledger.go internal/outcome/ledger_test.go
git commit -m "feat(outcome): Episodes() reconstructs open->resolve history"
```

---

### Task 4: `OpenCounts()` — per-entry aggregate

**Files:**
- Modify: `internal/outcome/ledger.go` (add `Aggregate` type + `OpenCounts` method)
- Test: `internal/outcome/ledger_test.go`

- [ ] **Step 1: Write the failing tests**

Add to `internal/outcome/ledger_test.go`:

```go
func TestOpenCountsAggregatesRecallEntries(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(7000, 0)
	for i := 0; i < 3; i++ {
		_ = l.Open(Event{Fingerprint: "fp", Kind: "recall", Entry: "x.md", At: t0.Add(time.Duration(i) * time.Second)})
	}
	resolveAt := t0.Add(10 * time.Second)
	_, _, _ = l.Resolve("fp", resolveAt)
	counts, err := l.OpenCounts()
	if err != nil {
		t.Fatalf("OpenCounts: %v", err)
	}
	a := counts["x.md"]
	if a.Recalls != 3 || a.Resolved != 1 {
		t.Fatalf("want recalls=3 resolved=1, got %+v", a)
	}
	if !a.LastConfirmed.Equal(resolveAt) {
		t.Fatalf("LastConfirmed = %v, want %v", a.LastConfirmed, resolveAt)
	}
}

func TestOpenCountsSkipsFresh(t *testing.T) {
	l, _ := New(filepath.Join(t.TempDir(), "o.jsonl"))
	t0 := time.Unix(8000, 0)
	_ = l.Open(Event{Fingerprint: "f1", Kind: "fresh", At: t0}) // fresh → no entry
	_, _, _ = l.Resolve("f1", t0.Add(time.Minute))
	counts, _ := l.OpenCounts()
	if len(counts) != 0 {
		t.Fatalf("fresh opens must not appear in OpenCounts, got %+v", counts)
	}
}

func TestOpenCountsEmptyMapWhenDisabled(t *testing.T) {
	dis, _ := New("")
	counts, err := dis.OpenCounts()
	if err != nil || counts == nil || len(counts) != 0 {
		t.Fatalf("disabled: want empty non-nil map; got %v,%v", counts, err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/outcome/ -run TestOpenCounts`
Expected: FAIL — compile error `l.OpenCounts undefined` (and `Aggregate` undefined).

- [ ] **Step 3: Implement `Aggregate` + `OpenCounts()`**

In `internal/outcome/ledger.go`, add (after `Episodes`):

```go
// Aggregate is a per-entry roll-up of recall episodes: how often the entry was
// recalled, how often the incident then resolved, and when it last resolved.
type Aggregate struct {
	Recalls       int
	Resolved      int
	LastConfirmed time.Time
}

// OpenCounts rolls Episodes up per catalog entry, counting recall episodes only
// (fresh investigations carry no entry). It is the input to recall decay:
// resolve-rate ≈ (Resolved+1)/(Recalls+2). A disabled/empty ledger yields an
// empty (non-nil) map.
func (l *Ledger) OpenCounts() (map[string]Aggregate, error) {
	eps, err := l.Episodes()
	if err != nil {
		return nil, err
	}
	counts := map[string]Aggregate{}
	for _, e := range eps {
		if e.Kind != "recall" || e.Entry == "" {
			continue
		}
		a := counts[e.Entry]
		a.Recalls++
		if e.Resolved {
			a.Resolved++
			if e.ResolvedAt.After(a.LastConfirmed) {
				a.LastConfirmed = e.ResolvedAt
			}
		}
		counts[e.Entry] = a
	}
	return counts, nil
}
```

(Note: `OpenCounts` does NOT take `l.mu` itself — `Episodes()` already locks, and `sync.Mutex` is not reentrant.)

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/outcome/`
Expected: PASS — the three new tests plus all pre-existing tests.

- [ ] **Step 5: Commit**

```bash
git add internal/outcome/ledger.go internal/outcome/ledger_test.go
git commit -m "feat(outcome): OpenCounts() per-entry recall/resolved aggregate"
```

---

### Task 5: Whole-package verification

- [ ] **Step 1: Build, test, vet**

Run:
```bash
go build ./... && go test ./internal/outcome/ -count=1 -v && go vet ./internal/outcome/
```
Expected: build clean; every test PASS (old + new); vet clean.

No commit (verification only).

---

## Notes for the implementer

- This is a **read-only** addition. Do not change `Open`/`Resolve` recording semantics beyond setting the new `Resolved` flag in the episode `Resolve()` already returns.
- LIFO pairing is intentional — it mirrors how the live `Resolve()` matches the *latest* open for a fingerprint. Recurrence counts (`Recalls`/`Resolved`) are independent of pairing order; only *which* episode is marked resolved differs.
- Do not wire `OpenCounts()` into `deriveRecallConfidence`, add `expired`/`link` events, a `RecurrenceStore`, or metrics — all deferred to later slices (spec §7).
- Keep everything in `internal/outcome/ledger.go`; do not touch other packages.
