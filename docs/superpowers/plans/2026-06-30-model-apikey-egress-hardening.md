# Model API-key Egress Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Stop a model API key from leaking off its intended TLS channel — via a cross-host redirect (S1) or a cleartext `http://` endpoint (S2).

**Architecture:** Both fixes live in shared layers, not the per-provider clients. S1 extends the shared `httpx.DenyInternalRedirect` (the `CheckRedirect` used by every secure client) to delete provider key headers when a redirect changes host. S2 adds a fail-closed check to `config.Config.Validate()` that rejects an `http://` base URL carrying an API key on a public host, using a pure (no-DNS) private-host classifier.

**Tech Stack:** Go 1.26, standard library only (`net`, `net/url`, `net/http`, `strings`). No new dependencies.

## Global Constraints

- Go 1.26.0; standard library only — no new module dependencies.
- No co-authored commits; no AI attribution in commit messages or PR text.
- Security-only change: zero behaviour change for correctly-configured deployments (https endpoints, keyless in-cluster endpoints, same-host redirects).
- Tests must be network-free: stub DNS via the existing `lookupIP` package var (httpx); use pure classification in config.
- Hostname comparison and host classification are **hostname-only** (port ignored) and case-insensitive.
- Spec: `docs/superpowers/specs/2026-06-30-model-apikey-egress-hardening-design.md`.

---

### Task 1: S1 — strip provider key headers on a host-changing redirect

**Files:**
- Modify: `internal/httpx/client.go` (add `sensitiveAuthHeaders`; insert host-change strip into `DenyInternalRedirect`; add `strings` import)
- Test: `internal/httpx/client_test.go`

**Interfaces:**
- Consumes: existing `DenyInternalRedirect(req *http.Request, via []*http.Request) error`, `maxRedirects` const, `lookupIP` package var, `hostIsInternal`, `isInternalIP`.
- Produces: same `DenyInternalRedirect` signature (unchanged) with added header-stripping behaviour; new package var `sensitiveAuthHeaders []string`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/httpx/client_test.go`:

```go
// mkreqWithKeys builds a redirect-target request carrying the three provider key headers.
func mkreqWithKeys(t *testing.T, rawurl string) *http.Request {
	t.Helper()
	r := mkreq(t, rawurl)
	r.Header.Set("X-Api-Key", "sk-secret")
	r.Header.Set("X-Goog-Api-Key", "goog-secret")
	r.Header.Set("Authorization", "Bearer tok")
	return r
}

func TestDenyInternalRedirectStripsKeyOnCrossHost(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()
	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil } // public

	origin := mkreq(t, "https://api.anthropic.com/v1/messages")
	target := mkreqWithKeys(t, "https://attacker.example/v1/messages")
	if err := DenyInternalRedirect(target, []*http.Request{origin}); err != nil {
		t.Fatalf("public cross-host redirect should be allowed (headers stripped), got %v", err)
	}
	for _, h := range []string{"X-Api-Key", "X-Goog-Api-Key", "Authorization"} {
		if got := target.Header.Get(h); got != "" {
			t.Fatalf("header %s must be stripped on cross-host redirect, got %q", h, got)
		}
	}
}

