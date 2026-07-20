# Templated Notifier + Registry Pinning Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A config-only generic templated notifier — POST findings to any chat/webhook endpoint (Teams, Discord, ntfy, incident.io…) with a user-supplied Go `text/template` body — plus tests that pin the already-unified notifier registry so it can never regress.

**Architecture:** Extract webhook's private JSON payload into an exported `notify.Payload` (single definition of "what a delivery carries"), then add a self-registering `internal/notify/templated` package modeled on `internal/notify/webhook`: config lives under the existing inline `notify.Extra` catch-all (`config.go:551` — ZERO config-struct changes), template parse errors surface through `BuildEnabled`'s error return and therefore fail serve startup, template execution errors fail only that instance's delivery (the existing `Multi.Deliver` at `slack.go:742-751` already logs-and-joins per-notifier errors). Redaction needs no new seam: every notifier receives an already-redacted Investigation (`redactInvestigation`, `internal/investigate/loop.go:965`, reflective walk at `:1007`) — state this in docs, do not re-redact.

**Fact check that reshaped scope (vs the audit finding):** Slack and Matrix construction is ALREADY registry-based — `slack.go:23-44` and `matrix.go:24-37` self-register via `init()` + `Register`, and `app.BuildNotifier` (`internal/app/notify.go:17`) is just `BuildEnabled`. The remaining asymmetry is the *config surface only*: built-ins get typed blocks (`notify.slack`, `notify.matrix`), drop-ins use the inline `Extra` map — that is a deliberate design (typed validation for built-ins, zero-config-edit extensibility for drop-ins), not debt. So Task 4 PINS the unified behavior with round-trip tests and documents the two surfaces; it does not migrate anything.

**Tech Stack:** Go (toolchain go1.26.5); stdlib `text/template` only (no new dependencies); existing `internal/{notify,providers,httpx,config,app}`.

## Global Constraints

- Quality gate before every commit: `go build ./... && go vet ./... && go test ./... && gofmt -l .` (empty) and `golangci-lint run ./...` → `0 issues.`; plus `go test -race ./internal/notify/... ./internal/config/... ./internal/app/...` before the final push.
- SPDX header `// SPDX-License-Identifier: Apache-2.0` as line 1 of every new `.go` file.
- Conventional Commits; **no co-author trailer**, no AI attribution anywhere.
- **Zero user-visible config breakage**: `notify.slack`, `notify.matrix`, `notify.webhook` keys keep working verbatim; the webhook notifier's JSON body must be byte-identical after the Task-1 refactor (existing `webhook_test.go` passes unchanged).
- No new third-party dependencies. Egress via `httpx.SecureClient` only (SSRF guard parity with webhook, `webhook.go:40`).
- Fail-safe bias: unset env var ⇒ instance disabled (webhook parity, `webhook.go:171-174`); template PARSE error ⇒ startup failure; template EXEC error ⇒ that delivery fails loudly but siblings still deliver (`Multi` isolation).

---

### Task 1: Export the delivery payload as `notify.Payload`

**Files:**
- Create: `internal/notify/payload.go`
- Create: `internal/notify/payload_test.go`
- Modify: `internal/notify/webhook/webhook.go` (delete local payload types, use `notify.NewPayload`)

**Interfaces:**
- Consumes: `providers.Investigation`, `notify.Format` (same package).
- Produces: `notify.Payload` / `notify.PriorPayload` / `notify.MatchedPayload` structs (json tags copied VERBATIM from `webhook.go:46-86`) and `func NewPayload(inv providers.Investigation) Payload` carrying the exact mapping currently inlined in `webhook.Deliver` (`webhook.go:89-125`): RFC3339 `StartedAt` ("" when zero), `Prior` nil-guard, `MatchedKnowledge` surfaced only when `Prior == nil`, `Text: Format(inv)`. Tasks 2-3 template over this struct.

