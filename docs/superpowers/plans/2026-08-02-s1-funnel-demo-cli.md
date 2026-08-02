# S1 — Funnel: keyless demo + CLI front door — Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A stranger with no API key and no cluster sees a real RunLore root cause in under a minute, and can then run `lore investigate` against their own cluster without writing a config file.

**Architecture:** A new `internal/model/replay` package implements `providers.ModelProvider` by replaying a recorded JSON transcript of assistant turns. `lore demo investigate --offline` wires it into the *existing* demo path — real fake providers, real loop, real `notify.Format` card — so only the model is canned. `--record` captures a fresh transcript from a live model. Separately, `lore investigate` gains a zero-config path that synthesizes a model config from environment variables when no `runlore.yaml` exists, and degrades gracefully when sources are absent.

**Tech Stack:** Go 1.x (see `go.mod`), stdlib only for the new package (`encoding/json`, `sync`), POSIX `sh` for `install.sh`, Hugo/Hextra for the docs page.

**Spec:** [`docs/superpowers/specs/2026-08-02-s1-funnel-demo-cli-design.md`](../specs/2026-08-02-s1-funnel-demo-cli-design.md)

## Global Constraints

- Every new `.go` file starts with `// SPDX-License-Identifier: Apache-2.0`.
- `golangci-lint run` must pass; the repo's config is `.golangci.yml` (revive package-comments and staticcheck are enabled — every new package needs a package comment).
- No new third-party dependencies.
- Credentials are read by env-var indirection only, never inlined, never logged.
- Errors are lower-case, no trailing punctuation, wrapped with `%w` where a cause exists.
- Comments explain *why*, matching the density of surrounding code — this codebase comments heavily and deliberately.
- Commit messages follow Conventional Commits (`feat:`, `fix:`, `docs:`, `test:`, `chore:`); release-please parses them.
- **Never** add co-author trailers or AI attribution to commits or PRs.

---

### Task 1: The replay model provider

**Files:**
- Create: `internal/model/replay/replay.go`
- Create: `internal/model/replay/replay_test.go`
- Create: `internal/model/replay/testdata/two-turns.json`

**Interfaces:**
- Consumes: `providers.ModelProvider`, `providers.CompletionResponse`, `providers.ToolCall`, `providers.Usage` (`internal/providers/providers.go:495`, `:822`, `:883`, `:870`).
- Produces:
  - `type Transcript struct { Version int; Scenario string; RecordedAt string; RecordedWith Recorded; Turns []Turn }`
  - `type Recorded struct { Provider string; Model string }`
  - `type Turn struct { Text string; ToolCalls []providers.ToolCall; Usage providers.Usage }`
  - `func Load(path string) (*Transcript, error)`
  - `func New(t *Transcript) *Provider`
  - `func (p *Provider) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error)`
  - `func (t *Transcript) ToolNames() []string`

- [ ] **Step 1: Write the failing test**

Create `internal/model/replay/testdata/two-turns.json`:

```json
{
  "version": 1,
  "scenario": "harbor-chart-bump",
  "recorded_at": "2026-08-02T09:14:00Z",
  "recorded_with": {"provider": "anthropic", "model": "claude-sonnet-5"},
  "turns": [
    {
      "text": "",
      "tool_calls": [{"id": "1", "name": "what_changed", "args": "{\"namespace\":\"harbor\"}"}],
      "usage": {"input_tokens": 4211, "output_tokens": 96}
    },
    {
      "text": "",
      "tool_calls": [{"id": "2", "name": "submit_findings", "args": "{\"confidence\":0.86}"}],
      "usage": {"input_tokens": 5310, "output_tokens": 402}
    }
  ]
}
```

Create `internal/model/replay/replay_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package replay_test

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/model/replay"
	"github.com/Smana/runlore/internal/providers"
)

// TestReplayReturnsTurnsInOrder is the core contract: the provider ignores the
// request and hands back the recorded assistant turns in the order they were
// recorded, usage included (the demo's cost footer reads it).
func TestReplayReturnsTurnsInOrder(t *testing.T) {
	tr, err := replay.Load("testdata/two-turns.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := replay.New(tr)

	first, err := p.Complete(context.Background(), providers.CompletionRequest{})
	if err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	if len(first.ToolCalls) != 1 || first.ToolCalls[0].Name != "what_changed" {
		t.Fatalf("turn 1 tool calls = %+v, want one what_changed", first.ToolCalls)
	}
	if first.Usage.InputTokens != 4211 || first.Usage.OutputTokens != 96 {
		t.Errorf("turn 1 usage = %+v, want 4211 in / 96 out", first.Usage)
	}

	second, err := p.Complete(context.Background(), providers.CompletionRequest{})
	if err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	if len(second.ToolCalls) != 1 || second.ToolCalls[0].Name != "submit_findings" {
		t.Fatalf("turn 2 tool calls = %+v, want one submit_findings", second.ToolCalls)
	}
}

// TestReplayExhaustionIsLoud: running past the recorded turns must fail with an
// actionable error naming the fix. Returning an empty completion would produce a
// demo that silently ends with no findings — the exact failure mode a first-time
// user cannot diagnose.
func TestReplayExhaustionIsLoud(t *testing.T) {
	tr, err := replay.Load("testdata/two-turns.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	p := replay.New(tr)
	for i := 0; i < 2; i++ {
		if _, err := p.Complete(context.Background(), providers.CompletionRequest{}); err != nil {
			t.Fatalf("turn %d: %v", i+1, err)
		}
	}
	_, err = p.Complete(context.Background(), providers.CompletionRequest{})
	if err == nil {
		t.Fatal("expected an error past the end of the transcript")
	}
	if !strings.Contains(err.Error(), "exhausted") || !strings.Contains(err.Error(), "--record") {
		t.Errorf("error must say it is exhausted and how to fix it, got: %v", err)
	}
}

// TestToolNames exposes the recorded tool names so the demo wiring can assert they
// all still exist (the drift guard in Task 3).
func TestToolNames(t *testing.T) {
	tr, err := replay.Load("testdata/two-turns.json")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	got := tr.ToolNames()
	want := map[string]bool{"what_changed": true, "submit_findings": true}
	if len(got) != len(want) {
		t.Fatalf("ToolNames() = %v, want %d distinct names", got, len(want))
	}
	for _, n := range got {
		if !want[n] {
			t.Errorf("unexpected tool name %q", n)
		}
	}
}

// TestLoadRejectsEmptyTranscript: an empty turns list would fail later, deep in the
// loop, with a confusing message. Fail at load with a clear one instead.
func TestLoadRejectsEmptyTranscript(t *testing.T) {
	if _, err := replay.Load("testdata/does-not-exist.json"); err == nil {
		t.Fatal("expected an error for a missing transcript")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/model/replay/ -v`
