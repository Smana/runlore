# Slack / Matrix Delivery Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Deliver completed investigations to chat. Implement the `providers.Notifier` interface for **Slack** (incoming webhook) and **Matrix** (client-server `send` API), a shared Investigation→markdown formatter, and a best-effort fan-out; then wire `serve`'s `OnComplete` to deliver to all configured notifiers.

**Architecture:** One `internal/notify` package: `Format(Investigation) string` (shared), a `Slack` notifier (webhook POST), a `Matrix` notifier (authenticated PUT to the room `send` endpoint), and `Multi` (best-effort fan-out that logs per-notifier errors). Both notifiers are hand-rolled HTTP, **`httptest`-tested** — CI-safe, no real tokens. `serve` builds the configured notifiers (secrets via env) and sets the investigator's `OnComplete` to deliver to all.

**Tech Stack:** Go 1.26 stdlib (`net/http`, `encoding/json`, `net/http/httptest`). Contracts: `providers.Notifier`/`Investigation`/`Hypothesis`; `config`; `investigate.LoopInvestigator.OnComplete`.

---

## File Structure

| File | Responsibility |
|---|---|
| `internal/notify/format.go` + `_test.go` *(create)* | `Format(Investigation) string` |
| `internal/notify/slack.go` + `_test.go` *(create)* | `Slack` notifier (incoming webhook) + `Multi` fan-out |
| `internal/notify/matrix.go` + `_test.go` *(create)* | `Matrix` notifier (client-server send) |
| `internal/config/config.go` *(modify)* | `Notify` config |
| `cmd/lore/main.go` *(modify)* | build notifiers; `OnComplete` → deliver to all |

---

## Task 1: Formatter + Slack notifier + fan-out

**Files:**
- Create: `internal/notify/format.go`, `internal/notify/format_test.go`, `internal/notify/slack.go`, `internal/notify/slack_test.go`

- [ ] **Step 1: Write the failing tests**

Create `internal/notify/format_test.go`:

```go
package notify

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func sampleInvestigation() providers.Investigation {
	return providers.Investigation{
		Confidence: 0.82,
		RootCauses: []providers.Hypothesis{{
			Summary: "chart 1.15 enabled DB migrations; harbor-db CrashLoopBackOff",
			Confidence: 0.82, Evidence: []string{"pg_up=0", "migration lock timeout"},
			SuggestedAction: "flux rollback hr/harbor", Reversible: true,
		}},
		Unresolved: []string{"why the migration lock never released"},
	}
}

func TestFormat(t *testing.T) {
	out := Format(sampleInvestigation())
	for _, want := range []string{"82%", "chart 1.15", "pg_up=0", "flux rollback hr/harbor", "reversible", "why the migration lock"} {
		if !strings.Contains(out, want) {
			t.Fatalf("formatted message missing %q:\n%s", want, out)
		}
	}
}
```

Create `internal/notify/slack_test.go`:

```go
package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestSlackDeliver(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	if err := NewSlack(srv.URL).Deliver(context.Background(), sampleInvestigation()); err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	text, _ := got["text"].(string)
	if text == "" || !contains(text, "flux rollback hr/harbor") {
		t.Fatalf("unexpected slack payload: %v", got)
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (s == sub || indexOf(s, sub) >= 0) }
func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}

// failingNotifier always errors.
type failingNotifier struct{}

func (failingNotifier) Deliver(context.Context, providers.Investigation) error {
	return io.ErrUnexpectedEOF
}

func TestMultiBestEffort(t *testing.T) {
	var delivered int
	ok := notifierFunc(func(context.Context, providers.Investigation) error { delivered++; return nil })
	m := NewMulti(slog.New(slog.NewTextHandler(io.Discard, nil)), failingNotifier{}, ok)
	// Must not return an error even though one notifier fails, and must still call the good one.
	if err := m.Deliver(context.Background(), sampleInvestigation()); err != nil {
		t.Fatalf("Multi.Deliver returned error: %v", err)
	}
	if delivered != 1 {
		t.Fatalf("good notifier called %d times, want 1", delivered)
	}
}

// notifierFunc adapts a func to providers.Notifier.
type notifierFunc func(context.Context, providers.Investigation) error

func (f notifierFunc) Deliver(ctx context.Context, inv providers.Investigation) error { return f(ctx, inv) }
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/notify/ -v`
Expected: FAIL — package/symbols undefined.