- [ ] **Step 1: Write the failing test** — `internal/notify/payload_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func TestNewPayloadMapping(t *testing.T) {
	inv := providers.Investigation{
		Title:      "CrashLoopBackOff payments",
		Confidence: 0.72,
		Resource:   providers.ResourceRef{Namespace: "payments", Name: "api"},
		Verdict:    providers.VerdictActionSuggested,
		StartedAt:  time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
		Prior:      &providers.PriorKnowledge{Cause: "bad rollout", Resolution: "rollback", EntryPath: "e.md", Recalls: 3, Resolved: 2},
		MatchedKnowledge: &providers.MatchedEntry{Path: "m.md", Title: "seen", URL: "u", Score: 0.9},
	}
	p := NewPayload(inv)
	if p.Title != inv.Title || p.Namespace != "payments" || p.Resource != "api" {
		t.Errorf("identity fields: %+v", p)
	}
	if p.StartedAt != "2026-07-20T10:00:00Z" {
		t.Errorf("StartedAt = %q, want RFC3339", p.StartedAt)
	}
	if p.Prior == nil || p.Prior.Cause != "bad rollout" || p.Prior.Recalls != 3 {
		t.Errorf("Prior = %+v", p.Prior)
	}
	// The shared Format-text guard: matched knowledge is suppressed when Prior is set,
	// so the structured field never disagrees with the rendered text (webhook.go:98-104).
	if p.MatchedKnowledge != nil {
		t.Errorf("MatchedKnowledge must be nil when Prior != nil, got %+v", p.MatchedKnowledge)
	}
	if p.Text == "" {
		t.Error("Text must carry Format(inv)")
	}

	inv.Prior = nil
	if q := NewPayload(inv); q.MatchedKnowledge == nil || q.MatchedKnowledge.Path != "m.md" {
		t.Errorf("MatchedKnowledge must surface when Prior == nil, got %+v", q.MatchedKnowledge)
	}
	if q := NewPayload(providers.Investigation{}); q.StartedAt != "" {
		t.Errorf("zero StartedAt must render empty, got %q", q.StartedAt)
	}
}
```