Expected: FAIL — `no required module provides package github.com/Smana/runlore/internal/model/replay`

- [ ] **Step 3: Write the implementation**

Create `internal/model/replay/replay.go`:

```go
// SPDX-License-Identifier: Apache-2.0

// Package replay serves a recorded LLM transcript as a providers.ModelProvider.
//
// It exists so `lore demo investigate --offline` can show a REAL root cause with no
// API key and no network: the model turns are replayed from a transcript recorded
// once against a live model, while the tools, the investigation loop and the
// rendered verdict card remain the production code paths. Only the model is canned.
package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/Smana/runlore/internal/providers"
)

// Transcript is a recorded investigation: the ordered assistant turns a live model
// produced for one scenario, plus the provenance a reader needs to judge them.
type Transcript struct {
	Version      int      `json:"version"`
	Scenario     string   `json:"scenario"`
	RecordedAt   string   `json:"recorded_at"`
	RecordedWith Recorded `json:"recorded_with"`
	Turns        []Turn   `json:"turns"`
}

// Recorded names the model that produced the transcript. It is rendered with the
// demo output so a replayed card never passes itself off as a fresh live run.
type Recorded struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
}

// Turn is one recorded assistant completion.
type Turn struct {
	Text      string               `json:"text,omitempty"`
	ToolCalls []providers.ToolCall `json:"tool_calls,omitempty"`
	Usage     providers.Usage      `json:"usage,omitzero"`
}

// Load reads and validates a transcript. A missing file, malformed JSON, or an
// empty turn list fails here — where the error can name the file — rather than
// midway through a demo the user is watching.
func Load(path string) (*Transcript, error) {
	data, err := os.ReadFile(path) //nolint:gosec // G304: operator-supplied transcript path
	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}
	var t Transcript
	if err := json.Unmarshal(data, &t); err != nil {
		return nil, fmt.Errorf("parse transcript %s: %w", path, err)
	}
	if len(t.Turns) == 0 {
		return nil, fmt.Errorf("transcript %s has no turns", path)
	}
	return &t, nil
}

// ToolNames returns the distinct tool names the transcript calls, in first-seen
// order. The demo wiring asserts every one still exists, so renaming a tool fails
// CI instead of shipping a broken demo.
func (t *Transcript) ToolNames() []string {
	seen := map[string]bool{}
	var names []string
	for _, turn := range t.Turns {
		for _, tc := range turn.ToolCalls {
			if !seen[tc.Name] {
				seen[tc.Name] = true
				names = append(names, tc.Name)
			}
		}
	}
	return names
}

// Provider replays a transcript's turns in order. Safe for concurrent use because
// the loop may call from more than one goroutine, though today it does not.
type Provider struct {
	t  *Transcript
	mu sync.Mutex
	i  int
}

// New wraps a loaded transcript as a model provider.
func New(t *Transcript) *Provider { return &Provider{t: t} }

// Transcript exposes the replayed transcript so callers can render its provenance.
func (p *Provider) Transcript() *Transcript { return p.t }

// Complete returns the next recorded turn, ignoring the request entirely — the
// transcript is a fixed script, not a function of the prompt.
//
// Running past the end is an ERROR, deliberately. The loop asking for one more turn
// than was recorded means the fixture no longer matches the code driving it, and the
// only correct response is to say so and name the fix.
func (p *Provider) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.i >= len(p.t.Turns) {
		return providers.CompletionResponse{}, fmt.Errorf(
			"transcript exhausted after %d turns (the loop asked for another) — re-record with `lore demo investigate --record <path>`",
			len(p.t.Turns))
	}
	turn := p.t.Turns[p.i]
	p.i++
	return providers.CompletionResponse{
		Text:      turn.Text,
		ToolCalls: turn.ToolCalls,
		Usage:     turn.Usage,
	}, nil
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/model/replay/ -v`
Expected: PASS — all four tests.

If `omitzero` is rejected by the toolchain's Go version, use `json:"usage"` instead — the field is always written by the recorder, so omission is not needed.

- [ ] **Step 5: Lint**

Run: `golangci-lint run ./internal/model/replay/...`
Expected: no issues.

- [ ] **Step 6: Commit**

```bash
git add internal/model/replay/
git commit -m "feat(demo): replay a recorded LLM transcript as a model provider"
```

---

### Task 2: The recorder

**Files:**
- Create: `internal/model/replay/record.go`
- Create: `internal/model/replay/record_test.go`

**Interfaces:**
- Consumes: `replay.Transcript`, `replay.Turn`, `providers.ModelProvider` (Task 1).
- Produces:
  - `func NewRecorder(inner providers.ModelProvider, meta Recorded, scenario string) *Recorder`
  - `func (r *Recorder) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error)`
  - `func (r *Recorder) Write(path, recordedAt string) error`

- [ ] **Step 1: Write the failing test**

Create `internal/model/replay/record_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package replay_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/model/replay"
	"github.com/Smana/runlore/internal/providers"
)

// fakeModel returns a fixed sequence of completions, standing in for a live model
// during the recorder test (no network, no key).
type fakeModel struct {
	responses []providers.CompletionResponse
	i         int
}

func (f *fakeModel) Complete(context.Context, providers.CompletionRequest) (providers.CompletionResponse, error) {
	r := f.responses[f.i]
	f.i++
	return r, nil
}

// TestRecordRoundTrip: what the recorder writes must replay identically. This is the
// guarantee that makes `--record` trustworthy — a re-recorded fixture behaves exactly
// like the live run it captured.
func TestRecordRoundTrip(t *testing.T) {
	live := &fakeModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "what_changed", Args: `{"namespace":"harbor"}`}},
			Usage: providers.Usage{InputTokens: 100, OutputTokens: 20}},
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: "submit_findings", Args: `{"confidence":0.9}`}},
			Usage: providers.Usage{InputTokens: 300, OutputTokens: 80}},
	}}

	rec := replay.NewRecorder(live, replay.Recorded{Provider: "anthropic", Model: "claude-sonnet-5"}, "harbor-chart-bump")
	for i := 0; i < 2; i++ {
		if _, err := rec.Complete(context.Background(), providers.CompletionRequest{}); err != nil {
			t.Fatalf("record turn %d: %v", i+1, err)
		}
	}

	path := filepath.Join(t.TempDir(), "transcript.json")
	if err := rec.Write(path, "2026-08-02T09:14:00Z"); err != nil {
		t.Fatalf("write: %v", err)
	}

	tr, err := replay.Load(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if tr.Scenario != "harbor-chart-bump" || tr.RecordedWith.Model != "claude-sonnet-5" {
		t.Errorf("provenance lost: %+v", tr)
	}
	if len(tr.Turns) != 2 {
		t.Fatalf("turns = %d, want 2", len(tr.Turns))
	}

	p := replay.New(tr)
	for i, want := range []string{"what_changed", "submit_findings"} {
		got, err := p.Complete(context.Background(), providers.CompletionRequest{})
		if err != nil {
			t.Fatalf("replay turn %d: %v", i+1, err)
		}
		if got.ToolCalls[0].Name != want {
			t.Errorf("replay turn %d = %q, want %q", i+1, got.ToolCalls[0].Name, want)
		}
	}
}

// TestWriteRefusesEmpty: writing a transcript with no captured turns would ship a
// fixture that breaks the demo on first use. Fail at write time instead.
func TestWriteRefusesEmpty(t *testing.T) {
	rec := replay.NewRecorder(&fakeModel{}, replay.Recorded{}, "none")
	err := rec.Write(filepath.Join(t.TempDir(), "empty.json"), "2026-08-02T09:14:00Z")
	if err == nil {
		t.Fatal("expected an error writing a transcript with no turns")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/model/replay/ -run 'TestRecord|TestWrite' -v`