func TestDenyInternalRedirectKeepsKeyOnSameHost(t *testing.T) {
	orig := lookupIP
	defer func() { lookupIP = orig }()
	lookupIP = func(string) ([]net.IP, error) { return []net.IP{net.ParseIP("93.184.216.34")}, nil } // public

	// Same hostname, http→https upgrade (port 80→443): the key must be retained.
	origin := mkreq(t, "http://api.anthropic.com/v1/messages")
	target := mkreqWithKeys(t, "https://api.anthropic.com/v1/messages")
	if err := DenyInternalRedirect(target, []*http.Request{origin}); err != nil {
		t.Fatalf("same-host redirect should be allowed, got %v", err)
	}
	if target.Header.Get("X-Api-Key") == "" || target.Header.Get("Authorization") == "" {
		t.Fatal("key headers must be retained on a same-host redirect")
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/httpx/ -run 'TestDenyInternalRedirect(StripsKeyOnCrossHost|KeepsKeyOnSameHost)' -v`
Expected: FAIL — `TestDenyInternalRedirectStripsKeyOnCrossHost` fails because the headers are not yet stripped (`X-Api-Key must be stripped ...`).

- [ ] **Step 3: Add the header list and import**

In `internal/httpx/client.go`, add `"strings"` to the import block, and add this var after the `maxRedirects` const:

```go
// sensitiveAuthHeaders are request headers that carry a provider credential. Go's
// net/http strips Authorization/Cookie itself on a host-changing redirect but NOT
// custom headers, so DenyInternalRedirect deletes these explicitly (canonical form;
// http.Header.Del is case-insensitive). x-api-key = Anthropic, x-goog-api-key = Gemini.
var sensitiveAuthHeaders = []string{"X-Api-Key", "X-Goog-Api-Key", "Authorization"}
```

- [ ] **Step 4: Insert the host-change strip into `DenyInternalRedirect`**

In `internal/httpx/client.go`, immediately after the `maxRedirects` cap check and before the internal-origin early-return, insert:

```go
	// Strip provider key headers when a redirect changes host, so a credential is never
	// replayed to a different host (a compromised/MITM upstream, or an http endpoint
	// 3xx-ing elsewhere). Hostname-only compare (ignore port) keeps a same-host
	// http→https upgrade authenticated. Guard the nil entries that the cap test passes.
	if n := len(via); n > 0 && via[n-1] != nil {
		if !strings.EqualFold(req.URL.Hostname(), via[n-1].URL.Hostname()) {
			for _, h := range sensitiveAuthHeaders {
				req.Header.Del(h)
			}
		}
	}
```

The surrounding function is unchanged. The full top of the function now reads:

```go
func DenyInternalRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= maxRedirects {
		return fmt.Errorf("stopped after %d redirects", maxRedirects)
	}
	if n := len(via); n > 0 && via[n-1] != nil {
		if !strings.EqualFold(req.URL.Hostname(), via[n-1].URL.Hostname()) {
			for _, h := range sensitiveAuthHeaders {
				req.Header.Del(h)
			}
		}
	}
	// In-cluster-origin chains redirect among private addresses legitimately — only
	// guard chains that began at a public endpoint.
	if len(via) > 0 && hostIsInternal(via[0].URL.Hostname()) {
		return nil
	}
	// ... rest unchanged ...
```

- [ ] **Step 5: Run the full httpx test suite to verify pass + no regressions**

Run: `go test ./internal/httpx/ -v`
Expected: PASS — the two new tests pass and all existing `TestDenyInternalRedirect*` / `TestSecureClient` tests still pass (the cap test passes nil-filled `via` and returns at the cap before the new block; the nil-guard prevents a panic).

- [ ] **Step 6: Commit**

```bash
git add internal/httpx/client.go internal/httpx/client_test.go
git commit -m "fix(httpx): strip provider key headers on host-changing redirect

Go strips Authorization on a cross-host redirect but not custom headers, so
Anthropic's x-api-key and Gemini's x-goog-api-key were replayed to the redirect
target. DenyInternalRedirect now deletes the provider key headers whenever a
redirect changes host (hostname-only compare, so same-host http->https keeps auth)."
```

---

### Task 2: S2 — reject a cleartext API key on a public endpoint at config validation

**Files:**
- Modify: `internal/config/config.go` (add `net` + `net/url` imports; add the endpoint check to `Validate()`; add `isPrivateHost` and `checkSecureKeyEndpoint` helpers)
- Test: `internal/config/config_test.go`

**Interfaces:**
- Consumes: existing `(*Config).Validate() error`, `Model`, `ModelOverride`, `Embeddings` structs (fields `BaseURL`, `APIKeyEnv`).
- Produces: two new package-private funcs — `isPrivateHost(host string) bool` and `checkSecureKeyEndpoint(urlField, keyField, baseURL, apiKeyEnv string) error`; new validation behaviour in `Validate()`.

- [ ] **Step 1: Write the failing tests**

Add to `internal/config/config_test.go`:

```go
func TestValidateRejectsCleartextKeyOnPublicHost(t *testing.T) {
	cases := []struct {
		name      string
		baseURL   string
		apiKeyEnv string
		wantErr   bool
	}{
		{"http public + key", "http://api.openai.com/v1", "OPENAI_API_KEY", true},
		{"https public + key", "https://api.openai.com/v1", "OPENAI_API_KEY", false},
		{"http private IP + key", "http://10.0.0.5:8000/v1", "K", false},
		{"http localhost + key", "http://localhost:8000/v1", "K", false},
		{"http single-label + key", "http://vllm:8000/v1", "K", false},
		{"http .svc + key", "http://vllm.ai.svc.cluster.local/v1", "K", false},
		{"http public no key", "http://api.openai.com/v1", "", false},
		{"empty base_url + key", "", "OPENAI_API_KEY", false},
		{"unparseable + key", "http://%zz/v1", "K", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Model: Model{Provider: "openai", BaseURL: tc.baseURL, APIKeyEnv: tc.apiKeyEnv}}
			err := c.Validate()
			if tc.wantErr && err == nil {
				t.Fatalf("base_url %q + key %q must be rejected", tc.baseURL, tc.apiKeyEnv)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("base_url %q + key %q must validate clean, got %v", tc.baseURL, tc.apiKeyEnv, err)
			}
		})
	}
}

