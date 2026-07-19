# MCP Remote-Tool Allowlist Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the outbound MCP client's "read-only extension layer" enforceable: a per-server `tools:` allowlist keeps un-listed remote tools from ever reaching the model, with a fail-closed `mcp.require_allowlist` mode — and `tools/list` pagination is fixed so the allowlist is checked against the *complete* advertised list (roadmap N9).

**Architecture:** Config-side, `MCPServer` gains `Tools []string` (exact names, no globs) and `MCP` gains `RequireAllowlist bool`, both enforced in `config.Validate`. Enforcement lives at discovery in `appendMCPTools` (`internal/app/investigate.go:287`): tools outside the allowlist are never adapted or registered, with one aggregated INFO log for the skipped and one WARN for allowlisted-but-not-advertised names (typo guard). `Client.ListTools` (`internal/mcp/client.go:74`) follows `nextCursor` with a bounded page cap so neither the allowlist nor deny-by-default operates on a silently truncated list.

**Tech Stack:** Go 1.26 (toolchain go1.26.5), standard library only; existing `internal/mcp` + `internal/config` + `internal/app`. No new deps.

## Global Constraints

- Quality gate before every commit: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...` — 0 issues, `gofmt -l` empty.
- SPDX `// SPDX-License-Identifier: Apache-2.0` as line 1 of any new `.go` file (this plan modifies existing files only).
- Conventional Commits; NO co-author trailer; PR metadata in English.
- Compatibility: an absent/empty `tools:` list preserves today's behavior (all advertised tools) unless `mcp.require_allowlist: true` — a config with no MCP changes must behave identically.
- Existing MCP invariants stay untouched: namespacing `<server>__<tool>`, built-ins-win collision handling, failure isolation per server, no retry on `tools/call`.

---

### Task 1: Config — `tools:` allowlist + `require_allowlist`, validated

**Files:**
- Modify: `internal/config/config.go:192-202` (`MCP` / `MCPServer` structs), `internal/config/config.go:1056-1078` (the `Validate` MCP loop)
- Test: the config package's existing validate test file (add cases beside the current `mcp.servers` cases)

**Interfaces:**
- Produces (consumed by Task 3): `config.MCPServer.Tools []string` (yaml `tools`); `config.MCP.RequireAllowlist bool` (yaml `require_allowlist`).

- [ ] **Step 1: Write the failing tests**

Add to the config validate tests (mirror the style of the existing `mcp.servers` cases — build a minimal valid config with one MCP server, mutate, expect an error containing the quoted fragment):

```go
func TestValidateMCPToolAllowlist(t *testing.T) {
	base := func() *Config {
		c := &Config{}
		c.MCP.Servers = []MCPServer{{Name: "kb", Endpoint: Endpoint{URL: "https://mcp.example/mcp"}}}
		return c
	}
	t.Run("empty tool name rejected", func(t *testing.T) {
		c := base()
		c.MCP.Servers[0].Tools = []string{""}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "mcp.servers[kb].tools") {
			t.Fatalf("want tools validation error, got %v", err)
		}
	})
	t.Run("whitespace tool name rejected", func(t *testing.T) {
		c := base()
		c.MCP.Servers[0].Tools = []string{"a b"}
		if err := c.Validate(); err == nil {
			t.Fatal("want error for whitespace tool name")
		}
	})
	t.Run("duplicate tool name rejected", func(t *testing.T) {
		c := base()
		c.MCP.Servers[0].Tools = []string{"query", "query"}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "duplicate") {
			t.Fatalf("want duplicate error, got %v", err)
		}
	})
	t.Run("require_allowlist without tools fails closed", func(t *testing.T) {
		c := base()
		c.MCP.RequireAllowlist = true
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "require_allowlist") {
			t.Fatalf("want require_allowlist error, got %v", err)
		}
	})
	t.Run("require_allowlist with tools passes", func(t *testing.T) {
		c := base()
		c.MCP.RequireAllowlist = true
		c.MCP.Servers[0].Tools = []string{"query"}
		if err := c.Validate(); err != nil {
			t.Fatalf("valid allowlisted config rejected: %v", err)
		}
	})
}
```