Expected: FAIL — `undefined: replay.NewRecorder`

- [ ] **Step 3: Write the implementation**

Create `internal/model/replay/record.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package replay

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sync"

	"github.com/Smana/runlore/internal/providers"
)

// Recorder decorates a live model, capturing every completion so the run can be
// replayed later with no key. It is the only writer of transcripts — regenerating a
// demo fixture is `--record`, never hand-editing JSON.
type Recorder struct {
	inner    providers.ModelProvider
	meta     Recorded
	scenario string

	mu    sync.Mutex
	turns []Turn
}

// NewRecorder wraps inner, tagging the capture with the model that produced it.
func NewRecorder(inner providers.ModelProvider, meta Recorded, scenario string) *Recorder {
	return &Recorder{inner: inner, meta: meta, scenario: scenario}
}

// Complete delegates and captures the response. A failed call is NOT captured: a
// transcript must contain only turns that really happened, or replay would diverge
// from the live run it claims to reproduce.
func (r *Recorder) Complete(ctx context.Context, req providers.CompletionRequest) (providers.CompletionResponse, error) {
	resp, err := r.inner.Complete(ctx, req)
	if err != nil {
		return resp, err
	}
	r.mu.Lock()
	r.turns = append(r.turns, Turn{Text: resp.Text, ToolCalls: resp.ToolCalls, Usage: resp.Usage})
	r.mu.Unlock()
	return resp, nil
}

// Write serializes the captured turns to path. recordedAt is passed in rather than
// read from the clock so callers stay testable.
func (r *Recorder) Write(path, recordedAt string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.turns) == 0 {
		return fmt.Errorf("nothing recorded — refusing to write an empty transcript to %s", path)
	}
	t := Transcript{
		Version: 1, Scenario: r.scenario, RecordedAt: recordedAt,
		RecordedWith: r.meta, Turns: r.turns,
	}
	data, err := json.MarshalIndent(t, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal transcript: %w", err)
	}
	return os.WriteFile(path, append(data, '\n'), 0o600)
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/model/replay/ -v`
Expected: PASS — all six tests.

- [ ] **Step 5: Lint**

Run: `golangci-lint run ./internal/model/replay/...`
Expected: no issues.

- [ ] **Step 6: Commit**

```bash
git add internal/model/replay/record.go internal/model/replay/record_test.go
git commit -m "feat(demo): record a live investigation to a replayable transcript"
```

---

### Task 3: Wire `--offline` and `--record` into `lore demo investigate`

**Files:**
- Modify: `internal/app/demo.go:56-149`
- Create: `internal/app/testdata/demo-transcript.json`
- Modify: `internal/app/demo_test.go` (append tests)

**Interfaces:**
- Consumes: `replay.Load`, `replay.New`, `replay.NewRecorder`, `replay.Recorded`, `(*Recorder).Write`, `(*Transcript).ToolNames` (Tasks 1–2); the existing `runDemoInvestigateWithModel` seam (`demo.go:64`).
- Produces: `--offline <path>` and `--record <path>` flags on `lore demo investigate`; `demoDefaultTranscript` constant pointing at the shipped fixture.

**Note on the verify pass:** `demo.go:107` already sets `verifyModel = nil` whenever a model is injected, so verify turns draw from the same model. Replay and record both inherit that, which is why one ordered channel is correct.

- [ ] **Step 1: Write the failing test**

Create `internal/app/testdata/demo-transcript.json` — a hand-authored fixture for the *unit* test only (Task 4 records the real one the demo ships with). The tool calls mirror the proven script in `demo_test.go:39`:

```json
{
  "version": 1,
  "scenario": "harbor-chart-bump",
  "recorded_at": "2026-08-02T00:00:00Z",
  "recorded_with": {"provider": "test", "model": "fixture"},
  "turns": [
    {"tool_calls": [{"id": "1", "name": "what_changed", "args": "{\"namespace\":\"apps\"}"}],
     "usage": {"input_tokens": 4211, "output_tokens": 96}},
    {"tool_calls": [{"id": "2", "name": "submit_findings", "args": "{\"confidence\":0.8,\"root_causes\":[{\"summary\":\"chart 1.15 bump enabled a DB migration on harbor-db that blocks harbor-core\",\"confidence\":0.8}]}"}],
     "usage": {"input_tokens": 5310, "output_tokens": 402}},
    {"tool_calls": [{"id": "3", "name": "submit_verdicts", "args": "{\"verdicts\":[{\"index\":0,\"verdict\":\"keep\",\"confidence\":0.8}]}"}],
     "usage": {"input_tokens": 900, "output_tokens": 40}}
  ]
}
```

Append to `internal/app/demo_test.go`:

```go
// TestDemoOfflineThroughSeam is the funnel guarantee: with NO api key and NO network,
// `demo investigate --offline` drives the real loop over the real fixture tools and
// renders the real verdict card. This is what hack/demo.sh shows a first-time
// visitor. It asserts through the same writer seam the existing end-to-end test uses.
func TestDemoOfflineThroughSeam(t *testing.T) {
	var out, errOut bytes.Buffer
	err := runDemoInvestigateWithModel([]string{
		"--scenarios", "../../examples/scenarios",
		"--scenario", "harbor-chart-bump",
		"--offline", "testdata/demo-transcript.json",
	}, &out, &errOut, nil)
	if err != nil {
		t.Fatalf("demo --offline: %v\nstderr:\n%s", err, errOut.String())
	}
	got := out.String()
	for _, want := range []string{"→ what_changed", "submit_findings", "migration"} {
		if !strings.Contains(got, want) {
			t.Errorf("output missing %q, got:\n%s", want, got)
		}
	}
	// Provenance: a replayed card must say it is replayed, and name the model.
	if !strings.Contains(got, "recorded") {
		t.Errorf("output must disclose that the model turns are recorded, got:\n%s", got)
	}
}

// TestDemoOfflineNeedsNoAPIKey proves the key check is skipped on the offline path —
// the whole point of --offline.
func TestDemoOfflineNeedsNoAPIKey(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "")
	var out, errOut bytes.Buffer
	if err := runDemoInvestigateWithModel([]string{
		"--scenarios", "../../examples/scenarios",
		"--scenario", "harbor-chart-bump",
		"--offline", "testdata/demo-transcript.json",
	}, &out, &errOut, nil); err != nil {
		t.Fatalf("offline demo must not require a key, got: %v", err)
	}
}

```

