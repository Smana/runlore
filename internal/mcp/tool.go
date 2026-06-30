package mcp

import (
	"context"
	"strings"
)

const maxRemoteDescriptionBytes = 2048

// mcpTool adapts a remote MCP tool to the investigate.Tool contract (satisfied
// structurally — this package does not import internal/investigate).
type mcpTool struct {
	client     *Client
	remoteName string
	name       string
	desc       string
	schema     string
}

// NewTool builds an adapter for one discovered remote tool, namespaced by server.
// The returned *mcpTool satisfies the investigate.Tool contract structurally.
//
//nolint:revive // mcpTool is intentionally unexported; Tool is taken by the server-side struct.
func NewTool(c *Client, rt RemoteTool) *mcpTool {
	schema := strings.TrimSpace(string(rt.InputSchema))
	if schema == "" {
		schema = `{"type":"object"}`
	}
	return &mcpTool{
		client:     c,
		remoteName: rt.Name,
		name:       SanitizeName(c.name) + "__" + SanitizeName(rt.Name),
		desc:       boundDescription(c.name, rt.Description),
		schema:     schema,
	}
}

// Name returns the namespaced tool name in the form <server>__<tool>.
func (t *mcpTool) Name() string { return t.name }

// Description returns the bounded, sanitized description with a provenance prefix.
func (t *mcpTool) Description() string { return t.desc }

// Schema returns the JSON schema string for the tool's input, defaulting to
// {"type":"object"} when the remote tool advertises none.
func (t *mcpTool) Schema() string { return t.schema }

// Call delegates to the remote MCP server, passing args as-is.
func (t *mcpTool) Call(ctx context.Context, args string) (string, error) {
	return t.client.CallTool(ctx, t.remoteName, []byte(args))
}

// SanitizeName makes a server/tool name safe for the model-facing tool name: lowercase,
// non-alphanumeric runs collapsed to a single underscore, trimmed.
func SanitizeName(s string) string {
	var b strings.Builder
	prevUnderscore := false
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			prevUnderscore = false
		} else if !prevUnderscore {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	return strings.Trim(b.String(), "_")
}

// boundDescription strips control characters, caps length, and prefixes provenance so the
// model treats it as an external tool.
func boundDescription(server, desc string) string {
	var b strings.Builder
	for _, r := range desc {
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		} else if r == '\n' || r == '\t' {
			b.WriteByte(' ')
		}
		if b.Len() >= maxRemoteDescriptionBytes {
			break
		}
	}
	return "[external MCP: " + server + "] " + strings.TrimSpace(b.String())
}
