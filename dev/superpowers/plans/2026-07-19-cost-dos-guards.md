# Cost-DoS Guards Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Bound the model bill and the auth-guessing surface: default the investigation
rate limit on, cap how many investigation requests one webhook payload can enqueue, and
back off repeated failed authentications per remote host.

**Architecture:** Three independent guards, one task each. (1) `investigation.rate_limit.max_per_window`
becomes `*int` (repo pattern: `CancelQueuedOnResolve *bool`) so *unset* defaults to 30/h while an
*explicit* `0` keeps today's unlimited behavior — the existing `ratelimit.Window` wiring in
`serve.go` is unchanged beyond the deref. (2) `Built.Handler` truncates `DecodeResult.Requests`
at a constant cap after decode (resolutions exempt — cheap and outcome-critical). (3) A new
`authGuard` in `internal/server` counts consecutive failed auths per remote host and blocks
before the token compare, with exponential, capped block windows; a correct token always resets.

**Tech Stack:** Go (toolchain go1.26.5), stdlib only — no new dependencies. Reuses
`internal/ratelimit.Window` (unchanged) and the existing `source.Pipeline` logger.

## Global Constraints

- Quality gate before every commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — golangci-lint must report `0 issues`, `gofmt -l` empty; also `go test -race` on touched packages.
- SPDX header `// SPDX-License-Identifier: Apache-2.0` as line 1 of every NEW `.go` file.
- Conventional Commits; **no co-author trailer**; PR title/description in English.
- Fail-safe bias: defaults must not brick existing deployments — an explicit `max_per_window: 0` keeps unlimited; the auth guard fails open on memory pressure, never on auth; the payload cap truncates (Alertmanager re-delivers on `repeat_interval`, so a truncated legitimate batch self-heals).
- The behavior change (unset rate limit now defaults to 30/h) must be called out in the commit body and PR description — release-please builds the changelog from commits.
- Do NOT touch `Actions.Auto.MaxPerWindow` (`internal/config/config.go:805`) — different struct, already validated `> 0` under `auto`.

---

### Task 1: Default investigation rate limit (unset ⇒ 30/h; explicit 0 ⇒ unlimited)

**Files:**
- Modify: `internal/config/config.go:349-353` (RateLimit struct)
- Modify: `internal/config/load.go:89-93` (applyDefaults)
- Modify: `internal/config/config.go` `Validate` (reject negative; put it near the other investigation checks)
- Modify: `internal/app/serve.go:186-190` (deref)
- Modify: `docs/configuration.md:112`, `deploy/helm/runlore/values.yaml:154-157`
- Test: `internal/config/load_test.go`

**Interfaces:**
- Consumes: `loadDoc(t, doc)` helper (`internal/config/load_test.go:15`), `applyDefaults(c *Config)` (`load.go:38`).
- Produces: `RateLimit.MaxPerWindow *int` — every consumer must nil-check + deref. The only production consumers are `load.go` (default), `Validate`, and `serve.go:186`.

- [ ] **Step 1: Write the failing tests** (append to `internal/config/load_test.go`)

```go
// TestRateLimitDefaultsOn pins the cost-DoS default: an UNSET
// investigation.rate_limit.max_per_window defaults to 30 per 1h window. An
// unbounded default let any token-holding caller (or a misfiring Alertmanager)
// run up the model bill — per-incident cost was capped, count was not.
func TestRateLimitDefaultsOn(t *testing.T) {
	c := loadDoc(t, `
sources:
  alertmanager: {}
`)
	if c.Investigation.RateLimit.MaxPerWindow == nil {
		t.Fatal("unset max_per_window must be defaulted (non-nil) by applyDefaults")
	}
	if got := *c.Investigation.RateLimit.MaxPerWindow; got != 30 {
		t.Fatalf("unset max_per_window must default to 30, got %d", got)
	}
	if c.Investigation.RateLimit.Window != Duration(time.Hour) {
		t.Fatalf("defaulted budget must also default window to 1h, got %v", c.Investigation.RateLimit.Window)
	}
}

// TestRateLimitExplicitZeroStaysUnlimited pins backward compatibility: a deployed
// `max_per_window: 0` keeps its documented unlimited meaning after the default flip.
func TestRateLimitExplicitZeroStaysUnlimited(t *testing.T) {
	c := loadDoc(t, `