If the surrounding test file builds `Validate`-able configs through a helper (some checks may require model/server fields), reuse that helper instead of the bare `&Config{}` — match whatever the adjacent MCP validate tests do.

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/config/ -run TestValidateMCPToolAllowlist -v`
Expected: FAIL — `unknown field Tools` / `unknown field RequireAllowlist`

- [ ] **Step 3: Implement**

Structs:

```go
// MCP configures outbound connections to external MCP servers whose tools the
// investigation loop may call. Empty Servers disables it (the default — MCP is opt-in).
type MCP struct {
	Servers []MCPServer `yaml:"servers"`

	// RequireAllowlist refuses startup unless EVERY server declares a tools
	// allowlist — deny-by-default for operators who treat remote MCP servers
	// as untrusted. Default false: a listed server exposes all its tools,
	// matching pre-allowlist behavior.
	RequireAllowlist bool `yaml:"require_allowlist"`
}

// MCPServer is one external MCP server reachable over streamable-HTTP.
type MCPServer struct {
	Name     string `yaml:"name"` // identifier; namespaces its tools as name__tool
	Endpoint `yaml:",inline"`

	// Tools is an exact-name allowlist of remote tools to expose to the model
	// (pre-namespacing names, as advertised by tools/list). Empty ⇒ every
	// advertised tool (unless mcp.require_allowlist). Enforced at discovery:
	// a tool outside the list is never registered, so it cannot be called.
	Tools []string `yaml:"tools"`
}
```

In the `Validate` MCP loop (inside the existing `for i, s := range c.MCP.Servers`, after the `checkSecureKeyEndpoint` call):

```go
		seenTool := map[string]bool{}
		for j, tn := range s.Tools {
			if tn == "" || strings.ContainsAny(tn, " \t") {
				return fmt.Errorf("mcp.servers[%s].tools[%d]: tool name must be non-empty without whitespace", s.Name, j)
			}
			if seenTool[tn] {
				return fmt.Errorf("mcp.servers[%s].tools: duplicate tool name %q", s.Name, tn)
			}
			seenTool[tn] = true
		}
		if c.MCP.RequireAllowlist && len(s.Tools) == 0 {
			return fmt.Errorf("mcp.require_allowlist: server %q declares no tools allowlist (add mcp.servers[%s].tools or disable require_allowlist)", s.Name, s.Name)
		}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `go test ./internal/config/ -run TestValidateMCPToolAllowlist -v`
Expected: PASS (all five subtests)

- [ ] **Step 5: Commit**

```bash
git add internal/config/
git commit -m "feat(config): mcp per-server tools allowlist and require_allowlist gate"
```

---

### Task 2: `ListTools` pagination — follow `nextCursor`, bounded

**Files:**
- Modify: `internal/mcp/client.go:73-90` (`ListTools`)
- Test: `internal/mcp/client_test.go`

**Interfaces:**
- Produces: `ListTools` unchanged signature, now returning the union of all pages; package const `maxToolListPages = 16`.
- Consumes: existing `(*Client).rpc` (idempotent discovery ⇒ keep `attempts=3` per page).

- [ ] **Step 1: Write the failing tests**

Add to `client_test.go`, following its existing httptest+JSON-RPC decode pattern:

```go
// TestListToolsPagination: two pages joined via nextCursor; the second request
// must echo the cursor back in params.
func TestListToolsPagination(t *testing.T) {
	var gotCursors []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		if req.Method != "tools/list" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
			return
		}
		gotCursors = append(gotCursors, req.Params.Cursor)
		if req.Params.Cursor == "" {
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []map[string]any{{"name": "a"}}, "nextCursor": "p2"}})
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"tools": []map[string]any{{"name": "b"}}}})
	}))
	defer srv.Close()
	c := NewClient("s", srv.URL, "", nil, nil)
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 2 || tools[0].Name != "a" || tools[1].Name != "b" {
		t.Fatalf("want [a b], got %+v", tools)
	}
	if len(gotCursors) != 2 || gotCursors[1] != "p2" {
		t.Fatalf("cursor not echoed: %v", gotCursors)
	}
}

// TestListToolsPageCap: a server that never stops paginating is cut off at the
// page cap and still returns what was collected.
func TestListToolsPageCap(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"tools": []map[string]any{{"name": "x"}}, "nextCursor": "again"}})
	}))
	defer srv.Close()
	c := NewClient("s", srv.URL, "", nil, nil)
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != maxToolListPages {
		t.Fatalf("want %d tools (one per capped page), got %d", maxToolListPages, len(tools))
	}
}
```