- [ ] **Step 3: Implement format, Slack, Multi**

Create `internal/notify/format.go`:

```go
// Package notify delivers completed investigations to chat (Slack, Matrix).
package notify

import (
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// Format renders an Investigation as a concise markdown-ish message used by all
// notifiers.
func Format(inv providers.Investigation) string {
	var b strings.Builder
	fmt.Fprintf(&b, "*Investigation* — confidence %.0f%%\n", inv.Confidence*100)
	for i, rc := range inv.RootCauses {
		fmt.Fprintf(&b, "%d. *%s* (%.0f%%)\n", i+1, rc.Summary, rc.Confidence*100)
		for _, e := range rc.Evidence {
			fmt.Fprintf(&b, "   • %s\n", e)
		}
		if rc.SuggestedAction != "" {
			rev := ""
			if rc.Reversible {
				rev = " (reversible)"
			}
			fmt.Fprintf(&b, "   → suggested: %s%s\n", rc.SuggestedAction, rev)
		}
	}
	if len(inv.Unresolved) > 0 {
		b.WriteString("*Unresolved:*\n")
		for _, u := range inv.Unresolved {
			fmt.Fprintf(&b, "   • %s\n", u)
		}
	}
	return b.String()
}
```

Create `internal/notify/slack.go`:

```go
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// Slack delivers via a Slack incoming webhook.
type Slack struct {
	webhookURL string
	http       *http.Client
}

// NewSlack builds a Slack webhook notifier.
func NewSlack(webhookURL string) *Slack {
	return &Slack{webhookURL: webhookURL, http: &http.Client{Timeout: 15 * time.Second}}
}

var _ providers.Notifier = (*Slack)(nil)

// Deliver posts the formatted investigation to the webhook.
func (s *Slack) Deliver(ctx context.Context, inv providers.Investigation) error {
	body, err := json.Marshal(map[string]string{"text": Format(inv)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack status %d", resp.StatusCode)
	}
	return nil
}

// Multi delivers to several notifiers, best-effort: a failing notifier is logged,
// not propagated, so one bad sink doesn't block the others.
type Multi struct {
	notifiers []providers.Notifier
	log       *slog.Logger
}

// NewMulti builds a fan-out notifier.
func NewMulti(log *slog.Logger, notifiers ...providers.Notifier) *Multi {
	return &Multi{notifiers: notifiers, log: log}
}

var _ providers.Notifier = (*Multi)(nil)

// Deliver fans out to every notifier; errors are logged, never returned.
func (m *Multi) Deliver(ctx context.Context, inv providers.Investigation) error {
	for _, n := range m.notifiers {
		if err := n.Deliver(ctx, inv); err != nil {
			m.log.Error("delivery failed", "err", err)
		}
	}
	return nil
}

// Len reports how many notifiers are configured.
func (m *Multi) Len() int { return len(m.notifiers) }
```

- [ ] **Step 4: Run + gate + commit**