Add the import `"github.com/Smana/runlore/internal/model/replay"` to `demo_test.go` if the tests reference it.

> The drift guard over the **shipped** transcript lives in Task 4, alongside the fixture it reads. Every task here ends green.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run 'TestDemoOffline|TestShippedTranscript' -v`
Expected: FAIL — `flag provided but not defined: -offline`

- [ ] **Step 3: Write the implementation**

In `internal/app/demo.go`, add the constant beside the existing defaults:

```go
// demoDefaultTranscript is the recorded transcript `--offline` replays when no path
// is given: a REAL investigation captured once against a live model, so a first-time
// user sees genuine model output with no key and no network. Re-record it with
// `lore demo investigate --record <path>`.
const demoDefaultTranscript = "examples/demo/harbor-chart-bump.transcript.json"
```

Add the flags in `runDemoInvestigateWithModel` after the existing ones:

```go
	offline := fs.String("offline", "", "replay a recorded transcript instead of calling a model — no API key, no network (use \"default\" for the shipped one)")
	record := fs.String("record", "", "record this run's model turns to a transcript file for later --offline replay")
```

Replace the model-resolution block (`demo.go:91-108`) with:

```go
	// Resolve the model. Three paths, in precedence order:
	//   1. a test-injected model (the existing seam) — used verbatim;
	//   2. --offline — replay a recorded transcript: no key, no network;
	//   3. the live model built from config, optionally wrapped by --record.
	//
	// Paths 1 and 2 both answer the verify turns from the SAME model (verifyModel is
	// nil), because a transcript is one ordered stream. --record forces the same
	// shape, so what is recorded is exactly what will later replay.
	verifyModel := BuildVerifyModel(cfg)
	var transcript *replay.Transcript
	var recorder *replay.Recorder
	switch {
	case model != nil:
		verifyModel = nil // the injected model answers verify turns itself
	case *offline != "":
		path := *offline
		if path == "default" {
			path = demoDefaultTranscript
		}
		t, err := replay.Load(path)
		if err != nil {
			return err
		}
		transcript, model, verifyModel = t, replay.New(t), nil
	default:
		if apiKeyEnv != "" && os.Getenv(apiKeyEnv) == "" {
			return fmt.Errorf("the demo needs a model API key: set %s to your key "+
				"(or point --config at a runlore.yaml with a configured model, or run with "+
				"--offline default to replay a recorded investigation with no key at all). "+
				"Everything else runs against built-in fake providers — no cluster required", apiKeyEnv)
		}
		apiKey := ""
		if apiKeyEnv != "" {
			apiKey = os.Getenv(apiKeyEnv)
		}
		model = BuildModel(cfg, apiKey)
		if *record != "" {
			recorder = replay.NewRecorder(model,
				replay.Recorded{Provider: cfg.Model.Provider, Model: cfg.Model.Model}, "")
			model, verifyModel = recorder, nil // record one ordered stream, replayable as-is
		}
	}
```

Update the header print (`demo.go:113`) to disclose provenance:

```go
	if transcript != nil {
		demoPrintf(out, "== RunLore demo: investigating %q (recorded model turns, fake providers, no cluster) ==\n", c.DisplayName())
		demoPrintf(out, "   model turns recorded %s with %s/%s\n\n",
			transcript.RecordedAt, transcript.RecordedWith.Provider, transcript.RecordedWith.Model)
	} else {
		demoPrintf(out, "== RunLore demo: investigating %q (fake providers, no cluster) ==\n\n", c.DisplayName())
	}
	demoPrintf(out, "incident: %s\n\n", oneLineIndent(c.Symptom()))
```

After the findings are printed (`demo.go:147`), flush the recording:

```go
	if recorder != nil {
		if err := recorder.Write(*record, time.Now().UTC().Format(time.RFC3339)); err != nil {
			return fmt.Errorf("write transcript: %w", err)
		}
		demoPrintf(out, "\ntranscript written to %s — replay it with `lore demo investigate --offline %s`\n", *record, *record)
	}
```

The recorder needs the scenario name, which is known only after `pickScenario`. Move the recorder construction to just after `c` is resolved, or set it via the existing struct — simplest is to pass `c.DisplayName()` by constructing the recorder after `pickScenario`:  move the whole `switch` block to *after* the `c, err := pickScenario(...)` call (it already is — `pickScenario` runs at `demo.go:81`, before `demoConfig` at `:86`), and use `replay.NewRecorder(model, meta, c.DisplayName())`.

Add `"time"` and `"github.com/Smana/runlore/internal/model/replay"` to the imports.

Update the usage string in `cmd/lore/main.go:31`:

```
  lore demo investigate --offline default                watch a REAL recorded investigation — no cluster, no API key, no network
  lore demo investigate [--scenario <name>]           watch a full investigation against fake providers (no cluster; needs a model API key)
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/app/ -run TestDemo -v`
Expected: PASS — `TestDemoOfflineThroughSeam`, `TestDemoOfflineNeedsNoAPIKey`, and the pre-existing demo tests.

Run: `go test ./internal/app/ && golangci-lint run ./internal/app/...`
Expected: PASS, no lint issues. **This task ends green** — nothing is left failing for a later task.

- [ ] **Step 5: Commit**

```bash
git add internal/app/demo.go internal/app/demo_test.go internal/app/testdata/ cmd/lore/main.go
git commit -m "feat(demo): --offline replays a recorded investigation, --record captures one"
```

---

### Task 4: Record the shipped transcript and rewrite `hack/demo.sh`

**Files:**
- Create: `examples/demo/harbor-chart-bump.transcript.json` (generated, committed)
- Rewrite: `hack/demo.sh`
- Create: `hack/demo-trigger-policy.sh` (the old `demo.sh`, moved)
- Modify: `CONTRIBUTING.md:181`, `AGENTS.md:20`, `README.md:178`

**Interfaces:**
- Consumes: `--record` / `--offline` from Task 3; `demoDefaultTranscript`.
- Produces: a committed transcript at the path `demoDefaultTranscript` names; `hack/demo.sh` as the verdict-card demo.

> **This task needs the maintainer's API key.** Recording is a one-time human step; everything after it is keyless forever.

- [ ] **Step 1: Record the transcript**

```bash
mkdir -p examples/demo
export ANTHROPIC_API_KEY=<your key>
go run ./cmd/lore demo investigate \
  --scenario harbor-chart-bump \
  --record examples/demo/harbor-chart-bump.transcript.json