sources:
  alertmanager: {}
investigation:
  rate_limit:
    max_per_window: 0
`)
	if c.Investigation.RateLimit.MaxPerWindow == nil {
		t.Fatal("explicit 0 should be non-nil (distinguishable from unset)")
	}
	if got := *c.Investigation.RateLimit.MaxPerWindow; got != 0 {
		t.Fatalf("explicit max_per_window: 0 must stay 0 (unlimited), got %d", got)
	}
}

// TestRateLimitExplicitValueKept pins that an operator's value survives defaulting.
func TestRateLimitExplicitValueKept(t *testing.T) {
	c := loadDoc(t, `
sources:
  alertmanager: {}
investigation:
  rate_limit:
    max_per_window: 7
`)
	if got := *c.Investigation.RateLimit.MaxPerWindow; got != 7 {
		t.Fatalf("explicit max_per_window: 7 must be kept, got %d", got)
	}
}
```

(`loadDoc` and `Duration` are already in scope in this package's tests; `time` is already imported in `load_test.go` — verify, add if missing.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestRateLimit' -v`
Expected: compile error — `c.Investigation.RateLimit.MaxPerWindow == nil` is invalid on `int` (mismatched types). That compile failure IS the red state for a type change.

- [ ] **Step 3: Change the struct field** (`internal/config/config.go:349-353`)

```go
// RateLimit caps investigation starts per sliding window.
type RateLimit struct {
	// MaxPerWindow caps investigation STARTS per Window. nil (unset) defaults to
	// 30 (see applyDefaults) — a cost-DoS guard, since per-incident spend is
	// bounded but the count of incidents was not. An EXPLICIT 0 preserves the
	// pre-default unlimited behavior for configs that opted into it.
	MaxPerWindow *int     `yaml:"max_per_window"`
	Window       Duration `yaml:"window"`
	MaxRequeues  int      `yaml:"max_requeues"` // drop a key after this many backoff requeues
}
```

- [ ] **Step 4: Default it in applyDefaults** (`internal/config/load.go:89-93`, replacing the existing window-default block)

```go
	// Investigation rate limit: UNSET defaults to 30/h (cost-DoS guard — the count
	// of investigations was unbounded out of the box; the Helm chart already ships
	// an explicit 20). An explicit 0 keeps the documented unlimited meaning.
	if c.Investigation.RateLimit.MaxPerWindow == nil {
		n := 30
		c.Investigation.RateLimit.MaxPerWindow = &n
	}
	// Rate-limit window default: 1h when a per-window budget is in effect but no
	// window is given (a zero window would silently allow unlimited investigations).
	if *c.Investigation.RateLimit.MaxPerWindow > 0 && c.Investigation.RateLimit.Window == 0 {
		c.Investigation.RateLimit.Window = Duration(time.Hour)
	}
```

- [ ] **Step 5: Reject negatives in Validate** (`internal/config/config.go`, alongside the other `investigation.*` validation)

```go
	if mpw := c.Investigation.RateLimit.MaxPerWindow; mpw != nil && *mpw < 0 {
		return fmt.Errorf("investigation.rate_limit.max_per_window must be >= 0 (0 = unlimited), got %d", *mpw)
	}
```

And a test for it (append to `load_test.go`, using the same `os.WriteFile` + `Load` shape as the explicit-zero debounce test at `load_test.go:160-181` if `loadDoc` fatals on Validate errors — check `loadDoc`; if it returns the error path, assert `Load` returns an error containing `max_per_window`):

```go
func TestRateLimitNegativeRejected(t *testing.T) {
	p := filepath.Join(t.TempDir(), "runlore.yaml")
	doc := `
sources:
  alertmanager: {}
investigation:
  rate_limit:
    max_per_window: -1
`
	if err := os.WriteFile(p, []byte(doc), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(p); err == nil || !strings.Contains(err.Error(), "max_per_window") {
		t.Fatalf("negative max_per_window must fail Load, got err=%v", err)
	}
}
```

- [ ] **Step 6: Fix the serve wiring** (`internal/app/serve.go:186-190`)

```go
	var rlStarts *ratelimit.Window
	if rl := cfg.Investigation.RateLimit; rl.MaxPerWindow != nil && *rl.MaxPerWindow > 0 {
		w := rl.Window.Std()
		rlStarts = ratelimit.New(*rl.MaxPerWindow, w)
		log.Info("investigation rate limit configured",
			"max_per_window", *rl.MaxPerWindow, "window", w, "max_requeues", rl.MaxRequeues)
	}
```

(The nil-check is belt-and-braces for tests that construct `Config` bare; the serve path always goes through `Load` → `applyDefaults`.)

- [ ] **Step 7: Sweep for other consumers**

Run: `grep -rn "Investigation.RateLimit.MaxPerWindow\|RateLimit\.MaxPerWindow" --include='*.go' .`
Expected: only `internal/config/load.go`, `internal/config/config.go` (Validate), `internal/app/serve.go`, and test files. Fix any test that sets `MaxPerWindow: 3` literally to `MaxPerWindow: ptrTo(3)` style — if the config package has no int-pointer helper, inline `n := 3; ... = &n` in each test (do not add a helper for two uses — YAGNI).

- [ ] **Step 8: Run the tests**

Run: `go test ./internal/config/ ./internal/app/ -v -run 'TestRateLimit'`
Expected: all PASS. Then the full package suites: `go test ./internal/config/ ./internal/app/` — PASS.

- [ ] **Step 9: Update docs + chart**

`docs/configuration.md:112` becomes:

```markdown
- `rate_limit` — `max_per_window` (**default 30**; an explicit **0 = unlimited**), `window` (default **1h**),
  `max_requeues`.
```

`deploy/helm/runlore/values.yaml:155` comment becomes:

```yaml
    rate_limit:
      max_per_window: 20        # cap investigations per window (window defaults to 1h);
                                #   explicit 0 = unlimited; UNSET now defaults to 30
```

- [ ] **Step 10: Quality gate + commit**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
go test -race ./internal/config/ ./internal/app/
git add internal/config/ internal/app/serve.go docs/configuration.md deploy/helm/runlore/values.yaml
git commit -m "feat(config): default investigation rate limit to 30/h

BEHAVIOR CHANGE: an unset investigation.rate_limit.max_per_window now
defaults to 30 per 1h window (cost-DoS guard). An explicit 0 keeps the
documented unlimited behavior; the Helm chart already ships 20."
```

---

### Task 2: Cap investigation requests per webhook payload

**Files:**
- Modify: `internal/source/webhook.go` (`Built.Handler`, `webhook.go:26-57`)
- Test: `internal/source/webhook_test.go`

**Interfaces:**
- Consumes: `Built.Handler(auth, bodyCap, pipe)` (`webhook.go:26`), `Pipeline.log` (same-package unexported field, may be nil — tests pass `NewPipeline(cfg, enq, nil, nil)`), test fakes `fakeDecoder`/`webhookBuilt`/`capEnq`/`matchAllCfg` (`webhook_test.go:16-49`).
- Produces: exported const `MaxRequestsPerPayload = 100` in package `source` (exported so the test file — and, later, docs — reference one symbol; keep the name if you change the value).

- [ ] **Step 1: Write the failing test** (append to `internal/source/webhook_test.go`; add `"fmt"` to imports)

```go
// TestHandlerPayloadRequestCap pins the per-delivery cost guard: one webhook
// payload can enqueue at most MaxRequestsPerPayload investigation requests. The
// 1MiB body cap alone still admits ~1k alerts; distinct alertnames each bill an
// investigation, so the count must be bounded at the door. Truncation (not 4xx)
// is deliberate: Alertmanager re-delivers on repeat_interval, so a legitimate
// mega-batch self-heals, while a rejected delivery would lose ALL its alerts.
func TestHandlerPayloadRequestCap(t *testing.T) {
	enq := &capEnq{}
	pipe := NewPipeline(matchAllCfg(), enq, nil, nil)
	var res DecodeResult
	for i := 0; i < MaxRequestsPerPayload+50; i++ {
		res.Requests = append(res.Requests, investigate.Request{
			Title:       fmt.Sprintf("storm-%d", i),
			Severity:    "critical",
			Workload:    providers.Workload{Namespace: "default", Name: fmt.Sprintf("app-%d", i)},
			Fingerprint: fmt.Sprintf("fp-%d", i),
		})
	}
	b := webhookBuilt(fakeDecoder{result: res})
	h := b.Handler(nil, 1<<20, pipe)
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", strings.NewReader(`{}`))
	rec := httptest.NewRecorder()
	h(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("want 202 (truncate, not reject), got %d", rec.Code)
	}
	if len(enq.reqs) != MaxRequestsPerPayload {
		t.Fatalf("want %d enqueued (cap), got %d", MaxRequestsPerPayload, len(enq.reqs))
	}
}
```

(If `capEnq`/`matchAllCfg` admission drops some of these — e.g. the trigger policy doesn't match — mirror whatever `TestHandlerValidRequest` at `webhook_test.go:89` sets on its Request so every request is admitted; the assertion must isolate the cap, not the trigger policy.)

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/source/ -run TestHandlerPayloadRequestCap -v`
Expected: compile FAIL — `undefined: MaxRequestsPerPayload`.

- [ ] **Step 3: Implement the cap** (`internal/source/webhook.go` — const above `Handler`, truncation after the decode-error return at `webhook.go:49-53`)

```go
// MaxRequestsPerPayload bounds how many investigation requests one webhook
// delivery may enqueue (cost-DoS guard): the 1MiB body cap still admits ~1k
// alerts, and distinct alertnames bypass dedup — each would bill a model
// investigation. Resolutions are EXEMPT (cheap, and dropping one would corrupt
// outcome tracking). Truncation over rejection: Alertmanager re-delivers on
// repeat_interval, so a truncated legitimate batch self-heals; a 4xx loses all
// of it. Not configurable until someone needs it (YAGNI).
const MaxRequestsPerPayload = 100
```

```go
		res, derr := wh.Decode(body, r.Header)
		if derr != nil {
			http.Error(w, "bad payload", http.StatusBadRequest)
			return
		}
		if len(res.Requests) > MaxRequestsPerPayload {
			if pipe.log != nil {
				pipe.log.Warn("webhook payload cap engaged; truncating requests",
					"source", b.Desc.Name, "decoded", len(res.Requests), "cap", MaxRequestsPerPayload)
			}
			res.Requests = res.Requests[:MaxRequestsPerPayload]
		}
		pipe.Ingest(r.Context(), b.Desc.Admission, res)
```

- [ ] **Step 4: Run tests**

Run: `go test ./internal/source/ -race -v`
Expected: all PASS, including the new test.

- [ ] **Step 5: Commit**

```bash
git add internal/source/webhook.go internal/source/webhook_test.go
git commit -m "feat(source): cap investigation requests per webhook payload at 100"
```

---

### Task 3: Failed-auth backoff on control + webhook endpoints

**Files:**
- Create: `internal/server/authguard.go`
- Create: `internal/server/authguard_test.go`
- Modify: `internal/server/server.go:31-44` (Server struct), `NewServer` (add field init), `authorized` (`server.go:191-196`), `webhookAuthorized` (`server.go:442-452`)
- Modify: `docs/security-model.md` (exposure section — one paragraph)

**Interfaces:**
- Consumes: `Server.log *slog.Logger` (assumed non-nil, as elsewhere in the file), `r.RemoteAddr`.
- Produces: `newAuthGuard() *authGuard` with methods `blocked(host string) bool`, `fail(host string)`, `success(host string)`; helper `remoteHost(remoteAddr string) string`. `Server` gains field `guard *authGuard`.

**Design invariants (encode as comments — they carry the security reasoning):**
- Only *consecutive failures* count; a correct token calls `success` and clears the host, so backoff can never lock out an operator whose token is right — except *during* a live block window (max 60s), the accepted NAT trade-off: without a pre-compare block, backoff would slow nothing.
- Block is checked BEFORE the compare; a blocked host learns nothing (not even timing).
- Map bounded at 4096 hosts; on overflow sweep expired entries; if all are live, the new host goes untracked — fail open on memory, never on auth.
- Tokens stay compared with `subtle.ConstantTimeCompare` — the guard is additive.

- [ ] **Step 1: Write the failing unit tests** (`internal/server/authguard_test.go`)

```go
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"testing"
	"time"
)

func guardAt(now *time.Time) *authGuard {
	g := newAuthGuard()
	g.now = func() time.Time { return *now }
	return g
}

func TestAuthGuardBlocksAfterThreshold(t *testing.T) {
	now := time.Unix(0, 0)
	g := guardAt(&now)
	for i := 0; i < failThreshold-1; i++ {
		g.fail("10.0.0.1")
		if g.blocked("10.0.0.1") {
			t.Fatalf("blocked after only %d failures (threshold %d)", i+1, failThreshold)
		}
	}
	g.fail("10.0.0.1")
	if !g.blocked("10.0.0.1") {
		t.Fatal("must block after reaching the failure threshold")
	}
	if g.blocked("10.0.0.2") {
		t.Fatal("other hosts must be unaffected")
	}
}

func TestAuthGuardBlockExpiresAndIsCapped(t *testing.T) {
	now := time.Unix(0, 0)
	g := guardAt(&now)
	for i := 0; i < failThreshold; i++ {
		g.fail("10.0.0.1")
	}
	now = now.Add(baseBlock + time.Millisecond)
	if g.blocked("10.0.0.1") {
		t.Fatal("first block must expire after baseBlock")
	}
	// Pile on failures: the block must never exceed maxBlock.
	for i := 0; i < 100; i++ {
		g.fail("10.0.0.1")
	}
	now = now.Add(maxBlock + time.Millisecond)
	if g.blocked("10.0.0.1") {
		t.Fatal("block must be capped at maxBlock — lockouts are never permanent")
	}
}

func TestAuthGuardSuccessResets(t *testing.T) {
	now := time.Unix(0, 0)
	g := guardAt(&now)
	for i := 0; i < failThreshold-1; i++ {
		g.fail("10.0.0.1")
	}
	g.success("10.0.0.1")
	g.fail("10.0.0.1")
	if g.blocked("10.0.0.1") {
		t.Fatal("success must reset the consecutive-failure count")
	}
}

func TestAuthGuardMapBounded(t *testing.T) {
	now := time.Unix(0, 0)
	g := guardAt(&now)
	for i := 0; i < maxHosts+100; i++ {
		g.fail(fmt.Sprintf("10.0.%d.%d", i/256, i%256))
	}
	g.mu.Lock()
	n := len(g.hosts)
	g.mu.Unlock()
	if n > maxHosts {
		t.Fatalf("guard map must stay bounded at %d, got %d", maxHosts, n)
	}
}

func TestRemoteHost(t *testing.T) {
	if got := remoteHost("192.0.2.1:1234"); got != "192.0.2.1" {
		t.Fatalf("want 192.0.2.1, got %q", got)
	}
	if got := remoteHost("no-port"); got != "no-port" {
		t.Fatalf("want passthrough for portless addr, got %q", got)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/server/ -run 'TestAuthGuard|TestRemoteHost' -v`
Expected: compile FAIL — `undefined: newAuthGuard`.

- [ ] **Step 3: Implement the guard** (`internal/server/authguard.go`)

```go
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net"
	"sync"
	"time"
)

// authGuard throttles repeated FAILED authentications per remote host with an
// exponential, capped block — bounding brute-force attempts on the shared-token
// endpoints. Invariants:
//
//   - Only consecutive failures count; success() clears the host, so a correct
//     token is never punished for a NAT-mate's noise — except DURING a live
//     block window (≤ maxBlock), the accepted trade-off: without a pre-compare
//     block, backoff would slow nothing.
//   - blocked() is consulted BEFORE the token compare — a blocked host learns
//     nothing, not even timing.
//   - The map is bounded: past maxHosts, expired entries are swept; if all are
//     live, the new host goes untracked. Fail open on memory, never on auth.
type authGuard struct {
	mu    sync.Mutex
	hosts map[string]*hostState
	now   func() time.Time
}

type hostState struct {
	fails      int
	blockUntil time.Time
}

const (
	failThreshold = 10               // consecutive failures before the first block
	baseBlock     = 1 * time.Second  // first block; doubles per further failure
	maxBlock      = 60 * time.Second // hard cap — a lockout is never permanent
	maxHosts      = 4096             // guard-map bound
)

func newAuthGuard() *authGuard {
	return &authGuard{hosts: map[string]*hostState{}, now: time.Now}
}

// blocked reports whether host is inside a live block window.
func (g *authGuard) blocked(host string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.hosts[host]
	return ok && g.now().Before(st.blockUntil)
}

// fail records one failed authentication, arming/extending the block once
// failThreshold consecutive failures accumulate.
func (g *authGuard) fail(host string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	st, ok := g.hosts[host]
	if !ok {
		if len(g.hosts) >= maxHosts {
			g.sweepLocked()
		}
		if len(g.hosts) >= maxHosts {
			return
		}
		st = &hostState{}
		g.hosts[host] = st
	}
	st.fails++
	if st.fails >= failThreshold {
		d := baseBlock << uint(st.fails-failThreshold)
		if d <= 0 || d > maxBlock { // <= 0 catches shift overflow
			d = maxBlock
		}
		st.blockUntil = g.now().Add(d)
	}
}

// success clears host — a correct token ends any backoff immediately.
func (g *authGuard) success(host string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.hosts, host)
}

// sweepLocked drops entries with no live block (expired or never armed).
func (g *authGuard) sweepLocked() {
	now := g.now()
	for h, st := range g.hosts {
		if !now.Before(st.blockUntil) {
			delete(g.hosts, h)
		}
	}
}

// remoteHost extracts the host half of an "ip:port" http RemoteAddr.
func remoteHost(remoteAddr string) string {
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return h
	}
	return remoteAddr
}
```

- [ ] **Step 4: Run unit tests**

Run: `go test ./internal/server/ -run 'TestAuthGuard|TestRemoteHost' -race -v`
Expected: PASS.

- [ ] **Step 5: Write the failing integration tests** (append to `authguard_test.go`; add imports `"log/slog"`, `"io"`, `"net/http/httptest"`, `"net/http"`)

```go
func testServerWithGuard(token string) *Server {
	return &Server{
		token: token,
		guard: newAuthGuard(),
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestAuthorizedBackoff pins the end-to-end behavior: enough wrong tokens from
// one host block it — and, during the (≤60s) block window, even the RIGHT token
// is rejected without being compared. That last property is the point: a
// blocked host learns nothing.
func TestAuthorizedBackoff(t *testing.T) {
	s := testServerWithGuard("secret")
	bad := httptest.NewRequest(http.MethodPost, "/actions/x/approve", nil)
	bad.Header.Set("X-Approval-Token", "wrong")
	for i := 0; i < failThreshold; i++ {
		if s.authorized(bad) {
			t.Fatal("wrong token must never authorize")
		}
	}
	good := httptest.NewRequest(http.MethodPost, "/actions/x/approve", nil)
	good.Header.Set("X-Approval-Token", "secret")
	if s.authorized(good) {
		t.Fatal("correct token must be rejected while the host is blocked")
	}
}

// TestAuthorizedSuccessResetsGuard pins that a correct token BEFORE the
// threshold clears the failure count (no creeping lockout for fat fingers).
func TestAuthorizedSuccessResetsGuard(t *testing.T) {
	s := testServerWithGuard("secret")
	bad := httptest.NewRequest(http.MethodPost, "/actions/x/approve", nil)
	bad.Header.Set("X-Approval-Token", "wrong")
	for i := 0; i < failThreshold-1; i++ {
		s.authorized(bad)
	}
	good := httptest.NewRequest(http.MethodPost, "/actions/x/approve", nil)
	good.Header.Set("X-Approval-Token", "secret")
	if !s.authorized(good) {
		t.Fatal("correct token below threshold must authorize")
	}
	if s.guard.blocked(remoteHost(good.RemoteAddr)) {
		t.Fatal("success must have cleared the guard")
	}
}
```

(`httptest.NewRequest` sets `RemoteAddr` to `192.0.2.1:1234` — both requests share the host, which is exactly what the tests need.)

- [ ] **Step 6: Run to verify they fail**

Run: `go test ./internal/server/ -run 'TestAuthorized' -v`
Expected: compile FAIL — `Server` has no field `guard` (and `authorized` doesn't consult it).

- [ ] **Step 7: Wire the guard into Server**

In the `Server` struct (`server.go:31`), after `approvers`:

```go
	guard *authGuard // failed-auth backoff for the shared-token endpoints
```

In `NewServer`, add to the `&Server{...}` composite literal (wherever the other fields are set):

```go
		guard: newAuthGuard(),
```

Replace `authorized` (`server.go:191-196`):

```go
// authorized enforces the approval token (constant-time compare). It FAILS
// CLOSED: with no token configured the control endpoints are denied, never open.
// (main refuses to start with actions enabled and an empty token, so a running
// rung-2/3 server always has one.) Repeated failures from one host arm an
// exponential backoff (see authGuard) — checked BEFORE the compare.
func (s *Server) authorized(r *http.Request) bool {
	host := remoteHost(r.RemoteAddr)
	if s.guard.blocked(host) {
		s.log.Warn("auth backoff engaged; rejecting without compare", "host", host)
		return false
	}
	if s.token == "" {
		return false
	}
	if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-Approval-Token")), []byte(s.token)) == 1 {
		s.guard.success(host)
		return true
	}
	s.guard.fail(host)
	return false
}
```

Replace `webhookAuthorized` (`server.go:442-452`) — the open (no-token) path stays untouched:

```go
// webhookAuthorized checks the optional alert-webhook bearer token (constant-time).
// When no token is configured the webhook is open — Validate forbids that once
// actions.mode=auto, so an auto-executing server always authenticates it. With a
// token configured, repeated failures from one host arm the same backoff as the
// control endpoints.
func (s *Server) webhookAuthorized(r *http.Request) bool {
	if s.webhookToken == "" {
		return true
	}
	host := remoteHost(r.RemoteAddr)
	if s.guard.blocked(host) {
		s.log.Warn("webhook auth backoff engaged; rejecting without compare", "host", host)
		return false
	}
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if strings.HasPrefix(h, prefix) &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(h, prefix)), []byte(s.webhookToken)) == 1 {
		s.guard.success(host)
		return true
	}
	s.guard.fail(host)
	return false
}
```

Then sweep existing server tests: any test constructing `Server` directly (not via `NewServer`) needs `guard: newAuthGuard()` — find them with `grep -n "&Server{" internal/server/*_test.go` and add the field; tests going through `NewServer` are covered.

- [ ] **Step 8: Run the package tests**

Run: `go test ./internal/server/ -race`
Expected: PASS (including all pre-existing auth tests — none send ≥10 wrong tokens from one host in sequence; if one does, reset with a fresh Server per subtest rather than weakening the guard).

- [ ] **Step 9: Document** (`docs/security-model.md`, in the section that discusses the approval/control endpoints — add one paragraph)

```markdown
Failed authentications on the control endpoints and the alert webhook are
rate-limited per remote host: after 10 consecutive failures the host is blocked
for 1s, doubling up to a 60s cap, and the block is checked before the token
compare. A correct token always clears the counter. Behind a shared NAT this
can delay a legitimate caller for at most one block window during a live
attack. Tokens should be ≥128-bit random values (e.g. `openssl rand -hex 16`);
the backoff is a brake on weak tokens, not a substitute for a strong one.
```

- [ ] **Step 10: Quality gate + commit**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
go test -race ./internal/server/
git add internal/server/ docs/security-model.md
git commit -m "feat(server): back off repeated failed auths per remote host"
```

---

## Final verification

- [ ] Full gate at HEAD of the branch: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` → 0 issues, all green.
- [ ] `go test -race ./internal/config/ ./internal/app/ ./internal/server/ ./internal/source/` → PASS.
- [ ] PR description: lead with the behavior change (unset rate limit → 30/h default; explicit 0 unchanged; Helm chart already pins 20), then the payload cap and the auth backoff with their trade-offs (truncate-not-reject; ≤60s NAT delay).