Run: `cd /home/smana/Sources/runlore && go test ./internal/notify/ -v && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: PASS; `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/notify/
git commit -m "feat(notify): Investigation formatter + Slack webhook notifier + best-effort fan-out"
```

---

## Task 2: Matrix notifier

**Files:**
- Create: `internal/notify/matrix.go`, `internal/notify/matrix_test.go`

- [ ] **Step 1: Write the failing test**

Create `internal/notify/matrix_test.go`:

```go
package notify

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestMatrixDeliver(t *testing.T) {
	var gotPath, gotAuth string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		_, _ = w.Write([]byte(`{"event_id":"$abc"}`))
	}))
	defer srv.Close()

	err := NewMatrix(srv.URL, "!room:hs", "tok").Deliver(context.Background(), sampleInvestigation())
	if err != nil {
		t.Fatalf("Deliver: %v", err)
	}
	if !strings.Contains(gotPath, "/_matrix/client/v3/rooms/") || !strings.Contains(gotPath, "/send/m.room.message/") {
		t.Fatalf("unexpected path: %s", gotPath)
	}
	if gotAuth != "Bearer tok" {
		t.Fatalf("auth = %q", gotAuth)
	}
	if mt, _ := gotBody["msgtype"].(string); mt != "m.notice" {
		t.Fatalf("msgtype = %v", gotBody["msgtype"])
	}
	if body, _ := gotBody["body"].(string); !strings.Contains(body, "flux rollback hr/harbor") {
		t.Fatalf("body missing content: %v", gotBody["body"])
	}
}
```

- [ ] **Step 2: Run to verify failure**

Run: `cd /home/smana/Sources/runlore && go test ./internal/notify/ -run TestMatrixDeliver -v`
Expected: FAIL — `NewMatrix` undefined.

- [ ] **Step 3: Implement the Matrix notifier**

Create `internal/notify/matrix.go`:

```go
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// Matrix delivers via the Matrix client-server send API.
type Matrix struct {
	homeserver string
	roomID     string
	token      string
	http       *http.Client
	txn        atomic.Int64
}

// NewMatrix builds a Matrix notifier. homeserver is the base URL (e.g.
// https://matrix.org); roomID is like "!abc:hs"; token is an access token.
func NewMatrix(homeserver, roomID, token string) *Matrix {
	return &Matrix{
		homeserver: strings.TrimRight(homeserver, "/"),
		roomID:     roomID,
		token:      token,
		http:       &http.Client{Timeout: 15 * time.Second},
	}
}

var _ providers.Notifier = (*Matrix)(nil)

// Deliver sends the formatted investigation as an m.notice message.
func (m *Matrix) Deliver(ctx context.Context, inv providers.Investigation) error {
	txn := fmt.Sprintf("runlore-%d", m.txn.Add(1))
	endpoint := fmt.Sprintf("%s/_matrix/client/v3/rooms/%s/send/m.room.message/%s",
		m.homeserver, url.PathEscape(m.roomID), url.PathEscape(txn))

	body, err := json.Marshal(map[string]string{"msgtype": "m.notice", "body": Format(inv)})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+m.token)
	resp, err := m.http.Do(req)
	if err != nil {
		return fmt.Errorf("matrix send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("matrix status %d", resp.StatusCode)
	}
	return nil
}
```

- [ ] **Step 4: Run + gate + commit**

Run: `cd /home/smana/Sources/runlore && go test ./internal/notify/ -v && go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: PASS; `0 issues`.

```bash
cd /home/smana/Sources/runlore
git add internal/notify/matrix.go internal/notify/matrix_test.go
git commit -m "feat(notify): Matrix client-server notifier"
```

---

## Task 3: Config + deliver findings in `serve`

**Files:**
- Modify: `internal/config/config.go`, `cmd/lore/main.go`

- [ ] **Step 1: Add the notify config**

In `internal/config/config.go`, add the types and the top-level field:

```go
	Notify Notify `yaml:"notify"` // chat delivery for findings
```

```go
// Notify configures where investigation findings are delivered.
type Notify struct {
	Slack  SlackNotify  `yaml:"slack"`
	Matrix MatrixNotify `yaml:"matrix"`
}

// SlackNotify configures Slack incoming-webhook delivery.
type SlackNotify struct {
	WebhookURLEnv string `yaml:"webhook_url_env"` // env var holding the webhook URL
}

// MatrixNotify configures Matrix delivery.
type MatrixNotify struct {
	Homeserver     string `yaml:"homeserver"`
	RoomID         string `yaml:"room_id"`
	AccessTokenEnv string `yaml:"access_token_env"` // env var holding the access token
}
```

- [ ] **Step 2: Wire `OnComplete` to deliver**