(The initialize handshake is not required before `ListTools` in the existing tests — keep that.)

- [ ] **Step 2: Run tests to verify they fail**

Run: `go test ./internal/mcp/ -run 'TestListTools(Pagination|PageCap)' -v`
Expected: FAIL — pagination test gets 1 tool; page-cap test gets 1 tool / `undefined: maxToolListPages`

- [ ] **Step 3: Implement**

Replace `ListTools`:

```go
// maxToolListPages bounds tools/list cursor-following so a misbehaving server
// can't spin discovery forever. 16 pages of any sane page size covers real
// servers by orders of magnitude.
const maxToolListPages = 16

// ListTools returns the server's advertised tools, following nextCursor
// pagination (bounded) so callers — including allowlist enforcement — see the
// COMPLETE list. Each page retries up to 3 times on transient failures.
func (c *Client) ListTools(ctx context.Context) ([]RemoteTool, error) {
	var all []RemoteTool
	params := map[string]any{}
	for page := 0; page < maxToolListPages; page++ {
		raw, err := c.rpc(ctx, "tools/list", params, 3)
		if err != nil {
			return nil, err
		}
		var res struct {
			Tools      []RemoteTool `json:"tools"`
			NextCursor string       `json:"nextCursor"`
		}
		if err := json.Unmarshal(raw, &res); err != nil {
			return nil, fmt.Errorf("mcp tools/list decode: %w", err)
		}
		all = append(all, res.Tools...)
		if res.NextCursor == "" {
			return all, nil
		}
		params = map[string]any{"cursor": res.NextCursor}
	}
	c.log.Warn("mcp: tools/list exceeded the page cap; list may be truncated", "server", c.name, "pages", maxToolListPages)
	return all, nil
}
```

- [ ] **Step 4: Run the package suite**

Run: `go test ./internal/mcp/ -v`
Expected: PASS — including every pre-existing `ListTools` test (single-page behavior unchanged)

- [ ] **Step 5: Commit**

```bash
git add internal/mcp/client.go internal/mcp/client_test.go
git commit -m "fix(mcp): follow tools/list pagination with a bounded page cap"
```

---

### Task 3: Enforcement at discovery — skip un-allowlisted tools

**Files:**
- Modify: `internal/app/investigate.go:287-324` (`appendMCPTools`)
- Test: `internal/app/investigate_test.go` (beside `TestAppendMCPToolsSkipsUnreachable`)

**Interfaces:**
- Consumes: `config.MCPServer.Tools` (Task 1); complete tool list from `ListTools` (Task 2).
- Produces: no signature change — `appendMCPTools(ctx, cfg, log, tools) []investigate.Tool` filters before adapting.

- [ ] **Step 1: Write the failing test**

Add beside `TestAppendMCPToolsSkipsUnreachable` (`investigate_test.go:344`), reusing its httptest JSON-RPC handler shape:

```go
// TestAppendMCPToolsAllowlist: with a tools allowlist, only listed remote tools
// are registered; unlisted advertised tools never become investigate.Tools.
func TestAppendMCPToolsAllowlist(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
		}
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		switch req.Method {
		case "tools/list":
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID,
				"result": map[string]any{"tools": []map[string]any{
					{"name": "query", "description": "d"},
					{"name": "delete_everything", "description": "d"},
				}}})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": map[string]any{}})
		}
	}))
	defer srv.Close()

	cfg := &config.Config{MCP: config.MCP{Servers: []config.MCPServer{
		{Name: "kb", Endpoint: config.Endpoint{URL: srv.URL}, Tools: []string{"query", "not_advertised"}},
	}}}
	var tools []investigate.Tool
	tools = appendMCPTools(context.Background(), cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), tools)

	var names []string
	for _, tl := range tools {
		names = append(names, tl.Name())
	}
	if len(names) != 1 || names[0] != "kb__query" {
		t.Fatalf("want only kb__query (delete_everything filtered), got %v", names)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/app/ -run TestAppendMCPToolsAllowlist -v`
Expected: FAIL — got `[kb__query kb__delete_everything]`