func TestValidateCleartextKeyCoversVerifyAndEmbeddings(t *testing.T) {
	// Verify override with its OWN http public base_url + own key.
	cv := &Config{Model: Model{Provider: "anthropic",
		Verify: &ModelOverride{BaseURL: "http://api.cheap.example/v1", APIKeyEnv: "CHEAP_KEY"}}}
	if err := cv.Validate(); err == nil {
		t.Fatal("verify override with http public base_url + key must be rejected")
	}
	// Verify override with its own http public base_url but INHERITING the parent key.
	ci := &Config{Model: Model{Provider: "anthropic", APIKeyEnv: "PARENT_KEY",
		Verify: &ModelOverride{BaseURL: "http://api.cheap.example/v1"}}}
	if err := ci.Validate(); err == nil {
		t.Fatal("verify override over http public, inheriting the parent key, must be rejected")
	}
	// Embeddings with http public base_url + key.
	ce := &Config{Model: Model{Provider: "anthropic",
		Embeddings: &Embeddings{BaseURL: "http://emb.example/v1", APIKeyEnv: "EMB_KEY"}}}
	if err := ce.Validate(); err == nil {
		t.Fatal("embeddings with http public base_url + key must be rejected")
	}
	// All-https equivalents validate clean.
	ok := &Config{Model: Model{Provider: "anthropic", APIKeyEnv: "PARENT_KEY",
		Verify:     &ModelOverride{BaseURL: "https://api.cheap.example/v1"},
		Embeddings: &Embeddings{BaseURL: "https://emb.example/v1", APIKeyEnv: "EMB_KEY"}}}
	if err := ok.Validate(); err != nil {
		t.Fatalf("https verify+embeddings must validate clean, got %v", err)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run 'TestValidate(RejectsCleartextKeyOnPublicHost|CleartextKeyCoversVerifyAndEmbeddings)' -v`
Expected: FAIL — `Validate()` does not yet perform the check, so the `wantErr` cases return nil instead of an error.

- [ ] **Step 3: Add imports and the two helpers**

In `internal/config/config.go`, add `"net"` and `"net/url"` to the import block. Then add these helpers (near `Validate`):

```go
// isPrivateHost reports whether host is a loopback / in-cluster / private endpoint
// where sending a key over plain http is acceptable. Pure — no DNS — so config
// validation stays deterministic and network-free. IP literals are classified by
// range; hostnames by well-known private forms. Anything else is treated as public.
func isPrivateHost(host string) bool {
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified()
	}
	h := strings.ToLower(strings.TrimSuffix(host, "."))
	if h == "localhost" {
		return true
	}
	if !strings.Contains(h, ".") {
		return true // single-label in-cluster service name, e.g. "vllm"
	}
	for _, suf := range []string{".local", ".internal", ".svc", ".cluster.local"} {
		if strings.HasSuffix(h, suf) {
			return true
		}
	}
	return false
}

// checkSecureKeyEndpoint rejects a base_url that would send an API key in cleartext.
// A key is "present" when apiKeyEnv is non-empty; an empty base_url uses the provider's
// built-in (https) default and is always fine. http is allowed only to a private host.
func checkSecureKeyEndpoint(urlField, keyField, baseURL, apiKeyEnv string) error {
	if apiKeyEnv == "" || baseURL == "" {
		return nil
	}
	u, err := url.Parse(baseURL)
	if err != nil {
		return fmt.Errorf("%s is not a valid URL (%s is set): %w", urlField, keyField, err)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		if isPrivateHost(u.Hostname()) {
			return nil
		}
		return fmt.Errorf("%s is %q with an API key (%s set) on a public host — the key would be sent in cleartext; use https or a loopback/in-cluster endpoint", urlField, baseURL, keyField)
	default:
		return fmt.Errorf("%s must be an http(s) URL when %s is set, got scheme %q", urlField, keyField, u.Scheme)
	}
}
```

- [ ] **Step 4: Wire the check into `Validate()`**

In `internal/config/config.go`, in `Validate()`, immediately after the `model.verify.max_tokens` check (the `if c.Model.Verify != nil && c.Model.Verify.MaxTokens < 0` block) and before the `switch c.Actions.Mode`, insert:

```go
	// Reject a cleartext API key over a public endpoint (the key would be sent in the
	// clear, and is the enabler for a redirect-based key leak). Cover the main model,
	// a verify override that targets its own endpoint, and embeddings. Loopback /
	// in-cluster hosts are exempt.
	if err := checkSecureKeyEndpoint("model.base_url", "model.api_key_env", c.Model.BaseURL, c.Model.APIKeyEnv); err != nil {
		return err
	}
	if v := c.Model.Verify; v != nil && v.BaseURL != "" {
		// A verify override inherits the parent key when its own api_key_env is unset.
		keyEnv := v.APIKeyEnv
		if keyEnv == "" {
			keyEnv = c.Model.APIKeyEnv
		}
		if err := checkSecureKeyEndpoint("model.verify.base_url", "model.verify.api_key_env (or inherited model.api_key_env)", v.BaseURL, keyEnv); err != nil {
			return err
		}
	}
	if e := c.Model.Embeddings; e != nil {
		if err := checkSecureKeyEndpoint("model.embeddings.base_url", "model.embeddings.api_key_env", e.BaseURL, e.APIKeyEnv); err != nil {
			return err
		}
	}
```

- [ ] **Step 5: Run config tests to verify pass + no regressions**

Run: `go test ./internal/config/ -v`
Expected: PASS — both new tests pass and existing `TestValidate*` tests still pass (existing tests use empty or https base URLs, so the new check is a no-op for them).

- [ ] **Step 6: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): reject a cleartext API key on a public model endpoint

Validate() now fails closed when a model/verify/embeddings base_url is http with
an API key configured on a public host — the key would otherwise be sent in
cleartext. Loopback and in-cluster hosts (private IPs, localhost, single-label
service names, .svc/.cluster.local/.local/.internal) stay exempt; classification
is pure (no DNS) so validation is deterministic."
```

---

### Task 3: Doc note + full verification

**Files:**
- Modify: `internal/config/config.go` (comment on the `Model.BaseURL` field)

**Interfaces:**
- Consumes: nothing new.
- Produces: nothing new — documentation + a green full-suite run.

- [ ] **Step 1: Add an https note to the `BaseURL` doc comment**

In `internal/config/config.go`, update the `Model.BaseURL` field comment (currently `// OpenAI: required; Anthropic/Gemini: optional (built-in default endpoint)`) to:

```go
	BaseURL   string `yaml:"base_url"`    // OpenAI: required; Anthropic/Gemini: optional (built-in default endpoint). Must be https when api_key_env is set on a public host (validated).
```

- [ ] **Step 2: Build and run the full test suite**

Run: `go build ./... && go test ./internal/httpx/ ./internal/config/ ./internal/model/...`
Expected: PASS — everything compiles and all model/httpx/config tests are green.

- [ ] **Step 3: Vet**

Run: `go vet ./internal/httpx/ ./internal/config/`
Expected: no output (clean).

- [ ] **Step 4: Commit**

```bash
git add internal/config/config.go
git commit -m "docs(config): note the https requirement on model.base_url with a key"
```

---

## Self-Review

**Spec coverage:**
- S1 (strip key header on host-changing redirect, shared `httpx`, hostname-only compare, all three headers, nil-guard) → Task 1. ✓
- S2 (hard-fail `Validate()` on http+key+public; main/verify/embeddings; pure private-host heuristic with the four suffixes + single-label + localhost + private IPs; unparseable/other-scheme error) → Task 2. ✓
- Verify-inherits-parent-key case → Task 2 Step 4 + test. ✓
- Doc note → Task 3. ✓
- Network-free tests (stub `lookupIP`; pure classifier) → Tasks 1 & 2. ✓

**Placeholder scan:** No TBD/TODO; every code step shows complete code; commands have expected output. ✓

**Type consistency:** `DenyInternalRedirect` signature unchanged across tasks; `checkSecureKeyEndpoint(urlField, keyField, baseURL, apiKeyEnv)` and `isPrivateHost(host)` used with matching signatures in Validate() and tests; test names match their `-run` regexes; struct field names (`BaseURL`, `APIKeyEnv`) match `config.go:233-270`. ✓