(Verify the exact field names of `providers.PriorKnowledge`, `providers.MatchedEntry`, `providers.ResourceRef`, and the verdict constant against `internal/providers/providers.go` before running — the mapping in `webhook.go:89-125` is authoritative; adjust the literal, not the contract.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/notify/ -run TestNewPayloadMapping -v`
Expected: FAIL — `undefined: NewPayload`

- [ ] **Step 3: Implement** — `internal/notify/payload.go`: move the three structs from `webhook.go:46-86` verbatim (exported: `Payload`, `PriorPayload`, `MatchedPayload`; keep every json tag identical) and lift the mapping from `webhook.Deliver`:

```go
// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// Payload is the exported delivery payload: the single definition of what an
// outbound notification carries. The webhook notifier marshals it as-is; the
// templated notifier exposes it as the template dot. Field set and json tags
// are the webhook notifier's original wire format — do not change tags without
// a compatibility note, external consumers parse them.
type Payload struct {
	Title            string          `json:"title"`
	Confidence       float64         `json:"confidence"`
	Namespace        string          `json:"namespace,omitempty"`
	Resource         string          `json:"resource,omitempty"`
	CuratedURL       string          `json:"curated_url,omitempty"`
	Text             string          `json:"text"`
	Verdict          string          `json:"verdict,omitempty"`
	Severity         string          `json:"severity,omitempty"`
	Cluster          string          `json:"cluster,omitempty"`
	Environment      string          `json:"environment,omitempty"`
	Tenant           string          `json:"tenant,omitempty"`
	AlertName        string          `json:"alert_name,omitempty"`
	StartedAt        string          `json:"started_at,omitempty"` // RFC3339; "" when unknown
	Occurrences      int             `json:"occurrences,omitempty"`
	PrevCuratedURL   string          `json:"prev_curated_url,omitempty"`
	RuledOut         []string        `json:"ruled_out,omitempty"`
	DataGaps         []string        `json:"data_gaps,omitempty"`
	Prior            *PriorPayload   `json:"prior,omitempty"`
	MatchedKnowledge *MatchedPayload `json:"matched_knowledge,omitempty"`
}

// MatchedPayload mirrors providers.MatchedEntry (see webhook package docs).
type MatchedPayload struct {
	Path  string  `json:"path,omitempty"`
	Title string  `json:"title,omitempty"`
	URL   string  `json:"url,omitempty"`
	Score float64 `json:"score,omitempty"`
}

// PriorPayload mirrors providers.PriorKnowledge (see webhook package docs).
type PriorPayload struct {
	Cause      string `json:"cause,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	EntryPath  string `json:"entry_path,omitempty"`
	Recalls    int    `json:"recalls,omitempty"`
	Resolved   int    `json:"resolved,omitempty"`
}

// NewPayload maps an (already-redacted) Investigation to the delivery payload.
// Matched knowledge is surfaced only when Prior is nil so the structured field
// never disagrees with the rendered Text (Prior already covers "seen before").
func NewPayload(inv providers.Investigation) Payload {
	startedAt := ""
	if !inv.StartedAt.IsZero() {
		startedAt = inv.StartedAt.UTC().Format(time.RFC3339)
	}
	var prior *PriorPayload
	if p := inv.Prior; p != nil {
		prior = &PriorPayload{Cause: p.Cause, Resolution: p.Resolution, EntryPath: p.EntryPath, Recalls: p.Recalls, Resolved: p.Resolved}
	}
	var matched *MatchedPayload
	if mk := inv.MatchedKnowledge; mk != nil && inv.Prior == nil {
		matched = &MatchedPayload{Path: mk.Path, Title: mk.Title, URL: mk.URL, Score: mk.Score}
	}
	return Payload{
		Title: inv.Title, Confidence: inv.Confidence,
		Namespace: inv.Resource.Namespace, Resource: inv.Resource.Name,
		CuratedURL: inv.CuratedURL, Text: Format(inv), Verdict: string(inv.Verdict),
		Severity: inv.Severity, Cluster: inv.Cluster, Environment: inv.Environment,
		Tenant: inv.Tenant, AlertName: inv.AlertName, StartedAt: startedAt,
		Occurrences: inv.Occurrences, PrevCuratedURL: inv.PrevCuratedURL,
		RuledOut: inv.RuledOut, DataGaps: inv.DataGaps,
		Prior: prior, MatchedKnowledge: matched,
	}
}
```

Then in `internal/notify/webhook/webhook.go`: delete the local `payload`/`priorPayload`/`matchedPayload` types and the mapping block; `Deliver` becomes:

```go
// Deliver marshals the investigation to JSON and POSTs it to the configured URL.
func (n *Notifier) Deliver(ctx context.Context, inv providers.Investigation) error {
	body, err := json.Marshal(notify.NewPayload(inv))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return &deliverError{status: resp.StatusCode}
	}
	return nil
}
```

Drop now-unused imports (`time` stays only if still used).

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/notify/... -v -run 'TestNewPayloadMapping|TestWebhook'`
Expected: PASS, including every pre-existing webhook test UNCHANGED (byte-identical wire format is the point).

- [ ] **Step 5: Gate + commit**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
git add internal/notify/payload.go internal/notify/payload_test.go internal/notify/webhook/webhook.go
git commit -m "refactor(notify): export the delivery payload as notify.Payload"
```

---

### Task 2: Templated notifier — config, parse-at-startup, registry

**Files:**
- Create: `internal/notify/templated/templated.go`
- Create: `internal/notify/templated/templated_test.go`
- Modify: `internal/app/serve.go:28` area (add blank import below the webhook one)

**Interfaces:**
- Consumes: `notify.Register`, `notify.Deps`, `notify.Payload`, `d.Cfg.Notify.Extra["templated"]` (`config.go:551` inline map — no config-struct change), `httpx.SecureClient`.
- Produces: package `templated`; `Notifier` (unexported instances inside); config schema:

```yaml
notify:
  templated:
    - name: teams                    # required, unique per instance; appears in logs/errors
      url_env: RUNLORE_TEAMS_URL     # required; env var holding the endpoint URL (unset ⇒ instance disabled)
      token_env: RUNLORE_TEAMS_TOKEN # optional; sent as "Authorization: Bearer <value>"
      content_type: application/json # optional; default application/json
      template: |                    # required; Go text/template over notify.Payload
        {"text": {{ toJSON (printf "[%s] %s — %.0f%%" .Verdict .Title (mulPct .Confidence)) }}}
```

Template funcs: exactly two — `toJSON` (marshal any value to a JSON fragment; the escaping-correct way to splice text into JSON bodies) and `mulPct` (×100 for percent display). Parse errors and schema errors (missing/duplicate name, missing url_env/template) return an error from `Build`, which `BuildEnabled` wraps (`registry.go:47-48`) and `app.BuildNotifier` propagates — serve startup fails, satisfying "template parse errors fail config validation at startup".

- [ ] **Step 1: Write the failing tests** — `internal/notify/templated/templated_test.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package templated

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func decodeExtra(t *testing.T, y string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(y), &n); err != nil {
		t.Fatal(err)
	}
	return *n.Content[0] // unwrap the document node
}

func TestBuildParsesInstances(t *testing.T) {
	t.Setenv("T_URL", "https://example.com/hook")
	node := decodeExtra(t, `
- name: teams
  url_env: T_URL
  template: '{"text": {{ toJSON .Title }}}'
`)
	n, err := build(node)
	if err != nil || n == nil || len(n.instances) != 1 {
		t.Fatalf("n=%+v err=%v", n, err)
	}
	if got := n.instances[0].contentType; got != "application/json" {
		t.Errorf("default content_type = %q", got)
	}
}

func TestBuildFailsClosedOnBadConfig(t *testing.T) {
	t.Setenv("T_URL", "https://example.com/hook")
	for name, y := range map[string]string{
		"parse error":    "- {name: a, url_env: T_URL, template: '{{ .Title }'}",
		"missing name":   "- {url_env: T_URL, template: ok}",
		"missing url":    "- {name: a, template: ok}",
		"missing tmpl":   "- {name: a, url_env: T_URL}",
		"duplicate name": "- {name: a, url_env: T_URL, template: ok}\n- {name: a, url_env: T_URL, template: ok}",
	} {
		if _, err := build(decodeExtra(t, y)); err == nil {
			t.Errorf("%s: want error, got nil", name)
		}
	}
}

func TestBuildDisablesInstanceOnUnsetEnv(t *testing.T) {
	node := decodeExtra(t, "- {name: a, url_env: T_UNSET_NEVER, template: ok}")
	n, err := build(node)
	if err != nil {
		t.Fatal(err)
	}
	if n != nil {
		t.Errorf("all instances env-disabled ⇒ nil notifier, got %+v", n)
	}
	_ = strings.TrimSpace // keep imports honest if unused later
}
```

(Drop the trailing `strings` line if `strings` ends up genuinely used/unused — the gate's lint decides.)

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/notify/templated/ -v`
Expected: FAIL — `undefined: build`

- [ ] **Step 3: Implement** — `internal/notify/templated/templated.go`:

```go
// SPDX-License-Identifier: Apache-2.0

// Package templated is a config-only generic notifier: it renders a
// user-supplied Go text/template over notify.Payload and POSTs the result to
// any endpoint (Teams, Discord, ntfy, incident.io, …). One YAML block per
// target — no Go, no rebuild. The Investigation is already secret-redacted
// before any notifier runs (investigate.redactInvestigation), so templates
// only ever see redacted data.
package templated

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"text/template"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"
)

// maxBody caps a rendered template. Oversize is an error, not a truncation —
// a truncated JSON/XML body is garbage to the receiver; failing loudly beats
// posting it.
const maxBody = 256 << 10

var funcs = template.FuncMap{
	// toJSON is the escaping-correct way to splice a value into a JSON body.
	"toJSON": func(v any) (string, error) {
		b, err := json.Marshal(v)
		return string(b), err
	},
	"mulPct": func(f float64) float64 { return f * 100 },
}

type instanceCfg struct {
	Name        string `yaml:"name"`
	URLEnv      string `yaml:"url_env"`
	TokenEnv    string `yaml:"token_env"`
	ContentType string `yaml:"content_type"`
	Template    string `yaml:"template"`
}

type instance struct {
	name        string
	url         string
	token       string
	contentType string
	tmpl        *template.Template
}

// Notifier fans one delivery out to every configured template instance.
type Notifier struct {
	instances []instance
	client    *http.Client
}

var _ providers.Notifier = (*Notifier)(nil)

// build decodes and validates the notify.templated block. Schema/template
// errors are returned (⇒ BuildEnabled fails ⇒ serve refuses to start); an
// instance whose url_env is unset is silently disabled (webhook parity).
func build(node yaml.Node) (*Notifier, error) {
	var cfgs []instanceCfg
	if err := node.Decode(&cfgs); err != nil {
		return nil, fmt.Errorf("templated: %w", err)
	}
	seen := map[string]bool{}
	var ins []instance
	for i, c := range cfgs {
		if c.Name == "" || c.URLEnv == "" || c.Template == "" {
			return nil, fmt.Errorf("templated[%d]: name, url_env and template are required", i)
		}
		if seen[c.Name] {
			return nil, fmt.Errorf("templated: duplicate instance name %q", c.Name)
		}
		seen[c.Name] = true
		tmpl, err := template.New(c.Name).Funcs(funcs).Parse(c.Template)
		if err != nil {
			return nil, fmt.Errorf("templated %q: parse template: %w", c.Name, err)
		}
		url := os.Getenv(c.URLEnv)
		if url == "" {
			continue // env unset ⇒ this instance is disabled
		}
		ct := c.ContentType
		if ct == "" {
			ct = "application/json"
		}
		token := ""
		if c.TokenEnv != "" {
			token = os.Getenv(c.TokenEnv)
		}
		ins = append(ins, instance{name: c.Name, url: url, token: token, contentType: ct, tmpl: tmpl})
	}
	if len(ins) == 0 {
		return nil, nil // nothing enabled
	}
	return &Notifier{instances: ins, client: httpx.SecureClient(10 * time.Second)}, nil
}

func init() {
	notify.Register(notify.Descriptor{
		Name: "templated",
		Build: func(d notify.Deps) (providers.Notifier, error) {
			node, ok := d.Cfg.Notify.Extra["templated"]
			if !ok {
				return nil, nil
			}
			n, err := build(node)
			if err != nil || n == nil {
				return nil, err // typed-nil guard: never return (*Notifier)(nil) as a non-nil interface
			}
			return n, nil
		},
	})
}
```

NOTE the typed-nil guard in `init`: `build` returns `*Notifier`; returning it directly when nil would hand `BuildEnabled` a non-nil interface wrapping a nil pointer.

Check the exact `Notify.Extra` value type in `config.go:551` — if it is `map[string]yaml.Node` (not pointers), the `Build` closure body above is correct as written; mirror how `webhook.go:160-166` decodes.

Add the blank import in `internal/app/serve.go` directly under line 28:

```go
	_ "github.com/Smana/runlore/internal/notify/templated" // self-registers the templated notifier
```

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/notify/templated/ -v`
Expected: PASS (3 tests)

- [ ] **Step 5: Gate + commit**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
git add internal/notify/templated/ internal/app/serve.go
git commit -m "feat(notify): templated notifier — parse-at-startup config and registry entry"
```

---

### Task 3: Templated delivery — render, cap, POST

**Files:**
- Modify: `internal/notify/templated/templated.go` (add `Deliver`)
- Modify: `internal/notify/templated/templated_test.go`

**Interfaces:**
- Consumes: `notify.NewPayload` (Task 1), `instance`/`Notifier` (Task 2).
- Produces: `(*Notifier).Deliver(ctx, inv) error` — renders each instance's template over `notify.NewPayload(inv)`, caps at `maxBody`, POSTs with the instance's content-type and optional bearer; per-instance errors are joined (one bad instance never blocks the others — mirroring `Multi.Deliver`'s isolation one level down).

- [ ] **Step 1: Write the failing tests** (append to `templated_test.go`; add imports `context`, `net/http`, `net/http/httptest`, `github.com/Smana/runlore/internal/providers`):

```go
func testNotifier(t *testing.T, tmplBody, url string) *Notifier {
	t.Helper()
	t.Setenv("T_URL", url)
	n, err := build(decodeExtra(t, "- name: teams\n  url_env: T_URL\n  token_env: T_TOK\n  template: '"+tmplBody+"'"))
	if err != nil || n == nil {
		t.Fatalf("build: n=%v err=%v", n, err)
	}
	return n
}

func TestDeliverRendersAndPosts(t *testing.T) {
	t.Setenv("T_TOK", "sekret")
	var gotBody, gotCT, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody, gotCT, gotAuth = string(b), r.Header.Get("Content-Type"), r.Header.Get("Authorization")
	}))
	defer srv.Close()
	n := testNotifier(t, `{"text": {{ toJSON .Title }}}`, srv.URL)
	inv := providers.Investigation{Title: `quote " and \ slash`}
	if err := n.Deliver(context.Background(), inv); err != nil {
		t.Fatal(err)
	}
	if gotBody != `{"text": "quote \" and \\ slash"}` {
		t.Errorf("body = %s", gotBody) // toJSON must escape — raw splice would be JSON injection
	}
	if gotCT != "application/json" || gotAuth != "Bearer sekret" {
		t.Errorf("ct=%q auth=%q", gotCT, gotAuth)
	}
}