```

Expected: the demo runs live, prints a verdict card, and reports `transcript written to examples/demo/harbor-chart-bump.transcript.json`.

- [ ] **Step 2: Verify the recording replays**

Run:
```bash
unset ANTHROPIC_API_KEY
go run ./cmd/lore demo investigate --offline default
```
Expected: the same verdict card, prefixed by `recorded model turns, fake providers, no cluster` and the provenance line. No network call to any model.

Read the card. **If the recorded root cause is weak or wrong, re-record** (models vary run to run) — this fixture is the first thing a stranger sees.

- [ ] **Step 3: Write the drift guard over the shipped transcript**

The fixture now exists, so the guard that reads it belongs here. Append to `internal/app/demo_test.go`:

```go
// TestShippedTranscriptToolsStillExist is the DRIFT GUARD over the fixture the demo
// actually ships. Every tool the transcript calls must still be registered by the
// demo's fixture tools; a renamed or removed tool fails CI here instead of breaking
// the demo silently for every new visitor.
func TestShippedTranscriptToolsStillExist(t *testing.T) {
	tr, err := replay.Load("../../" + demoDefaultTranscript)
	if err != nil {
		t.Fatalf("load shipped transcript: %v", err)
	}
	cases, err := eval.Load("../../examples/scenarios")
	if err != nil {
		t.Fatalf("load scenarios: %v", err)
	}
	c, err := pickScenario(cases, tr.Scenario)
	if err != nil {
		t.Fatalf("the transcript's scenario %q is gone: %v", tr.Scenario, err)
	}
	have := map[string]bool{
		// Loop-control tools are answered by the loop itself, not the fixture set.
		"submit_findings": true, "submit_verdicts": true,
	}
	for _, tool := range c.FakeTools() {
		have[tool.Name()] = true
	}
	for _, name := range tr.ToolNames() {
		if !have[name] {
			t.Errorf("shipped transcript calls tool %q, which no longer exists — re-record with `lore demo investigate --record %s`", name, demoDefaultTranscript)
		}
	}
}
```

Add the imports `"github.com/Smana/runlore/internal/eval"` and `"github.com/Smana/runlore/internal/model/replay"` to `demo_test.go` if not already present.

Run: `go test ./internal/app/ -run TestShippedTranscriptToolsStillExist -v`
Expected: PASS.

- [ ] **Step 3b: Mutation-test the guard**

Temporarily rename `what_changed` to `what_changed_XX` inside `examples/demo/harbor-chart-bump.transcript.json`. Run the test.
Expected: FAIL naming the missing tool and printing the re-record command. Revert the edit and re-run to confirm PASS.

- [ ] **Step 4: Move the old demo aside**

```bash
git mv hack/demo.sh hack/demo-trigger-policy.sh
```

Edit the header comment of `hack/demo-trigger-policy.sh` to:

```bash
#!/usr/bin/env bash
# Trigger-policy demo: run `lore serve` and fire mocked Alertmanager alerts through
# the trigger policy, showing which alerts become incidents (match, dedup,
# wrong-severity, wrong-environment, ignore-list, resolved).
#
# This shows the FILTER, not the investigation. For the investigation — a real root
# cause, keyless — run hack/demo.sh.
#
# Usage: hack/demo-trigger-policy.sh
```

- [ ] **Step 5: Write the new `hack/demo.sh`**

```bash
#!/usr/bin/env bash
# Demo: a REAL RunLore investigation, on recorded evidence, with no cluster, no API
# key and no network. The model turns are replayed from a transcript recorded once
# against a live model (examples/demo/*.transcript.json); the tools, the
# investigation loop and the rendered verdict card are the production code paths.
#
# Requires: Go. Nothing else.
# Usage: hack/demo.sh
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
BIN="$(mktemp -d)/lore"

go build -o "$BIN" "$ROOT/cmd/lore"
cd "$ROOT"          # the transcript + scenario paths are repo-relative
"$BIN" demo investigate --offline default
```

- [ ] **Step 6: Run it**

Run: `hack/demo.sh`
Expected: a verdict card on stdout within ~60s (mostly `go build`), no network access to a model endpoint.

- [ ] **Step 7: Update the references**

- `CONTRIBUTING.md:181` — change the annotation to two lines:
  ```
  hack/demo.sh                 # a real investigation on recorded evidence (no cluster, no key)
  hack/demo-trigger-policy.sh  # fires mocked Alertmanager alerts through the trigger policy
  ```
- `AGENTS.md:20` — same rename.
- `README.md:178` — update the quickstart block so it advertises the verdict card rather than the policy log lines, and keep the existing anchor `#-try-it-in-one-minute--no-cluster-no-keys` intact (Getting Started links to it).

- [ ] **Step 8: Shellcheck**

Run: `shellcheck hack/demo.sh hack/demo-trigger-policy.sh`
Expected: no findings.

- [ ] **Step 9: Commit**

```bash
git add examples/demo/ hack/demo.sh hack/demo-trigger-policy.sh CONTRIBUTING.md AGENTS.md README.md
git commit -m "feat(demo): hack/demo.sh shows a real root cause, keyless"
```

---

### Task 5: Zero-config `lore investigate`

**Files:**
- Modify: `internal/app/investigate_cmd.go:20-40`
- Create: `internal/app/investigate_config.go`
- Create: `internal/app/investigate_config_test.go`

**Interfaces:**
- Consumes: `config.Config`, `config.Model`, `config.Load` (`internal/config/load.go:16`).
- Produces:
  - `func resolveInvestigateConfig(path string, explicit bool) (*config.Config, error)`
  - `const defaultConfigPath = "runlore.yaml"`

- [ ] **Step 1: Write the failing test**