- [ ] **Step 3: Implement**

Inside `appendMCPTools`'s per-server loop, between the `ListTools` call and the adapt loop:

```go
		allowed := map[string]bool{}
		for _, tn := range s.Tools {
			allowed[tn] = true
		}
		advertised := map[string]bool{}
		var skipped []string
		added := 0
		for _, rt := range remote {
			advertised[rt.Name] = true
			if len(allowed) > 0 && !allowed[rt.Name] {
				skipped = append(skipped, rt.Name)
				continue
			}
			tl := mcp.NewTool(c, rt)
			if have[tl.Name()] {
				log.Warn("mcp: skipping tool (name collision)", "server", s.Name, "tool", tl.Name())
				continue
			}
			have[tl.Name()] = true
			tools = append(tools, tl)
			added++
		}
		if len(skipped) > 0 {
			log.Info("mcp: tools excluded by allowlist", "server", s.Name, "skipped", skipped)
		}
		for tn := range allowed {
			if !advertised[tn] {
				log.Warn("mcp: allowlisted tool not advertised by server (typo?)", "server", s.Name, "tool", tn)
			}
		}
		log.Info("mcp: registered server tools", "server", s.Name, "tools", added)
```

(This replaces the existing adapt loop + final Info line; the collision and failure-isolation behavior is unchanged.)

- [ ] **Step 4: Run the package suite**

Run: `go test ./internal/app/ -run TestAppendMCPTools -v`
Expected: PASS — both the new allowlist test and the pre-existing unreachable-server test

- [ ] **Step 5: Commit**

```bash
git add internal/app/investigate.go internal/app/investigate_test.go
git commit -m "feat(mcp): enforce per-server tool allowlist at discovery"
```

---

### Task 4: Docs — enforceable read-only, config reference

**Files:**
- Modify: `docs/mcp.md:85-108` (outbound config example + "What to know" bullets)
- Modify: `docs/security-model.md` (one honest paragraph on remote-tool trust)

- [ ] **Step 1: Update `docs/mcp.md`**

Extend the config example:

```yaml
mcp:
  # require_allowlist: true                # fail closed: every server MUST list tools
  servers:
    - name: runbooks                       # tools appear as runbooks__<tool>
      url: https://mcp.internal.example/mcp # streamable-HTTP transport
      # token_env: RUNBOOKS_MCP_TOKEN       # optional bearer token (env-var name)
      # tools: [search, get]                # exact-name allowlist; omit ⇒ all tools
```

Replace the current namespacing bullet's read-only sentence and add one bullet:

- Amend the **Namespacing** bullet: after "the action gate only knows the built-in operations)", append: "Note the gate constrains RunLore's *own* write path — a remote tool with server-side side effects is only prevented from being *called* by the allowlist below."
- New bullet **Allowlist**: "`tools:` is an exact-name allowlist enforced at discovery — an un-listed tool is never registered, so the model cannot call it. `mcp.require_allowlist: true` refuses startup unless every server declares one (deny-by-default). Discovery follows `tools/list` pagination (bounded at 16 pages), so the allowlist is applied to the server's complete tool list."

- [ ] **Step 2: Update `docs/security-model.md`**

In the section covering egress/tool trust (place it beside the existing MCP-adjacent or tool-output-trust prose; if no MCP subsection exists, add one titled "External MCP tools"):

> **External MCP tools.** Remote MCP tools run outside RunLore's action gate: the gate stops RunLore from *executing* cluster operations, but a remote tool that mutates state server-side would do so the moment it is called. Treat every configured MCP server as part of your TCB. Two controls bound this: per-server `mcp.servers[].tools` allowlists (a tool not listed is never registered, so the model can never call it), and `mcp.require_allowlist: true` to refuse startup unless every server is allowlisted. Tool *output* remains untrusted data (redaction + no-instruction-following), and per-server discovery failures are isolated.

- [ ] **Step 3: Full gate**

Run: `go build ./... && go vet ./... && go test ./... && gofmt -l . && golangci-lint run ./...`
Expected: all green, `0 issues.`

- [ ] **Step 4: Commit**

```bash
git add docs/mcp.md docs/security-model.md
git commit -m "docs(mcp): document the enforceable tool allowlist and remote-tool trust model"
```
