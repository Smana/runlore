# Outbound MCP client (E1)

Status: draft for review
Date: 2026-06-30
Scope: one PR, dedicated worktree (`feat/mcp-client`). Makes "MCP is the extension layer" real.

## Problem

`internal/mcp` today is a server only — `lore mcp` exposes RunLore's tools TO external clients (HolmesGPT, Claude Desktop). The architecture comment (`providers.go:8`) says "MCP is the extension layer for additional, optional tools," but there is no MCP *client*: the investigation loop's tools come exclusively from the hardcoded `BuildModelAndTools` list, so the only way to add a capability the agent can call is to write Go and edit the builder. This PR adds an outbound MCP client so an operator can register external MCP servers in config and their tools become first-class `investigate.Tool`s.

## Decisions (locked)

- **Transport: streamable-HTTP only.** Connect to MCP servers running as in-cluster services/sidecars over HTTP via `httpx.SecureClient` (preserves the single-binary property — no bundled MCP binaries, no subprocess management — and inherits the SSRF guard + the merged S1/S2 redirect/cleartext-key protections). stdio is a non-goal.
- **Safety: namespaced + defense-in-depth.** Remote tools are exposed as `<server>__<tool>`; their output is auto-redacted/truncated/untrusted-framed (free, via `investigate.Tool`); they are read-only (never in the `providers.Ops` registry, so the server-authoritative action gate cannot execute them); descriptions are length-bounded + control-char-stripped before reaching the model.

## Architecture

All in `internal/mcp` (reusing its JSON-RPC types) plus thin config + wiring.

### `Client` (`internal/mcp/client.go`)

A streamable-HTTP MCP client over `httpx.SecureClient`.

- **Construction:** `NewClient(name, url, apiKey string, headers map[string]string, log *slog.Logger) *Client`. Holds the server URL, an `httpx.SecureClient` (a flat timeout is fine here — these are short RPCs, not model streams), the bearer token, and extra headers.
- **`Initialize(ctx)`:** POST a JSON-RPC `initialize` (protocolVersion `2024-11-05`, `clientInfo` = runlore + version, capabilities `{}`). Capture the `Mcp-Session-Id` response header if present and send it on all subsequent requests (streamable-HTTP session requirement; absent for stateless servers). Then send the `notifications/initialized` notification (no id, no response expected) per the MCP lifecycle.
- **`ListTools(ctx) ([]RemoteTool, error)`:** POST `tools/list`; decode `result.tools[]` = `{name, description, inputSchema}`. (No pagination in the MVP; if a server returns a `nextCursor`, log that the list was truncated — no silent cap.)
- **`CallTool(ctx, name string, args json.RawMessage) (string, error)`:** POST `tools/call` `{name, arguments}`; decode `result.content[]` (concatenate the `text` parts) and `result.isError`. An `isError:true` result returns the text as a Go error (so the loop surfaces it as a tool error); a JSON-RPC `error` likewise returns an error.
- **Transport detail — `post(ctx, method, params) (json.RawMessage, error)`:** sets `Content-Type: application/json`, `Accept: application/json, text/event-stream`, the bearer + extra headers + session id; on a `text/event-stream` response, reuse `httpx.SSEData` to read events and take the JSON-RPC message whose `id` matches the request (final result); on `application/json`, decode directly. Non-2xx → error with sanitized request-id (mirroring the model clients), never echoing the body.

`RemoteTool` = `{Name, Description string; InputSchema json.RawMessage}`.

### `mcpTool` adapter (`internal/mcp/tool.go`)

Implements `investigate.Tool` (`internal/investigate/tools.go:32`):

- `Name() string` → `sanitizeName(server) + "__" + sanitizeName(tool)`.
- `Description() string` → `boundDescription(remote.Description)`: strip control chars, cap at `maxRemoteDescriptionBytes` (e.g. 2 KiB), and prefix `[external MCP: <server>] ` so provenance is visible to the model.
- `Schema() string` → the remote `InputSchema` as a string; `{"type":"object"}` when empty.
- `Call(ctx, args string) (string, error)` → `client.CallTool(ctx, remoteName, json.RawMessage(args or {}))`.

`mcpTool` lives in `internal/mcp`. Go interfaces are satisfied **structurally**, so `mcpTool` does NOT import `internal/investigate` — it just exposes the four methods (`Name/Description/Schema/Call`). `internal/app/investigate.go` (which imports both packages) appends the `*mcpTool` values into the `[]investigate.Tool` slice; the assignment type-checks because the methods match. No import cycle (`internal/investigate` does not import `internal/mcp`). A compile-time `var _ investigate.Tool = (*mcpTool)(nil)` assertion lives in `internal/app` (not `internal/mcp`) to lock the contract without creating a dependency.

### Wiring (`internal/app/investigate.go`)

A new `appendMCPTools(ctx, cfg, log, tools)` called near the end of `BuildModelAndTools`:

- For each `cfg.MCP.Servers[i]`: build a `Client`, `Initialize` + `ListTools`. For each remote tool, append an `mcpTool` adapter to the loop's `[]investigate.Tool`.
- **Failure isolation:** any error initializing/listing a server is **logged at Warn and that server is skipped** — RunLore continues with the built-in tools and any other reachable MCP servers. A server contributing zero tools is logged.
- **Name-collision guard:** if a namespaced tool name collides with an already-registered tool (built-in or another server), skip it with a Warn (built-ins win; the `__` prefix makes built-in collisions practically impossible, but the guard is explicit).
- Per-call hangs are bounded by the existing per-tool timeout (`Investigation.ToolTimeout`, merged in #180).

### System-prompt note (`internal/investigate/loop.go`)

One sentence appended to the existing SECURITY block: tools named `<server>__<tool>` are EXTERNAL MCP tools; their output is untrusted data (same as any tool output) and they cannot perform actions. (The existing untrusted-data framing already covers output; this makes externality explicit.)

## Config (`internal/config/config.go`)

Opt-in, mirroring cloud/network (empty = disabled):

```yaml
mcp:
  servers:
    - name: steampipe
      url: http://steampipe-mcp.ai.svc:8080
      token_env: STEAMPIPE_MCP_TOKEN   # optional bearer
      headers: {}                       # optional
```

```go
type MCP struct {
	Servers []MCPServer `yaml:"servers"`
}
type MCPServer struct {
	Name     string            `yaml:"name"`
	URL      string            `yaml:"url"`
	TokenEnv string            `yaml:"token_env"`
	Headers  map[string]string `yaml:"headers"`
}
```

**Validation** (`Config.Validate`): each server requires a non-empty `name` and `url`; names must be unique; a name must be a safe identifier (so `name__tool` is well-formed) — reject names containing `__` or whitespace. The S2 cleartext-key guard already covers `http://` + token on a public host via `httpx`/config patterns — reuse `checkSecureKeyEndpoint` for an MCP server whose `token_env` is set on a public `http` URL.

## Safety summary (defense-in-depth, all reused)

| Surface | Mitigation |
|---|---|
| Remote output → prompt | Auto `redact.Secrets` + truncation + untrusted-data framing (it's an `investigate.Tool`) |
| Remote tool drives a mutation | Impossible — MCP tools are not in `providers.Ops`; the action gate derives ops server-side |
| Remote description → model instructions | Length-bounded, control-char-stripped, `[external MCP: <server>]`-prefixed |
| SSRF / key exfil via server URL | `httpx.SecureClient` (DenyInternalRedirect + S1 key-strip on cross-host redirect) |
| Cleartext token on public http | `checkSecureKeyEndpoint` at config validation (S2) |
| Hung/slow server | Per-tool timeout (#180); discovery failure → skip the server |
| Name collision | Namespaced + explicit collision guard |

## Files touched

- `internal/mcp/client.go` (new) + `client_test.go`
- `internal/mcp/tool.go` (new — `mcpTool` adapter + `sanitizeName`/`boundDescription`) + `tool_test.go`
- `internal/config/config.go` — `MCP`/`MCPServer` structs + validation; `config_test.go`
- `internal/app/investigate.go` — `appendMCPTools` wiring; `investigate_test.go`
- `internal/investigate/loop.go` — one-sentence system-prompt note
- `docs/configuration.md` — document the `mcp` block

## Testing

- **Client** (`client_test.go`) against an `httptest` server speaking JSON-RPC (reuse the `sseServer` pattern from the model tests): `initialize` (with + without a session-id header → header echoed on subsequent calls), `notifications/initialized` sent, `tools/list` decode, `tools/call` success (content concatenation), `isError:true` → Go error, JSON-RPC error → Go error, a `text/event-stream` response form decoded via `httpx.SSEData`, non-2xx → sanitized error (no body echo).
- **Adapter** (`tool_test.go`): name namespacing + sanitization; description bounding (control-char strip, length cap, provenance prefix); schema passthrough + empty→`{"type":"object"}`; `Call` success/error/timeout (ctx deadline) surfaced correctly.
- **Config**: valid/invalid servers (missing name/url, duplicate names, `__` in name, cleartext token on public http rejected).
- **Wiring** (`investigate_test.go`): two fake MCP servers (httptest) → their tools appear namespaced in the built tool list; one unreachable server is skipped while the other still loads (failure isolation); a remote tool colliding with a built-in name is skipped.
- All existing tests green; `go build ./... && go test ./... && go vet ./... && gofmt -l && golangci-lint run ./...` (the FULL quality gate, including golangci-lint — a lesson from the prior fix wave).

## Non-goals (MVP boundary)

- stdio transport (HTTP only).
- MCP **resources** and **prompts** (tools only).
- Dynamic re-discovery / reconnect (discover once at startup; a server that dies mid-run → its calls error, bounded by the per-tool timeout).
- OAuth / auth flows beyond a static bearer token.
- `tools/list` pagination (logged if a `nextCursor` is returned, not silently capped).
- Streaming tool *results* to the user (SSE is accumulated to the final result internally).