Create `internal/app/investigate_config_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveFromOpenAIEnv: with no runlore.yaml, an OpenAI-compatible endpoint in the
// environment is enough to investigate. This is the laptop-to-value path — no config
// ceremony before the first answer.
func TestResolveFromOpenAIEnv(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("OPENAI_BASE_URL", "http://localhost:8000/v1")
	t.Setenv("OPENAI_MODEL", "qwen3-30b")
	t.Setenv("ANTHROPIC_API_KEY", "")

	cfg, err := resolveInvestigateConfig(defaultConfigPath, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Model.BaseURL != "http://localhost:8000/v1" {
		t.Errorf("base_url = %q, want the env value", cfg.Model.BaseURL)
	}
	if cfg.Model.Model != "qwen3-30b" {
		t.Errorf("model = %q, want the env value", cfg.Model.Model)
	}
	// Keyless is legitimate here: a local vLLM/Ollama needs no key.
	if cfg.Model.APIKeyEnv != "" && os.Getenv(cfg.Model.APIKeyEnv) != "" {
		t.Errorf("expected keyless, got api_key_env=%q", cfg.Model.APIKeyEnv)
	}
}

// TestResolveFromAnthropicEnv: the other zero-config path.
func TestResolveFromAnthropicEnv(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	cfg, err := resolveInvestigateConfig(defaultConfigPath, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Model.Provider != "anthropic" {
		t.Errorf("provider = %q, want anthropic", cfg.Model.Provider)
	}
	if cfg.Model.APIKeyEnv != "ANTHROPIC_API_KEY" {
		t.Errorf("api_key_env = %q, want ANTHROPIC_API_KEY", cfg.Model.APIKeyEnv)
	}
}

// TestResolveWithNoEnvExplainsBothPaths: the error a first-time user hits must tell
// them exactly what to set, not just that something is missing.
func TestResolveWithNoEnvExplainsBothPaths(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("OPENAI_BASE_URL", "")
	t.Setenv("OPENAI_API_KEY", "")
	t.Setenv("ANTHROPIC_API_KEY", "")

	_, err := resolveInvestigateConfig(defaultConfigPath, false)
	if err == nil {
		t.Fatal("expected an error with no config and no env")
	}
	for _, want := range []string{"OPENAI_BASE_URL", "ANTHROPIC_API_KEY", "runlore.yaml"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q, got: %v", want, err)
		}
	}
}

// TestExplicitMissingConfigStillErrors: silence here would hide a typo'd --config
// path behind a surprise zero-config run against the wrong model.
func TestExplicitMissingConfigStillErrors(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	_, err := resolveInvestigateConfig(filepath.Join(t.TempDir(), "typo.yaml"), true)
	if err == nil {
		t.Fatal("an explicitly named missing config must be an error")
	}
}

// TestExistingConfigWins: a real runlore.yaml is an explicit statement of intent and
// must never be silently replaced by environment guesses.
func TestExistingConfigWins(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	if err := os.WriteFile(filepath.Join(dir, defaultConfigPath), []byte(
		"model:\n  base_url: http://from-file:8000/v1\n  model: from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := resolveInvestigateConfig(defaultConfigPath, false)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if cfg.Model.Model != "from-file" {
		t.Errorf("model = %q, want the file's value", cfg.Model.Model)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run TestResolve -v`
Expected: FAIL — `undefined: resolveInvestigateConfig`

- [ ] **Step 3: Write the implementation**

Create `internal/app/investigate_config.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/Smana/runlore/internal/config"
)

// defaultConfigPath is the config `lore investigate` looks for when --config is not
// given. Its ABSENCE is not an error: the CLI is the laptop-to-value front door, and
// requiring a YAML file before the first answer is the friction this removes.
const defaultConfigPath = "runlore.yaml"

// Env vars the zero-config path reads, in precedence order. They are the names the
// ecosystem already uses, so a user who has run any OpenAI-compatible or Anthropic
// tool already has them exported.
const (
	envOpenAIBaseURL = "OPENAI_BASE_URL"
	envOpenAIKey     = "OPENAI_API_KEY"
	envOpenAIModel   = "OPENAI_MODEL"
	envAnthropicKey  = "ANTHROPIC_API_KEY"

	// defaultOpenAIModel / defaultAnthropicModel are used when the endpoint is known
	// but the model name is not. A local vLLM/Ollama commonly serves one model, and
	// naming it wrongly fails loudly at the first call rather than silently.
	defaultOpenAIModel    = "gpt-4o-mini"
	defaultAnthropicModel = "claude-sonnet-5"
)

// resolveInvestigateConfig loads the config for a one-off investigation.
//
//	explicit=true  (--config was given): a missing file is an ERROR. Falling back
//	               would run against a different model than the user asked for.
//	explicit=false (default path): a missing file falls back to the environment.
func resolveInvestigateConfig(path string, explicit bool) (*config.Config, error) {
	cfg, err := config.Load(path)
	switch {
	case err == nil:
		return cfg, nil
	case explicit:
		return nil, err
	case !errors.Is(err, fs.ErrNotExist):
		// The file exists but is broken — never paper over that with env guesses.
		return nil, err
	}
	return configFromEnv()
}

// configFromEnv synthesizes a minimal, model-only config from the environment. Every
// data source stays unset, so each disables its own tool — no cluster, no Flux, no
// KB repo, no forge and no notifier are required to get an answer.
func configFromEnv() (*config.Config, error) {
	switch {
	case os.Getenv(envOpenAIBaseURL) != "":
		m := config.Model{
			BaseURL: os.Getenv(envOpenAIBaseURL),
			Model:   or(os.Getenv(envOpenAIModel), defaultOpenAIModel),
		}
		// Keyless is a first-class case here: an in-cluster vLLM or a local Ollama
		// needs no credential, and demanding one would break the most private setup.
		if os.Getenv(envOpenAIKey) != "" {
			m.APIKeyEnv = envOpenAIKey
		}
		return &config.Config{Model: m}, nil
	case os.Getenv(envAnthropicKey) != "":
		return &config.Config{Model: config.Model{
			Provider:  "anthropic",
			Model:     defaultAnthropicModel,
			APIKeyEnv: envAnthropicKey,
		}}, nil
	default:
		return nil, fmt.Errorf(
			"no model configured: set %s (+ optional %s / %s) for an OpenAI-compatible endpoint, "+
				"or %s for Anthropic — or write a %s and pass it with --config",
			envOpenAIBaseURL, envOpenAIKey, envOpenAIModel, envAnthropicKey, defaultConfigPath)
	}
}

// or returns a when non-empty, else b.
func or(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
```

If `internal/app` already defines an `or` helper (check `model.go:75`), reuse it and drop this copy.

- [ ] **Step 4: Wire it into the command**

In `internal/app/investigate_cmd.go`, replace lines 22 and 32–35:

```go
	cfgPath := fs.String("config", "", "path to config file (default: ./runlore.yaml if present, else the environment)")
```

and

```go
	explicit := *cfgPath != ""
	path := *cfgPath
	if path == "" {
		path = defaultConfigPath
	}
	cfg, err := resolveInvestigateConfig(path, explicit)
	if err != nil {
		return err
	}
```

