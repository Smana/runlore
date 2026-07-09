// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"context"
	"regexp"
	"strings"
)

const maxRemoteDescriptionBytes = 2048

// Adapter adapts a remote MCP tool to the investigate.Tool contract (satisfied
// structurally — this package does not import internal/investigate).
type Adapter struct {
	client     *Client
	remoteName string
	name       string
	desc       string
	schema     string
}

// NewTool builds an adapter for one discovered remote tool, namespaced by server.
// The returned *Adapter satisfies the investigate.Tool contract structurally.
func NewTool(c *Client, rt RemoteTool) *Adapter {
	schema := strings.TrimSpace(string(rt.InputSchema))
	if schema == "" {
		schema = `{"type":"object"}`
	}
	return &Adapter{
		client:     c,
		remoteName: rt.Name,
		name:       SanitizeName(c.name) + "__" + SanitizeName(rt.Name),
		desc:       boundDescription(c.name, rt.Description),
		schema:     schema,
	}
}

// Name returns the namespaced tool name in the form <server>__<tool>.
func (t *Adapter) Name() string { return t.name }

// Description returns the bounded, sanitized description with a provenance prefix.
func (t *Adapter) Description() string { return t.desc }

// Schema returns the JSON schema string for the tool's input, defaulting to
// {"type":"object"} when the remote tool advertises none.
func (t *Adapter) Schema() string { return t.schema }

// Call delegates to the remote MCP server, passing args as-is.
func (t *Adapter) Call(ctx context.Context, args string) (string, error) {
	return t.client.CallTool(ctx, t.remoteName, []byte(args))
}

var sanitizeNameRE = regexp.MustCompile(`[^a-z0-9]+`)

// SanitizeName makes a server/tool name safe for the model-facing tool name: lowercase,
// non-alphanumeric runs collapsed to a single underscore, trimmed.
func SanitizeName(s string) string {
	return strings.Trim(sanitizeNameRE.ReplaceAllString(strings.ToLower(s), "_"), "_")
}

// boundDescription strips control characters, caps length, and prefixes provenance so the
// model treats it as an external tool.
func boundDescription(server, desc string) string {
	var b strings.Builder
	for _, r := range desc {
		if b.Len() >= maxRemoteDescriptionBytes {
			break
		}
		if r >= 0x20 && r != 0x7f {
			b.WriteRune(r)
		} else if r == '\n' || r == '\t' {
			b.WriteByte(' ')
		}
	}
	return "[external MCP: " + server + "] " + strings.TrimSpace(b.String())
}