func TestDeliverExecErrorFailsLoudWithoutPost(t *testing.T) {
	posted := false
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { posted = true }))
	defer srv.Close()
	n := testNotifier(t, `{{ .NoSuchField }}`, srv.URL) // parses fine, fails at exec
	if err := n.Deliver(context.Background(), providers.Investigation{Title: "x"}); err == nil {
		t.Error("want exec error")
	}
	if posted {
		t.Error("exec error must not POST")
	}
}

func TestDeliverNon2xxAndSizeCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	if err := testNotifier(t, `ok`, srv.URL).Deliver(context.Background(), providers.Investigation{}); err == nil {
		t.Error("want non-2xx error")
	}
	big := testNotifier(t, `{{ .Title }}`, srv.URL)
	inv := providers.Investigation{Title: strings.Repeat("A", maxBody+1)}
	if err := big.Deliver(context.Background(), inv); err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("want size-cap error, got %v", err)
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/notify/templated/ -run TestDeliver -v`
Expected: FAIL — `n.Deliver undefined`

- [ ] **Step 3: Implement** (append to `templated.go`):

```go
// Deliver renders and POSTs every enabled instance; a failing instance is
// reported but never blocks its siblings (errors are joined, mirroring
// notify.Multi one level down).
func (n *Notifier) Deliver(ctx context.Context, inv providers.Investigation) error {
	p := notify.NewPayload(inv)
	var errs []error
	for _, in := range n.instances {
		if err := n.deliverOne(ctx, in, p); err != nil {
			errs = append(errs, fmt.Errorf("templated %q: %w", in.name, err))
		}
	}
	return errors.Join(errs...)
}