- [ ] **Step 5: Run tests to verify they pass**

Run: `go test ./internal/app/ -run 'TestResolve|TestExplicit|TestExisting' -v`
Expected: PASS — all five.

Run: `go test ./internal/app/`
Expected: PASS — the whole package, no regressions.

- [ ] **Step 6: Commit**

```bash
git add internal/app/investigate_config.go internal/app/investigate_config_test.go internal/app/investigate_cmd.go
git commit -m "feat(cli): lore investigate runs with no runlore.yaml, from the environment"
```

---

### Task 6: Investigate flags and a graceful-degradation notice

**Files:**
- Modify: `internal/app/investigate_cmd.go`
- Modify: `internal/app/investigate_config_test.go` (append)

**Interfaces:**
- Consumes: `resolveInvestigateConfig` (Task 5); `BuildModelAndTools` (`investigate_cmd.go:44`).
- Produces: `--model`, `--base-url`, `--metrics-url`, `--logs-url` flags; `func disabledTools(cfg *config.Config) []string`.

- [ ] **Step 1: Write the failing test**

Append to `internal/app/investigate_config_test.go`:

```go
// TestDisabledToolsNamesWhatIsOff: a thin answer must be explainable. When metrics,
// logs and the catalog are unset, the command says so once on stderr rather than
// leaving the user wondering why the agent never looked at their dashboards.
func TestDisabledToolsNamesWhatIsOff(t *testing.T) {
	cfg := &config.Config{Model: config.Model{Model: "m", BaseURL: "http://x/v1"}}
	got := disabledTools(cfg)
	joined := strings.Join(got, " ")
	for _, want := range []string{"metrics", "logs", "knowledge catalog"} {
		if !strings.Contains(joined, want) {
			t.Errorf("disabledTools() = %v, must mention %q", got, want)
		}
	}
}

// TestDisabledToolsSilentWhenWired: nothing to warn about when everything is set.
// Note the shapes: config.MetricsConfig and config.LogsConfig embed config.Endpoint
// inline (config.go:116, :131, :76), so URL is set through the embedded struct.
func TestDisabledToolsSilentWhenWired(t *testing.T) {
	cfg := &config.Config{
		Model:   config.Model{Model: "m", BaseURL: "http://x/v1"},
		Metrics: config.MetricsConfig{Endpoint: config.Endpoint{URL: "http://vm:8429"}},
		Logs:    config.LogsConfig{Endpoint: config.Endpoint{URL: "http://vl:9428"}},
		Catalog: config.Catalog{Dir: "/kb"},
	}
	if got := disabledTools(cfg); len(got) != 0 {
		t.Errorf("disabledTools() = %v, want none", got)
	}
}
```

Add `"github.com/Smana/runlore/internal/config"` to the test imports.

Field reference, already verified — do not re-derive: `Config.Metrics` is `MetricsConfig`, `Config.Logs` is `LogsConfig`, both embedding `Endpoint` (which carries `URL`); `Config.Catalog` is `Catalog` with `Dir` and `Git.URL` (`config.go:456`, `:540`). *Reading* `cfg.Metrics.URL` works through the embedding; *constructing* one needs the nested literal above.

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run TestDisabledTools -v`
Expected: FAIL — `undefined: disabledTools`

- [ ] **Step 3: Write the implementation**

Add to `internal/app/investigate_cmd.go`:

```go
// disabledTools names the investigation signals this run will NOT have, so a thin
// answer is explainable rather than mysterious. The CLI deliberately degrades
// instead of demanding a full stack — but silence about it would look like a bug.
func disabledTools(cfg *config.Config) []string {
	var off []string
	if cfg.Metrics.URL == "" {
		off = append(off, "metrics (query_metrics)")
	}
	if cfg.Logs.URL == "" {
		off = append(off, "logs (query_logs)")
	}
	if cfg.Catalog.Dir == "" && cfg.Catalog.Git.URL == "" {
		off = append(off, "knowledge catalog (kb_search, instant recall)")
	}
	return off
}
```

Add the flags beside the existing ones:

```go
	modelName := fs.String("model", "", "override the model name")
	baseURL := fs.String("base-url", "", "override the OpenAI-compatible endpoint")
	metricsURL := fs.String("metrics-url", "", "PromQL endpoint — enables query_metrics")
	logsURL := fs.String("logs-url", "", "logs endpoint — enables query_logs")
```

Apply them after the config resolves, and emit the notice:

```go
	// Flags override the resolved config. They are how a user points the CLI at their
	// stack without writing a file.
	if *modelName != "" {
		cfg.Model.Model = *modelName
	}
	if *baseURL != "" {
		cfg.Model.BaseURL = *baseURL
	}
	if *metricsURL != "" {
		cfg.Metrics.URL = *metricsURL
	}
	if *logsURL != "" {
		cfg.Logs.URL = *logsURL
	}
	if off := disabledTools(cfg); len(off) > 0 {
		fmt.Fprintf(os.Stderr, "note: running without %s — pass --metrics-url/--logs-url or a --config to enable them\n",
			strings.Join(off, ", "))
	}
```

Add `"strings"` to the imports.

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/app/ -run TestDisabledTools -v`
Expected: PASS.

Run: `go test ./internal/app/ && golangci-lint run ./internal/app/...`
Expected: PASS, no lint issues.

- [ ] **Step 5: Manual smoke (needs a cluster + a key)**

```bash
export ANTHROPIC_API_KEY=<key>
go run ./cmd/lore investigate --alert KubePodCrashLooping --namespace default
```
Expected: the note on stderr naming the disabled signals, then a rendered verdict card. No `runlore.yaml` present, no Flux, no KB, no notifier.

- [ ] **Step 6: Commit**

```bash
git add internal/app/investigate_cmd.go internal/app/investigate_config_test.go
git commit -m "feat(cli): investigate flags for model/metrics/logs plus a degradation notice"
```

---

### Task 7: `install.sh` and the CLI docs page

**Files:**
- Create: `website/static/install.sh`
- Create: `website/content/docs/reference/cli.md`
- Modify: `.github/workflows/ci.yaml` (extend the shellcheck step)

**Interfaces:**
- Consumes: the goreleaser archive naming in `.goreleaser.yaml:31` and the `checksums.txt` it publishes.
- Produces: `curl -fsSL https://runlore.io/install.sh | sh`.

- [ ] **Step 1: Confirm the archive naming**

Run: `sed -n '8,48p' .goreleaser.yaml`

Already verified — the script below is written against it, so this step is a confirmation, not a discovery:

| Setting | Value | Consequence |
|---|---|---|
| `project_name` | `runlore` (`.goreleaser.yaml:10`) | the archive is **`runlore_…`**, not `lore_…` |
| `binary` | `lore` (`:15`) | the file *inside* the archive is `lore` |
| `name_template` | `{{ .ProjectName }}_{{ .Version }}_{{ .Os }}_{{ .Arch }}` (`:34`) | `runlore_0.11.0_linux_amd64` |
| `formats` | `tar.gz`, zip on Windows (`:37`) | `.tar.gz` for linux/darwin |
| `checksum.name_template` | `checksums.txt` (`:47`) | the verification file's name |

`.Version` is the tag **without** the leading `v`, which is why the script strips it.

- [ ] **Step 2: Write `website/static/install.sh`**

```sh
#!/bin/sh
# RunLore installer — downloads the released `lore` binary for this OS/arch,
# verifies its SHA-256 against the published checksums, and installs it.
#
#   curl -fsSL https://runlore.io/install.sh | sh
#
# Environment:
#   LORE_VERSION      pin a release tag (default: latest)
#   LORE_INSTALL_DIR  install target (default: /usr/local/bin, else ~/.local/bin)
#
# This script is auditable at
# https://github.com/Smana/runlore/blob/main/website/static/install.sh
# It never runs sudo for you and never sends anything anywhere.
set -eu

REPO="Smana/runlore"
VERSION="${LORE_VERSION:-latest}"

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)
case "$arch" in
  x86_64|amd64) arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *) echo "unsupported architecture: $arch" >&2; exit 1 ;;
esac
case "$os" in
  linux|darwin) ;;
  *) echo "unsupported OS: $os — build from source with 'go install github.com/Smana/runlore/cmd/lore@latest'" >&2; exit 1 ;;
esac

if [ "$VERSION" = "latest" ]; then
  VERSION=$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
    | sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -1)
  [ -n "$VERSION" ] || { echo "could not resolve the latest release tag" >&2; exit 1; }
fi

# Matches .goreleaser.yaml: project_name=runlore, name_template
# {{.ProjectName}}_{{.Version}}_{{.Os}}_{{.Arch}}, tar.gz. Note the archive is named
# for the PROJECT (runlore) while the binary inside is `lore`.
archive="runlore_${VERSION#v}_${os}_${arch}.tar.gz"
base="https://github.com/$REPO/releases/download/$VERSION"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "downloading $archive ($VERSION)"
curl -fsSL "$base/$archive" -o "$tmp/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

# Verify before extracting. A checksum mismatch means a corrupted or tampered
# download, and extracting it anyway would defeat the point of checking.
( cd "$tmp" && grep " $archive\$" checksums.txt | sha256sum -c - ) \
  || { echo "checksum verification FAILED for $archive" >&2; exit 1; }

tar -xzf "$tmp/$archive" -C "$tmp" lore

dir="${LORE_INSTALL_DIR:-/usr/local/bin}"
if [ ! -w "$dir" ]; then
  dir="$HOME/.local/bin"
  mkdir -p "$dir"
  echo "note: /usr/local/bin is not writable — installing to $dir"
fi
install -m 0755 "$tmp/lore" "$dir/lore"

echo "installed $("$dir/lore" version) to $dir/lore"
echo
echo "Try it with no cluster and no API key:"
echo "  lore demo investigate --offline default"
echo
echo "Verify the release signature (optional):"
echo "  cosign verify-blob --bundle checksums.txt.bundle \\"
echo "    --certificate-identity-regexp 'https://github.com/$REPO/.github/workflows/release-binaries.yml@.*' \\"
echo "    --certificate-oidc-issuer https://token.actions.githubusercontent.com checksums.txt"
```

On macOS `sha256sum` is absent — add a shim right before the verification block:

```sh
if ! command -v sha256sum >/dev/null 2>&1; then
  sha256sum() { shasum -a 256 "$@"; }
fi
```

- [ ] **Step 3: Test the script**

```bash
shellcheck website/static/install.sh
sh website/static/install.sh   # with LORE_INSTALL_DIR set to a temp dir
```

Run:
```bash
LORE_INSTALL_DIR=$(mktemp -d) sh website/static/install.sh
```
Expected: downloads, verifies the checksum, prints `installed lore <version>`. Then run the printed `lore demo investigate --offline default` — it must produce the verdict card.

Test the failure path by editing the archive name to a nonexistent one: expected a clean `curl` error, not a partial install.

- [ ] **Step 4: Write the CLI docs page**

Create `website/content/docs/reference/cli.md` with front matter `title: CLI` and `weight: 5`, covering:

- **Install** — the `curl | sh` one-liner, `LORE_VERSION`/`LORE_INSTALL_DIR`, the `go install github.com/Smana/runlore/cmd/lore@latest` alternative for readers who prefer not to pipe a script to a shell, and the cosign verification command.
- **`lore demo investigate --offline default`** — what it replays, that the model turns are recorded (with the provenance line explained), and that no key or network is needed.
- **`lore investigate`** — the zero-config env vars (`OPENAI_BASE_URL`/`OPENAI_API_KEY`/`OPENAI_MODEL`, `ANTHROPIC_API_KEY`), the flags from Task 6, and a table of what each unset source disables.
- **When to move to `lore serve`** — one paragraph pointing at Getting Started tier 3.

Use `{{< relref >}}` for internal links (`refLinksErrorLevel: ERROR` fails the build on a bad one) and absolute GitHub URLs for repo files.

- [ ] **Step 5: Build the site**

Run: `cd website && hugo --gc --minify`
Expected: build succeeds, no unresolved refs.

- [ ] **Step 6: Extend the shellcheck CI step**

In `.github/workflows/ci.yaml`, find the step that shellchecks `hack/*.sh` and add `website/static/install.sh` to its target list. If no such step exists, add one:

```yaml
      - name: shellcheck
        run: shellcheck hack/*.sh website/static/install.sh
```

- [ ] **Step 7: Commit**

```bash
git add website/static/install.sh website/content/docs/reference/cli.md .github/workflows/ci.yaml
git commit -m "feat(cli): install.sh plus a first-class CLI reference page"
```

---

## Final verification

- [ ] `go build ./... && go test ./... && golangci-lint run` — all clean
- [ ] `hack/demo.sh` on a shell with **no** `ANTHROPIC_API_KEY`/`OPENAI_API_KEY` prints a verdict card
- [ ] `cd website && hugo --gc --minify` builds
- [ ] `shellcheck hack/*.sh website/static/install.sh` clean
- [ ] Run the security review on the branch diff (`/security-review`), paying attention to `install.sh` (download verification, no privilege escalation, no credential handling) and the transcript loader (untrusted-path reads)
- [ ] Open the PR — English title and description, no AI attribution, no co-author trailers
</content>
