package mcp

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/investigate"
)

func TestToolNameNamespaced(t *testing.T) {
	c := NewClient("steam pipe", "http://x", "", nil, nil)
	tl := NewTool(c, RemoteTool{Name: "query", Description: "d"})
	if got := tl.Name(); got != "steam_pipe__query" {
		t.Fatalf("namespaced name = %q", got)
	}
}

func TestToolDescriptionBounded(t *testing.T) {
	c := NewClient("s", "http://x", "", nil, nil)
	raw := "line1\x00\x07ctrl" + strings.Repeat("y", 5000)
	tl := NewTool(c, RemoteTool{Name: "q", Description: raw})
	d := tl.Description()
	if strings.ContainsAny(d, "\x00\x07") {
		t.Fatal("control chars must be stripped")
	}
	if !strings.HasPrefix(d, "[external MCP: s] ") {
		t.Fatalf("missing provenance prefix: %q", d[:30])
	}
	if len(d) > 2070 { // 2KiB body cap (pre-condition: checked before write) + 18-byte prefix + max 4-byte rune
		t.Fatalf("description not bounded: %d bytes", len(d))
	}
}

func TestToolSchemaEmptyDefault(t *testing.T) {
	c := NewClient("s", "http://x", "", nil, nil)
	tl := NewTool(c, RemoteTool{Name: "q"})
	if tl.Schema() != `{"type":"object"}` {
		t.Fatalf("empty schema default = %q", tl.Schema())
	}
	tl2 := NewTool(c, RemoteTool{Name: "q", InputSchema: json.RawMessage(`{"type":"object","properties":{}}`)})
	if tl2.Schema() != `{"type":"object","properties":{}}` {
		t.Fatalf("schema passthrough = %q", tl2.Schema())
	}
}

// Compile-time assertion: *Adapter must satisfy the investigate.Tool interface.
var _ investigate.Tool = (*Adapter)(nil)