func (n *Notifier) deliverOne(ctx context.Context, in instance, p notify.Payload) error {
	var buf bytes.Buffer
	if err := in.tmpl.Execute(&buf, p); err != nil {
		return fmt.Errorf("execute template: %w", err)
	}
	if buf.Len() > maxBody {
		return fmt.Errorf("rendered body %d bytes exceeds cap %d", buf.Len(), maxBody)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, in.url, &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", in.contentType)
	if in.token != "" {
		req.Header.Set("Authorization", "Bearer "+in.token)
	}
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	return nil
}
```

(httptest servers listen on 127.0.0.1 — if `httpx.SecureClient`'s public-origin SSRF guard rejects loopback, mirror how `webhook_test.go` already solves this for its own httptest deliveries; that test file is the authoritative pattern. If it swaps the client, add the same seam here: an unexported `client` field is already in place.)

- [ ] **Step 4: Run the tests**

Run: `go test ./internal/notify/templated/ -v`
Expected: PASS (6 tests)

- [ ] **Step 5: Gate + commit**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
git add internal/notify/templated/
git commit -m "feat(notify): templated delivery — render, cap, and post"
```

---

### Task 4: Registry pinning + docs (Teams example)

**Files:**
- Modify: `internal/notify/registry_test.go` (round-trip pins)
- Modify: `docs/configuration.md` (templated section + Teams example + the two config surfaces)

**Interfaces:**
- Consumes: everything above; `config.Config` YAML loading (mirror how existing config tests build a `*config.Config` — reuse the package's established fixture idiom, do not invent one).

- [ ] **Step 1: Write the failing test** (append to `internal/notify/registry_test.go`; this is the "unification can never regress" pin — construction for slack, matrix, webhook AND templated all flows through `BuildEnabled`, with the existing `notify.slack`/`notify.matrix` keys working verbatim):

```go
func TestBuildEnabledRoundTripAllNotifiers(t *testing.T) {
	t.Setenv("RT_SLACK", "https://hooks.slack.example/x")
	t.Setenv("RT_MATRIX_TOK", "syt_x")
	t.Setenv("RT_WEBHOOK", "https://sink.example/w")
	t.Setenv("RT_TEAMS", "https://teams.example/hook")
	y := `
notify:
  slack:
    webhook_url_env: RT_SLACK
  matrix:
    homeserver: https://m.example
    room_id: "!r:m.example"
    access_token_env: RT_MATRIX_TOK
  webhook:
    url_env: RT_WEBHOOK
  templated:
    - name: teams
      url_env: RT_TEAMS
      template: '{"text": {{ toJSON .Title }}}'
`
	var cfg config.Config
	if err := yaml.Unmarshal([]byte(y), &cfg); err != nil {
		t.Fatal(err)
	}
	m, err := BuildEnabled(Deps{Cfg: &cfg, Log: slog.New(slog.DiscardHandler)})
	if err != nil {
		t.Fatal(err)
	}
	if got := m.Len(); got != 4 {
		t.Errorf("BuildEnabled built %d notifiers, want 4 (slack, matrix, webhook, templated)", got)
	}
}
```

Notes for the implementer: (a) this test lives in package `notify`, so importing `internal/notify/webhook` or `.../templated` for their `init()` side effects needs blank imports — if that creates an import cycle (webhook imports notify), move this test to `internal/notify/registry_roundtrip_test.go` with `package notify_test` and import notify, webhook, templated, config; (b) if `Multi` has no `Len()`, add the trivial accessor `func (m *Multi) Len() int { return len(m.notifiers) }` next to `NewMulti` (same package) rather than exporting the slice; (c) mirror the yaml/slog idioms of the surrounding tests (check `slog.DiscardHandler` availability in this Go version — the existing tests show the package's chosen pattern; copy it).

- [ ] **Step 2: Run to verify failure**

Run: `go test ./internal/notify/... -run TestBuildEnabledRoundTripAllNotifiers -v`
Expected: FAIL (missing `Len` and/or blank imports), then PASS after wiring exactly that.

- [ ] **Step 3: Implement the minimal pieces** (`Len()` accessor + blank imports / test relocation per Step-1 notes) and re-run to PASS.

- [ ] **Step 4: Docs** — `docs/configuration.md`, in the notify section, add:

```markdown
### Generic templated notifier (`notify.templated`)

Deliver findings to **any** webhook-speaking service — Microsoft Teams, Discord,
ntfy, incident.io — with one config block and no Go. Each instance renders a Go
`text/template` over the delivery payload (the same fields the `notify.webhook`
JSON carries) and POSTs it. Findings are secret-redacted **before** any notifier
runs, so templates only ever see redacted data. A template that fails to parse
refuses startup; a template that fails at delivery time is logged and skipped
without blocking other channels. Rendered bodies are capped at 256 KiB.

Worked example — Microsoft Teams (Incoming Webhook / MessageCard):

    notify:
      templated:
        - name: teams
          url_env: RUNLORE_TEAMS_WEBHOOK_URL
          template: |
            {
              "@type": "MessageCard", "@context": "https://schema.org/extensions",
              "summary": {{ toJSON .Title }},
              "themeColor": "d63333",
              "title": {{ toJSON (printf "[%s] %s (%.0f%%)" .Verdict .Title (mulPct .Confidence)) }},
              "text": {{ toJSON .Text }}
            }

Template functions: `toJSON` (escaping-correct JSON splicing — always use it for
values inside JSON bodies) and `mulPct` (×100 for percent display).

**Two config surfaces, one registry.** Built-in notifiers (`notify.slack`,
`notify.matrix`) use typed config blocks with startup validation; drop-in
notifiers (`notify.webhook`, `notify.templated`) live under the same `notify:`
key as self-describing blocks. Both build through the same registry — the split
is deliberate: type-checked config for the built-ins, zero-code extensibility
for everything else.
```

- [ ] **Step 5: Full gate + race + commit**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
go test -race ./internal/notify/... ./internal/config/... ./internal/app/...
git add internal/notify/ docs/configuration.md
git commit -m "test(notify): pin registry round-trip for all notifiers; document templated delivery"
```

---

## Self-review checklist (run before opening the PR)

1. **Wire-format freeze:** `git diff main -- internal/notify/webhook/` shows only the payload-type removal and the `NewPayload` call — every pre-existing webhook test passes unchanged.
2. **Startup-fail proof:** `TestBuildFailsClosedOnBadConfig` covers parse + schema errors; confirm manually that `BuildEnabled` wraps them (`notify "templated": …`) so `lore serve` refuses to start.
3. **No new deps:** `git diff main -- go.mod` is empty.
4. **Placeholder scan:** no TBD/TODO/"similar to Task N" anywhere in the diff.
5. **Registry pin honest:** the round-trip test genuinely exercises `notify.slack`/`notify.matrix` keys verbatim (zero user-visible change is asserted, not assumed).
