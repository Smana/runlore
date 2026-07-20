# Templated Generic Webhook Source Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A config-only `custom` webhook source: operators map any vendor's alert JSON (Grafana, Datadog, OpsGenie, …) to investigation requests via dot-path field extraction — no Go, no recompile.

**Architecture:** One new self-registered source adapter (`internal/source/custom`) following the registry pattern (`source.Register` in `init()`, config under `sources.custom`, nil-from-Build = disabled). N named *instances* share one wildcard route `/webhook/custom/{instance}`; the core's `Built.Handler` stamps the path wildcard into a sanitized synthetic header (`X-Runlore-Instance`) so the unchanged `Decode(body, header)` / `Authenticate(body, header)` interfaces can dispatch per instance. Field extraction is a dependency-free dot-path subset (`a.b[2].c`). Auth mirrors PagerDuty: per-instance bearer token (env-indirected), falling back to the shared `server.webhook_token_env` bearer, open-with-startup-guards when neither is set (`RequireWebhookAuth` already refuses model-configured + shared-token-empty; `mode=auto` fails closed in Build). Bad mappings fail at startup (Build error aborts), never silently at ingest. The `MaxRequestsPerPayload` cost cap applies automatically — it lives in the core `Handler`.

**Tech Stack:** Go (toolchain go1.26.5); stdlib + `gopkg.in/yaml.v3` only (no new dependencies); existing `internal/source` registry/pipeline, `internal/investigate.Request`, `internal/curator.IncidentKey`.

## Global Constraints

- Quality gate before every commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — `0 issues.`, `gofmt -l` empty; plus `go test -race ./internal/source/... ./internal/investigate/...` before the final push.
- SPDX header `// SPDX-License-Identifier: Apache-2.0` as line 1 of every new `.go` file.
- Conventional Commits; **no co-author trailer, no AI attribution**.
- **No new third-party dependencies** (the dot-path extractor is hand-rolled; NO JSONPath library).
- Fail-safe: a malformed instance config aborts startup with a precise error; at ingest, a single bad event is skipped, never the whole payload; unknown `{instance}` ⇒ 401/400, never a panic.
- Config stores env-var **names**, never secret values (`token_env`, repo-wide pattern).
- The `X-Runlore-Instance` header must be attacker-proof: the core **deletes** any client-supplied value before stamping the path value (Task 3).

## File Structure

```
internal/source/custom/path.go        dot-path parse + extract + scalar coercion (pure)
internal/source/custom/path_test.go
internal/source/custom/config.go      instance config decode + startup validation
internal/source/custom/config_test.go
internal/source/custom/custom.go      Source (Decode + init registration + Build)
internal/source/custom/custom_test.go
internal/source/custom/auth.go        Authenticate (per-instance → shared → open)
internal/source/custom/auth_test.go
internal/source/webhook.go            (modify) InstanceHeader const + stamp in Handler
internal/source/webhook_test.go       (modify) header-stamp + forged-header tests
internal/investigate/investigate.go   (modify) SourceCustom constant
docs/data-sources.md                  (modify) Custom webhooks section, 2 worked examples
```

---

### Task 1: Dot-path extractor

**Files:**
- Create: `internal/source/custom/path.go`
- Test: `internal/source/custom/path_test.go`

**Interfaces:**
- Produces: `parsePath(s string) (path, error)` where `type path []step`, `type step struct { key string; idx int; isIdx bool }`; `(p path) lookup(doc any) (any, bool)`; `coerce(v any) (string, bool)` — string/bool/float64/int coerced to string, objects/arrays/nil refuse. Later tasks call `parsePath` at config time and `lookup`+`coerce` at decode time.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: Apache-2.0

package custom

import "testing"

func TestParseAndLookup(t *testing.T) {
	doc := map[string]any{
		"alerts": []any{
			map[string]any{"labels": map[string]any{"alertname": "HighCPU", "code": float64(503), "firing": true}},
		},
		"title": "root",
	}
	cases := []struct {
		path string
		want string
		ok   bool
	}{
		{"title", "root", true},
		{"alerts[0].labels.alertname", "HighCPU", true},
		{"alerts[0].labels.code", "503", true},
		{"alerts[0].labels.firing", "true", true},
		{"alerts[1].labels.alertname", "", false}, // index out of range
		{"alerts[0].labels.missing", "", false},
		{"alerts", "", false}, // array leaf refuses coercion
	}
	for _, c := range cases {
		p, err := parsePath(c.path)
		if err != nil {
			t.Fatalf("parsePath(%q): %v", c.path, err)
		}
		v, found := p.lookup(doc)
		got, coerced := "", false
		if found {
			got, coerced = coerce(v)
		}
		if (found && coerced) != c.ok || got != c.want {
			t.Errorf("%q: got (%q,%v), want (%q,%v)", c.path, got, found && coerced, c.want, c.ok)
		}
	}
}