In `cmd/lore/main.go`, build the notifiers in `buildInvestigator` and replace the `OnComplete` log with delivery. Add a helper `buildNotifier` and update `buildInvestigator`'s `OnComplete`:

```go
// buildNotifier assembles the configured chat notifiers (best-effort fan-out).
func buildNotifier(cfg *config.Config, log *slog.Logger) *notify.Multi {
	var ns []providers.Notifier
	if env := cfg.Notify.Slack.WebhookURLEnv; env != "" {
		if url := os.Getenv(env); url != "" {
			ns = append(ns, notify.NewSlack(url))
		}
	}
	if mc := cfg.Notify.Matrix; mc.Homeserver != "" && mc.RoomID != "" && mc.AccessTokenEnv != "" {
		if tok := os.Getenv(mc.AccessTokenEnv); tok != "" {
			ns = append(ns, notify.NewMatrix(mc.Homeserver, mc.RoomID, tok))
		}
	}
	return notify.NewMulti(log, ns...)
}
```

In `buildInvestigator`, build the notifier once and use it in `OnComplete` (it already receives `ctx`? if not, capture a background ctx for delivery). Replace the `OnComplete` field with:

```go
	notifier := buildNotifier(cfg, log)
	log.Info("delivery notifiers", "count", notifier.Len())
	return &investigate.LoopInvestigator{
		Model: model,
		Tools: tools,
		Log:   log,
		OnComplete: func(found providers.Investigation) {
			log.Info("findings", "confidence", found.Confidence, "root_causes", len(found.RootCauses), "unresolved", len(found.Unresolved))
			if err := notifier.Deliver(context.Background(), found); err != nil {
				log.Error("deliver findings", "err", err)
			}
		},
	}
```

Add the `"github.com/Smana/runlore/internal/notify"` import (and `"context"` if not already imported).

- [ ] **Step 3: `go mod tidy`, gate, smoke**

Run:
```bash
cd /home/smana/Sources/runlore
go mod tidy
go build ./... && go vet ./... && go test ./... -count=1 && gofmt -l . && golangci-lint run ./...
```
Expected: clean, `0 issues`. (No new smoke needed — the no-model path is unchanged; the notifier set is empty without env vars and `Multi.Deliver` is a no-op. Optionally confirm `lore serve --config /tmp/rl.yaml` still starts and logs `delivery notifiers count=0`.)

- [ ] **Step 4: Commit**

```bash
cd /home/smana/Sources/runlore
git add internal/config/config.go cmd/lore/main.go go.mod go.sum
git commit -m "feat(serve): deliver findings to configured Slack/Matrix notifiers"
```

---

## What this plan delivers

Completed investigations are delivered to chat: `serve` builds the configured Slack (webhook) and Matrix (client-server) notifiers, and `OnComplete` fans out to all of them best-effort. No notifier configured → a no-op `Multi`. The full path now runs **trigger → policy → workqueue → ReAct loop → findings → Slack/Matrix.**

## Next plans (not in this plan)

- Richer Slack formatting (Block Kit + the `[Ack]`/`[Open rollback PR]` actions from the design), Slack threading.
- The **Curator** (issue/PR write loop) and **catalog** (kb_search) — the Learn pillar.
- Secrets via External Secrets rather than env vars.

---

## Self-Review

- **Spec coverage:** `providers.Notifier` impls for Slack (webhook) + Matrix (client-server), shared `Format`, best-effort `Multi`, wired into `serve`'s `OnComplete`. All notifiers `httptest`-tested (CI-safe). Block Kit / Curator / catalog are named follow-ups. ✅
- **Placeholder scan:** Complete code per step; the `OnComplete` log is retained alongside delivery (useful), not a stub. ✅
- **Type consistency:** `Slack`/`Matrix`/`Multi` satisfy `providers.Notifier` (compile-time checks); `Format` shared; `config.Notify` consumed by `buildNotifier`; `OnComplete` signature unchanged (`func(providers.Investigation)`), delivery uses `context.Background()`. ✅