func TestParsePathRejectsMalformed(t *testing.T) {
	for _, bad := range []string{"", "a[", "a[x]", "a[-1]", "a..b", "[0]", "a["} {
		if _, err := parsePath(bad); err == nil {
			t.Errorf("parsePath(%q): want error, got nil", bad)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/source/custom/ -run 'TestParse' -v`
Expected: FAIL — `undefined: parsePath` (package doesn't compile yet)

- [ ] **Step 3: Implement**

```go
// SPDX-License-Identifier: Apache-2.0

// Package custom is the config-only generic webhook source: operators map any
// vendor's alert JSON to investigation requests with dot-path field extraction
// (sources.custom.instances.<name>), no Go required.
package custom

import (
	"fmt"
	"strconv"
	"strings"
)

// step is one segment of a dot-path: a map key, optionally followed by one
// array index (`labels`, `alerts[0]`).
type step struct {
	key   string
	idx   int
	isIdx bool
}

type path []step

// parsePath parses the supported dot-path subset: dot-separated map keys, each
// optionally suffixed with a single non-negative `[n]` index. Deliberately NOT
// JSONPath (no wildcards, filters, recursion) — YAGNI, zero dependencies.
func parsePath(s string) (path, error) {
	if s == "" {
		return nil, fmt.Errorf("empty path")
	}
	var out path
	for _, seg := range strings.Split(s, ".") {
		if seg == "" {
			return nil, fmt.Errorf("path %q: empty segment", s)
		}
		key, rest := seg, ""
		if i := strings.IndexByte(seg, '['); i >= 0 {
			key, rest = seg[:i], seg[i:]
		}
		if key == "" {
			return nil, fmt.Errorf("path %q: segment %q lacks a key before '['", s, seg)
		}
		st := step{key: key}
		if rest != "" {
			if !strings.HasSuffix(rest, "]") {
				return nil, fmt.Errorf("path %q: unterminated index in %q", s, seg)
			}
			n, err := strconv.Atoi(rest[1 : len(rest)-1])
			if err != nil || n < 0 {
				return nil, fmt.Errorf("path %q: bad index in %q", s, seg)
			}
			st.idx, st.isIdx = n, true
		}
		out = append(out, st)
	}
	return out, nil
}

// lookup walks doc (json.Unmarshal-into-any shapes: map[string]any / []any).
func (p path) lookup(doc any) (any, bool) {
	cur := doc
	for _, st := range p {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, false
		}
		cur, ok = m[st.key]
		if !ok {
			return nil, false
		}
		if st.isIdx {
			arr, ok := cur.([]any)
			if !ok || st.idx >= len(arr) {
				return nil, false
			}
			cur = arr[st.idx]
		}
	}
	return cur, true
}

// coerce renders a scalar leaf as a string; composite values refuse (a mapping
// that lands on an object is a config mistake surfaced by absence, not garbage).
func coerce(v any) (string, bool) {
	switch t := v.(type) {
	case string:
		return t, true
	case bool:
		return strconv.FormatBool(t), true
	case float64: // encoding/json numbers
		return strconv.FormatFloat(t, 'g', -1, 64), true
	case int:
		return strconv.Itoa(t), true
	default:
		return "", false
	}
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/source/custom/ -run 'TestParse' -v`
Expected: PASS (both tests)

- [ ] **Step 5: Gate + commit**

```bash
go build ./... && go vet ./... && gofmt -l . && golangci-lint run ./internal/source/...
git add internal/source/custom/path.go internal/source/custom/path_test.go
git commit -m "feat(source): dot-path extractor for the generic webhook source"
```

---

### Task 2: Instance config — decode + startup validation

**Files:**
- Create: `internal/source/custom/config.go`
- Test: `internal/source/custom/config_test.go`

**Interfaces:**
- Consumes: `parsePath` (Task 1).
- Produces: `type instanceCfg struct` (yaml tags below), `type instance struct` (compiled form: parsed paths + token), `parseConfig(node yaml.Node) (map[string]*instance, error)` — called by `Build` (Task 5). Compiled `instance` fields: `fields map[string]path` (keys = the field names below), `items path` (nil = single event at root), `resolvedValue string`, `labels path`, `defaults map[string]string`, `severityMap map[string]string`, `tokenEnv string`.

Config shape (documented in Task 6):

```yaml
sources:
  custom:
    instances:
      grafana:
        token_env: GRAFANA_WEBHOOK_TOKEN   # optional; falls back to server.webhook_token_env
        items: alerts                      # optional; absent = whole body is one event
        fields:                            # dot-paths, relative to one event
          title: labels.alertname          # REQUIRED
          message: annotations.summary
          severity: labels.severity
          namespace: labels.namespace
          workload_kind: labels.kind
          workload_name: labels.pod
          environment: labels.env
          fingerprint: fingerprint
          resolved: status                 # value compared to resolved_value
        resolved_value: resolved           # default "resolved"
        labels: labels                     # optional; object of scalar labels
        defaults: { severity: warning }    # applied when the path yields nothing
        severity_map: { P1: critical }     # value normalization after extraction
```

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func mustNode(t *testing.T, y string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(y), &n); err != nil {
		t.Fatal(err)
	}
	return *n.Content[0] // unwrap the document node
}

func TestParseConfigValid(t *testing.T) {
	n := mustNode(t, `
instances:
  grafana:
    items: alerts
    fields: {title: labels.alertname, severity: labels.severity, resolved: status}
    severity_map: {P1: critical}
    defaults: {environment: prod}
`)
	insts, err := parseConfig(n)
	if err != nil {
		t.Fatal(err)
	}
	g := insts["grafana"]
	if g == nil || g.items == nil || g.resolvedValue != "resolved" {
		t.Fatalf("compiled instance wrong: %+v", g)
	}
	if _, ok := g.fields["title"]; !ok {
		t.Fatal("title path not compiled")
	}
}

func TestParseConfigRejects(t *testing.T) {
	cases := []struct{ name, yml, wantErr string }{
		{"missing title", `
instances:
  a: {fields: {severity: s}}`, "fields.title is required"},
		{"bad path", `
instances:
  a: {fields: {title: "x["}}`, "unterminated index"},
		{"unknown instance key", `
instances:
  a: {fields: {title: t}, itms: alerts}`, "unknown key"},
		{"no instances", `instances: {}`, "at least one instance"},
	}
	for _, c := range cases {
		_, err := parseConfig(mustNode(t, c.yml))
		if err == nil || !strings.Contains(err.Error(), c.wantErr) {
			t.Errorf("%s: err=%v, want containing %q", c.name, err, c.wantErr)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/source/custom/ -run 'TestParseConfig' -v`
Expected: FAIL — `undefined: parseConfig`

- [ ] **Step 3: Implement**

```go
// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"fmt"
	"sort"

	"gopkg.in/yaml.v3"
)

// instanceCfg is the raw yaml shape of one sources.custom.instances entry.
type instanceCfg struct {
	TokenEnv      string            `yaml:"token_env"`
	Items         string            `yaml:"items"`
	Fields        map[string]string `yaml:"fields"`
	ResolvedValue string            `yaml:"resolved_value"`
	Labels        string            `yaml:"labels"`
	Defaults      map[string]string `yaml:"defaults"`
	SeverityMap   map[string]string `yaml:"severity_map"`
}

type rootCfg struct {
	Instances map[string]instanceCfg `yaml:"instances"`
}

// fieldNames is the closed set of extractable Request fields.
var fieldNames = map[string]bool{
	"title": true, "message": true, "severity": true, "namespace": true,
	"workload_kind": true, "workload_name": true, "environment": true,
	"fingerprint": true, "resolved": true,
}

// instanceKeys is the closed set of per-instance config keys, for the loud
// unknown-key check (mirrors source.BuildEnabled's typo philosophy: a typo'd
// key must abort startup, not silently disable a mapping).
var instanceKeys = map[string]bool{
	"token_env": true, "items": true, "fields": true, "resolved_value": true,
	"labels": true, "defaults": true, "severity_map": true,
}

// instance is the compiled (validated, path-parsed) form used at decode time.
type instance struct {
	fields        map[string]path
	items         path // nil = single event at body root
	resolvedValue string
	labels        path // nil = none
	defaults      map[string]string
	severityMap   map[string]string
	tokenEnv      string
	token         string // resolved at Build; see auth.go
}

// parseConfig compiles the sources.custom block. Every error aborts startup —
// a bad mapping must never silently drop alerts at ingest.
func parseConfig(node yaml.Node) (map[string]*instance, error) {
	// Loud unknown-key check per instance (node-level, since instanceCfg's plain
	// Decode is not strict).
	var rawRoot struct {
		Instances map[string]map[string]yaml.Node `yaml:"instances"`
	}
	if err := node.Decode(&rawRoot); err != nil {
		return nil, fmt.Errorf("decode sources.custom: %w", err)
	}
	for name, keys := range rawRoot.Instances {
		for k := range keys {
			if !instanceKeys[k] {
				return nil, fmt.Errorf("sources.custom.instances.%s: unknown key %q", name, k)
			}
		}
	}

	var c rootCfg
	if err := node.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode sources.custom: %w", err)
	}
	if len(c.Instances) == 0 {
		return nil, fmt.Errorf("sources.custom: at least one instance is required")
	}
	out := make(map[string]*instance, len(c.Instances))
	names := make([]string, 0, len(c.Instances))
	for n := range c.Instances {
		names = append(names, n)
	}
	sort.Strings(names) // deterministic error order
	for _, name := range names {
		ic := c.Instances[name]
		if ic.Fields["title"] == "" {
			return nil, fmt.Errorf("sources.custom.instances.%s: fields.title is required", name)
		}
		inst := &instance{
			fields:        map[string]path{},
			resolvedValue: ic.ResolvedValue,
			defaults:      ic.Defaults,
			severityMap:   ic.SeverityMap,
			tokenEnv:      ic.TokenEnv,
		}
		if inst.resolvedValue == "" {
			inst.resolvedValue = "resolved"
		}
		for f, p := range ic.Fields {
			if !fieldNames[f] {
				return nil, fmt.Errorf("sources.custom.instances.%s: unknown field %q", name, f)
			}
			cp, err := parsePath(p)
			if err != nil {
				return nil, fmt.Errorf("sources.custom.instances.%s: fields.%s: %w", name, f, err)
			}
			inst.fields[f] = cp
		}
		if ic.Items != "" {
			cp, err := parsePath(ic.Items)
			if err != nil {
				return nil, fmt.Errorf("sources.custom.instances.%s: items: %w", name, err)
			}
			inst.items = cp
		}
		if ic.Labels != "" {
			cp, err := parsePath(ic.Labels)
			if err != nil {
				return nil, fmt.Errorf("sources.custom.instances.%s: labels: %w", name, err)
			}
			inst.labels = cp
		}
		out[name] = inst
	}
	return out, nil
}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/source/custom/ -run 'TestParseConfig' -v`
Expected: PASS

- [ ] **Step 5: Gate + commit**

```bash
go build ./... && go vet ./... && gofmt -l . && golangci-lint run ./internal/source/...
git add internal/source/custom/config.go internal/source/custom/config_test.go
git commit -m "feat(source): sources.custom instance config with startup validation"
```

---

### Task 3: Core — stamp the path wildcard into a sanitized header

**Files:**
- Modify: `internal/source/webhook.go` (Handler, ~line 38; new exported const)
- Test: `internal/source/webhook_test.go` (append)

**Interfaces:**
- Produces: `source.InstanceHeader = "X-Runlore-Instance"`. Handler guarantees: the header equals `r.PathValue("instance")` — client-supplied values are ALWAYS deleted first (anti-spoofing), and it is absent for routes without the wildcard. Tasks 4/5 read it in `Decode`/`Authenticate`.

- [ ] **Step 1: Write the failing tests** (append to `webhook_test.go`; helpers `webhookBuilt`, `capEnq`, `matchAllCfg`, `fakeDecoder`, `oneRequestResult` already exist there)

```go
// headerCapture records the header Decode saw.
type headerCapture struct {
	got    http.Header
	result DecodeResult
}

func (h *headerCapture) Decode(_ []byte, hdr http.Header) (DecodeResult, error) {
	h.got = hdr.Clone()
	return h.result, nil
}

func TestHandlerStampsInstanceHeader(t *testing.T) {
	cap := &headerCapture{result: oneRequestResult()}
	b := Built{Desc: Descriptor{Name: "custom", Kind: Webhook, Path: "/webhook/custom/{instance}"}, Impl: cap}
	pipe := NewPipeline(matchAllCfg(), &capEnq{}, nil, nil)

	mux := http.NewServeMux()
	MountWebhooks(mux, []Built{b}, nil, pipe, nil)
	// A forged client value must be OVERWRITTEN by the path value.
	req := httptest.NewRequest(http.MethodPost, "/webhook/custom/grafana", strings.NewReader(`{}`))
	req.Header.Set(InstanceHeader, "forged")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if got := cap.got.Get(InstanceHeader); got != "grafana" {
		t.Fatalf("InstanceHeader = %q, want %q", got, "grafana")
	}
}

func TestHandlerScrubsInstanceHeaderOnPlainRoutes(t *testing.T) {
	cap := &headerCapture{result: oneRequestResult()}
	b := webhookBuilt(fakeDecoder{result: oneRequestResult()})
	b.Impl = cap
	pipe := NewPipeline(matchAllCfg(), &capEnq{}, nil, nil)

	mux := http.NewServeMux()
	MountWebhooks(mux, []Built{b}, nil, pipe, nil)
	req := httptest.NewRequest(http.MethodPost, "/webhook/test", strings.NewReader(`{}`))
	req.Header.Set(InstanceHeader, "forged")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if got := cap.got.Get(InstanceHeader); got != "" {
		t.Fatalf("InstanceHeader = %q, want scrubbed", got)
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/source/ -run 'TestHandlerStampsInstance|TestHandlerScrubs' -v`
Expected: FAIL — `undefined: InstanceHeader`

- [ ] **Step 3: Implement.** In `webhook.go`, above `MaxRequestsPerPayload`:

```go
// InstanceHeader carries the {instance} path wildcard to Decode/Authenticate,
// whose signatures only see (body, header). The core owns it: any client-sent
// value is deleted before the path value is stamped, so adapters may trust it.
const InstanceHeader = "X-Runlore-Instance"
```

In `Handler`'s returned func, FIRST lines of the closure (before the shared-auth check, so `Authenticate` sees it too):

```go
		// Anti-spoofing: the instance header is core-owned. Delete any client
		// value, then stamp the route wildcard (empty for non-wildcard routes).
		r.Header.Del(InstanceHeader)
		if v := r.PathValue("instance"); v != "" {
			r.Header.Set(InstanceHeader, v)
		}
```

- [ ] **Step 4: Run to verify it passes**

Run: `go test ./internal/source/ -v`
Expected: PASS (new tests + all existing handler tests unchanged)

- [ ] **Step 5: Gate + commit**

```bash
go build ./... && go vet ./... && gofmt -l . && golangci-lint run ./internal/source/...
git add internal/source/webhook.go internal/source/webhook_test.go
git commit -m "feat(source): core-owned X-Runlore-Instance header from the route wildcard"
```

---

### Task 4: Decode — events → Requests/Resolutions

**Files:**
- Modify: `internal/investigate/investigate.go` (add `SourceCustom` constant next to `SourcePagerDuty`, ~line 36)
- Create: `internal/source/custom/custom.go` (Source + Decode; registration comes in Task 5)
- Test: `internal/source/custom/custom_test.go`

**Interfaces:**
- Consumes: `instance` (Task 2), `path.lookup`/`coerce` (Task 1), `source.InstanceHeader` (Task 3), `curator.IncidentKey(alertname, namespace, kind, name, cluster string) string`, `investigate.Request` (fields verified against `internal/investigate/investigate.go:44-66`), `source.DecodeResult`/`source.Resolution`.
- Produces: `type Source struct { instances map[string]*instance }` implementing `source.WebhookSource`. Instance name goes in the `IncidentKey` cluster slot (PagerDuty precedent: its service takes that slot) and as label `instance`.

- [ ] **Step 1: Add the constant** in `internal/investigate/investigate.go` after `SourcePagerDuty`:

```go
	// SourceCustom means the investigation was triggered by a generic
	// (config-mapped) vendor webhook — sources.custom.
	SourceCustom Source = "custom"
```

- [ ] **Step 2: Write the failing test**

```go
// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"net/http"
	"testing"

	"github.com/Smana/runlore/internal/investigate"
)

func grafanaInstance(t *testing.T) *Source {
	t.Helper()
	insts, err := parseConfig(mustNode(t, `
instances:
  grafana:
    items: alerts
    fields:
      title: labels.alertname
      message: annotations.summary
      severity: labels.severity
      namespace: labels.namespace
      workload_name: labels.pod
      fingerprint: fingerprint
      resolved: status
    labels: labels
    severity_map: {P1: critical}
    defaults: {environment: prod, severity: warning}
`))
	if err != nil {
		t.Fatal(err)
	}
	return &Source{instances: insts}
}

func hdr(instance string) http.Header {
	h := http.Header{}
	h.Set("X-Runlore-Instance", instance)
	return h
}

const grafanaBody = `{"alerts":[
  {"status":"firing","fingerprint":"fp1","labels":{"alertname":"HighCPU","severity":"P1","namespace":"payments","pod":"api-0"},"annotations":{"summary":"CPU is high"}},
  {"status":"resolved","fingerprint":"fp2","labels":{"alertname":"OldAlert"}},
  {"status":"firing","labels":{"alertname":"NoSeverity"}}
]}`

func TestDecodeGrafanaShape(t *testing.T) {
	s := grafanaInstance(t)
	res, err := s.Decode([]byte(grafanaBody), hdr("grafana"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 2 || len(res.Resolved) != 1 {
		t.Fatalf("got %d requests / %d resolved, want 2 / 1", len(res.Requests), len(res.Resolved))
	}
	r := res.Requests[0]
	if r.Source != investigate.SourceCustom || r.Title != "HighCPU" || r.Message != "CPU is high" {
		t.Errorf("request basics wrong: %+v", r)
	}
	if r.Severity != "critical" { // P1 through severity_map
		t.Errorf("severity = %q, want critical (mapped)", r.Severity)
	}
	if r.Workload.Namespace != "payments" || r.Workload.Name != "api-0" {
		t.Errorf("workload wrong: %+v", r.Workload)
	}
	if r.Environment != "prod" { // default applied
		t.Errorf("environment = %q, want prod (default)", r.Environment)
	}
	if r.Fingerprint != "fp1" || r.TriggerKey == "" || r.Labels["instance"] != "grafana" || r.Labels["alertname"] != "HighCPU" {
		t.Errorf("identity fields wrong: %+v", r)
	}
	if res.Resolved[0].Fingerprint != "fp2" {
		t.Errorf("resolution fingerprint = %q", res.Resolved[0].Fingerprint)
	}
	if res.Requests[1].Severity != "warning" { // default when path yields nothing
		t.Errorf("default severity not applied: %q", res.Requests[1].Severity)
	}
}

func TestDecodeSingleEventAtRoot(t *testing.T) {
	insts, err := parseConfig(mustNode(t, `
instances:
  datadog:
    fields: {title: title, message: body, severity: alert_type}
`))
	if err != nil {
		t.Fatal(err)
	}
	s := &Source{instances: insts}
	res, err := s.Decode([]byte(`{"title":"[Triggered] disk","body":"disk full","alert_type":"error"}`), hdr("datadog"))
	if err != nil || len(res.Requests) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if res.Requests[0].Title != "[Triggered] disk" || res.Requests[0].Severity != "error" {
		t.Errorf("request wrong: %+v", res.Requests[0])
	}
}

func TestDecodeUnknownInstanceErrors(t *testing.T) {
	s := grafanaInstance(t)
	if _, err := s.Decode([]byte(`{}`), hdr("nope")); err == nil {
		t.Fatal("want error for unknown instance")
	}
	if _, err := s.Decode([]byte(`{}`), http.Header{}); err == nil {
		t.Fatal("want error for missing instance header")
	}
}
```

- [ ] **Step 3: Run to verify it fails**

Run: `go test ./internal/source/custom/ -run 'TestDecode' -v`
Expected: FAIL — `undefined: Source`

- [ ] **Step 4: Implement**

```go
// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/source"
)

// Source is the generic webhook source adapter. One Source serves every
// configured instance; the core-stamped source.InstanceHeader selects which
// mapping applies to a delivery (see source.Built.Handler).
type Source struct {
	instances map[string]*instance
}

// Decode maps one delivery through the instance's field paths. A single
// non-conforming event is skipped (fail-safe: one junk element must not void a
// batch); an unknown instance is an error (→ 400) — it means a route/config
// mismatch, not vendor noise.
func (s *Source) Decode(body []byte, h http.Header) (source.DecodeResult, error) {
	name := h.Get(source.InstanceHeader)
	inst, ok := s.instances[name]
	if !ok {
		return source.DecodeResult{}, fmt.Errorf("custom: unknown instance %q", name)
	}
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return source.DecodeResult{}, fmt.Errorf("custom/%s: decode body: %w", name, err)
	}

	events := []any{doc}
	if inst.items != nil {
		v, ok := inst.items.lookup(doc)
		if !ok {
			return source.DecodeResult{}, fmt.Errorf("custom/%s: items path yields nothing", name)
		}
		arr, ok := v.([]any)
		if !ok {
			return source.DecodeResult{}, fmt.Errorf("custom/%s: items path is not an array", name)
		}
		events = arr
	}

	var out source.DecodeResult
	for _, ev := range events {
		get := func(field string) string {
			p, ok := inst.fields[field]
			if !ok {
				return inst.defaults[field]
			}
			v, found := p.lookup(ev)
			if !found {
				return inst.defaults[field]
			}
			s, ok := coerce(v)
			if !ok {
				return inst.defaults[field]
			}
			return s
		}

		fingerprint := get("fingerprint")
		if get("resolved") == inst.resolvedValue {
			if fingerprint != "" { // a resolution without identity cannot be attributed
				out.Resolved = append(out.Resolved, source.Resolution{Fingerprint: fingerprint, At: time.Now()})
			}
			continue
		}
		title := get("title")
		if title == "" {
			continue // fail-safe: skip the event, keep the batch
		}
		severity := get("severity")
		if mapped, ok := inst.severityMap[severity]; ok {
			severity = mapped
		}
		labels := map[string]string{"instance": name}
		if inst.labels != nil {
			if v, found := inst.labels.lookup(ev); found {
				if m, ok := v.(map[string]any); ok {
					for k, lv := range m {
						if s, ok := coerce(lv); ok {
							labels[k] = s
						}
					}
				}
			}
		}
		ns, kind, wname := get("namespace"), get("workload_kind"), get("workload_name")
		var fps []string
		if fingerprint != "" {
			fps = []string{fingerprint}
		}
		out.Requests = append(out.Requests, investigate.Request{
			Source:       investigate.SourceCustom,
			Title:        title,
			Severity:     severity,
			Environment:  get("environment"),
			Workload:     providers.Workload{Namespace: ns, Kind: kind, Name: wname},
			Reason:       severity,
			Message:      get("message"),
			Labels:       labels,
			At:           time.Now(),
			Fingerprint:  fingerprint,
			Fingerprints: fps,
			// Instance takes the cluster slot (PagerDuty precedent: its service
			// does) so two vendors reporting the same workload stay distinct.
			TriggerKey: curator.IncidentKey(title, ns, kind, wname, name),
		})
	}
	return out, nil
}
```

- [ ] **Step 5: Run to verify it passes**

Run: `go test ./internal/source/custom/ -v`
Expected: PASS (all tasks so far)

- [ ] **Step 6: Gate + commit**

```bash
go build ./... && go vet ./... && gofmt -l . && golangci-lint run ./internal/source/... ./internal/investigate/...
git add internal/investigate/investigate.go internal/source/custom/custom.go internal/source/custom/custom_test.go
git commit -m "feat(source): generic webhook Decode — config-mapped events to requests"
```

---

### Task 5: Auth + registration

**Files:**
- Create: `internal/source/custom/auth.go`
- Test: `internal/source/custom/auth_test.go`
- Modify: `internal/source/custom/custom.go` (append `init()` + `osGetenv` var)

**Interfaces:**
- Consumes: `source.Register`/`Descriptor`/`Deps` (registry.go:78-102), `config.ActionAuto` (PagerDuty Build precedent at `internal/source/pagerduty/pagerduty.go:191-217`), `Deps.Cfg.Server.WebhookTokenEnv`.
- Produces: `Source` also implements `source.Authenticator` — which SKIPS the core's shared auth, so the fallback re-implements it: per-instance `token_env` if set, else the shared `server.webhook_token_env` value (resolved once at Build), else open. Registration: `Name: "custom"`, `Kind: source.Webhook`, `Admission: source.MatchGated`, `Path: "/webhook/custom/{instance}"`.

- [ ] **Step 1: Write the failing test**

```go
// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"net/http"
	"testing"
)

func authSource(t *testing.T, instToken, sharedToken string) *Source {
	t.Helper()
	s := grafanaInstance(t)
	s.instances["grafana"].token = instToken
	s.shared = sharedToken
	return s
}

func bearer(instance, tok string) http.Header {
	h := hdr(instance)
	if tok != "" {
		h.Set("Authorization", "Bearer "+tok)
	}
	return h
}

func TestAuthenticate(t *testing.T) {
	cases := []struct {
		name                 string
		instToken, shared    string
		instance, presented  string
		want                 bool
	}{
		{"instance token ok", "sec1", "shared", "grafana", "sec1", true},
		{"instance token wrong", "sec1", "shared", "grafana", "shared", false}, // instance token set: shared no longer accepted
		{"fallback to shared", "", "shared", "grafana", "shared", true},
		{"fallback wrong", "", "shared", "grafana", "bad", false},
		{"both empty = open", "", "", "grafana", "", true},
		{"unknown instance fails closed", "sec1", "shared", "nope", "sec1", false},
	}
	for _, c := range cases {
		s := authSource(t, c.instToken, c.shared)
		if got := s.Authenticate(nil, bearer(c.instance, c.presented)); got != c.want {
			t.Errorf("%s: Authenticate = %v, want %v", c.name, got, c.want)
		}
	}
}
```

- [ ] **Step 2: Run to verify it fails**

Run: `go test ./internal/source/custom/ -run TestAuthenticate -v`
Expected: FAIL — `s.shared undefined` / `undefined: (*Source).Authenticate`

- [ ] **Step 3: Implement.** `auth.go`:

```go
// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"crypto/subtle"
	"net/http"
	"strings"

	"github.com/Smana/runlore/internal/source"
)

// Authenticate verifies a delivery's bearer token. Implementing
// source.Authenticator skips the core's shared webhook auth, so the ladder is
// re-established here explicitly: the instance's own token when configured
// (shared no longer accepted — tighter, per-vendor secrets), else the shared
// server.webhook_token_env value resolved at Build, else open (mirroring the
// alertmanager source; app.RequireWebhookAuth still refuses to start a
// model-configured server with an empty shared token, and Build fails closed
// under actions.mode=auto below).
func (s *Source) Authenticate(_ []byte, h http.Header) bool {
	inst, ok := s.instances[h.Get(source.InstanceHeader)]
	if !ok {
		return false // unknown instance: fail closed before Decode
	}
	want := inst.token
	if want == "" {
		want = s.shared
	}
	if want == "" {
		return true
	}
	const prefix = "Bearer "
	got := h.Get("Authorization")
	return strings.HasPrefix(got, prefix) &&
		subtle.ConstantTimeCompare([]byte(strings.TrimPrefix(got, prefix)), []byte(want)) == 1
}
```

Append to `custom.go`: add the `shared string` field to `Source`, and:

```go
// osGetenv is a package-level indirection for os.Getenv (PagerDuty precedent;
// tests set real env vars via t.Setenv).
var osGetenv = os.Getenv

func init() {
	source.Register(source.Descriptor{
		Name: "custom",
		Kind: source.Webhook, Admission: source.MatchGated, Path: "/webhook/custom/{instance}",
		Build: func(d source.Deps) (any, error) {
			node, ok := d.Raw["custom"]
			if !ok {
				return nil, nil // disabled: no sources.custom key
			}
			insts, err := parseConfig(node)
			if err != nil {
				return nil, err
			}
			shared := ""
			if d.Cfg != nil && d.Cfg.Server.WebhookTokenEnv != "" {
				shared = osGetenv(d.Cfg.Server.WebhookTokenEnv)
			}
			for name, inst := range insts {
				if inst.tokenEnv != "" {
					inst.token = osGetenv(inst.tokenEnv)
					if inst.token == "" {
						return nil, fmt.Errorf("sources.custom.instances.%s: token_env %q is empty", name, inst.tokenEnv)
					}
				}
				// Fail closed under mode=auto: an unattended executor must not
				// accept unauthenticated vendor webhooks (PagerDuty precedent).
				if d.Cfg != nil && d.Cfg.Actions.Mode == config.ActionAuto && inst.token == "" && shared == "" {
					return nil, fmt.Errorf("actions.mode=auto requires a token for sources.custom.instances.%s (token_env or server.webhook_token_env)", name)
				}
			}
			return &Source{instances: insts, shared: shared}, nil
		},
	})
}
```

(Add `"os"` and `"github.com/Smana/runlore/internal/config"` to custom.go's imports. Check the exact `ActionAuto` constant name in `internal/config` — PagerDuty's Build at `pagerduty.go:211` uses `config.ActionAuto`; mirror it.)

- [ ] **Step 4: Write + run the Build tests** (append to `auth_test.go`)

```go
func TestBuildFailClosedUnderAuto(t *testing.T) {
	// Registered descriptor is package-global; drive Build via source.BuildEnabled
	// in an integration test OR call the registered Build through Registered().
	// Simplest: look it up.
	var build func(source.Deps) (any, error)
	for _, d := range source.Registered() {
		if d.Name == "custom" {
			build = d.Build
		}
	}
	if build == nil {
		t.Fatal("custom source not registered")
	}
	raw := map[string]yaml.Node{"custom": mustNode(t, `
instances:
  a: {fields: {title: t}}
`)}
	cfg := &config.Config{}
	cfg.Actions.Mode = config.ActionAuto
	if _, err := build(source.Deps{Cfg: cfg, Raw: raw}); err == nil {
		t.Fatal("want fail-closed error under mode=auto with no token")
	}
	// And with a token it builds.
	t.Setenv("CUSTOM_TOK", "s3cret")
	raw["custom"] = mustNode(t, `
instances:
  a: {token_env: CUSTOM_TOK, fields: {title: t}}
`)
	impl, err := build(source.Deps{Cfg: cfg, Raw: raw})
	if err != nil || impl == nil {
		t.Fatalf("build with token: impl=%v err=%v", impl, err)
	}
}
```

(Imports for auth_test.go: add `"github.com/Smana/runlore/internal/config"`, `"github.com/Smana/runlore/internal/source"`, `"gopkg.in/yaml.v3"`.)

Run: `go test ./internal/source/custom/ -v`
Expected: PASS (all)

- [ ] **Step 5: Gate + commit**

```bash
go build ./... && go vet ./... && gofmt -l . && golangci-lint run ./internal/source/...
git add internal/source/custom/auth.go internal/source/custom/auth_test.go internal/source/custom/custom.go
git commit -m "feat(source): register the custom webhook source with per-instance bearer auth"
```

**Wiring check (no code expected):** `app/serve.go:266` builds every registered source via `source.BuildEnabled(... Raw: cfg.Sources)` and `MountWebhooks` mounts all Webhook-kind descriptors — a registered adapter needs NO app edits. Verify by grepping that nothing enumerates source names: `grep -rn '"alertmanager"\|"pagerduty"' internal/app/` — the only hits must be auth-guard helpers (`RequirePagerDutyAuth`), not mount lists. If a hit enumerates sources for mounting, stop and reassess (plan assumption broken).

---

### Task 6: Docs — worked Grafana + Datadog examples

**Files:**
- Modify: `docs/data-sources.md` (new section after the sources table)

**Interfaces:** none (docs only).

- [ ] **Step 1: Add the section.** Content (adjust heading depth to the file's existing structure):

```markdown
## Custom webhooks — any vendor, no code

The `custom` source maps ANY vendor's alert JSON to investigations with dot-path
field extraction — config only. Each named instance gets its own endpoint
`POST /webhook/custom/<instance>` and its own optional bearer token
(`token_env`, falling back to `server.webhook_token_env`). Field paths are
dot-separated with optional `[n]` indexes (`alerts[0].labels.alertname`); a
missing path falls back to `defaults`. `severity_map` normalizes vendor
severities to yours. A payload with `items` set is a batch (path to the event
array); without it the whole body is one event. Events whose `resolved` path
equals `resolved_value` (default `"resolved"`) record a resolution for the
outcome ledger instead of triggering an investigation (requires `fingerprint`).
The per-delivery request cap and 1MiB body cap apply as for every webhook
source. A typo'd instance key, an unparseable path, or a missing `fields.title`
aborts startup — mappings never fail silently at ingest.

### Grafana Alerting

```yaml
sources:
  custom:
    instances:
      grafana:
        token_env: GRAFANA_WEBHOOK_TOKEN
        items: alerts
        fields:
          title: labels.alertname
          message: annotations.summary
          severity: labels.severity
          namespace: labels.namespace
          workload_name: labels.pod
          fingerprint: fingerprint
          resolved: status
        labels: labels
        defaults: { environment: prod }
```

Point a Grafana webhook contact point at
`https://<runlore>/webhook/custom/grafana` with an `Authorization: Bearer …`
custom header.

### Datadog (custom webhook payload)

Datadog webhooks POST a single flat JSON you define with template variables:

```json
{"title": "$EVENT_TITLE", "text": "$TEXT_ONLY_MSG", "alert_type": "$ALERT_TYPE",
 "alert_status": "$ALERT_TRANSITION", "aggreg_key": "$AGGREG_KEY"}
```

```yaml
sources:
  custom:
    instances:
      datadog:
        token_env: DATADOG_WEBHOOK_TOKEN
        fields:
          title: title
          message: text
          severity: alert_type
          fingerprint: aggreg_key
          resolved: alert_status
        resolved_value: Recovered
        severity_map: { error: critical }
```

Requests without a Kubernetes workload recall only resource-less entries (the
scopeless tier) — same as PagerDuty.
```

- [ ] **Step 2: Full gate + race + commit + push**

```bash
go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...
go test -race ./internal/source/... ./internal/investigate/...
git add docs/data-sources.md
git commit -m "docs(data-sources): custom webhook source with Grafana and Datadog examples"
```

Expected: `0 issues.`, empty gofmt, all packages ok (including `-race`).

---

## Self-Review Notes (already applied)

- **Spec coverage:** config-only ✓ (registry, no app edits — verified `BuildEnabled`/`MountWebhooks` are name-agnostic, wiring check in Task 5) · dot-path subset, no dependency ✓ · per-instance token env with shared fallback ✓ · startup-fail validation incl. loud unknown keys ✓ · payload cap applies (core Handler, nothing to do) ✓ · Grafana + Datadog docs ✓.
- **Anti-spoofing:** `InstanceHeader` is deleted-then-stamped by the core (Task 3) — adapters may trust it; tested with a forged header on both wildcard and plain routes.
- **Type consistency:** `parsePath/lookup/coerce` (T1) ← config.go (T2) ← custom.go (T4); `Source{instances, shared}` shared by T4/T5; `hdr`/`mustNode`/`grafanaInstance` helpers defined once (T2/T4) and reused in T5 tests.
- **Known judgment calls for the implementer:** the exact `config.ActionAuto` constant and `Server.WebhookTokenEnv` field names must be confirmed against `internal/config` at implementation time (both verified present today via the PagerDuty adapter and `server.go`); `matchAllCfg`/`capEnq` live in `webhook_test.go` (package `source`) and are reusable from Task 3's tests only (same package) — the `custom` package tests are self-contained by design.
```
