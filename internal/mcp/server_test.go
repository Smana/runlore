package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func testServer() *Server {
	s := NewServer("runlore-test", "v0", nil)
	s.AddTool(Tool{
		Name:        "echo",
		Description: "echoes its args",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"msg":{"type":"string"}}}`),
		Handler:     func(_ context.Context, args json.RawMessage) (string, error) { return "got:" + string(args), nil },
	})
	s.AddTool(Tool{
		Name:        "boom",
		Description: "always errors",
		Handler:     func(context.Context, json.RawMessage) (string, error) { return "", fmt.Errorf("kaboom") },
	})
	return s
}

// run feeds newline-delimited requests through Serve and decodes the response lines.
func run(t *testing.T, s *Server, lines ...string) []map[string]any {
	t.Helper()
	var out bytes.Buffer
	if err := s.Serve(context.Background(), strings.NewReader(strings.Join(lines, "\n")+"\n"), &out); err != nil {
		t.Fatalf("Serve: %v", err)
	}
	var resps []map[string]any
	dec := json.NewDecoder(&out)
	for dec.More() {
		var m map[string]any
		if err := dec.Decode(&m); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		resps = append(resps, m)
	}
	return resps
}

func TestInitializeEchoesProtocolAndAdvertisesTools(t *testing.T) {
	r := run(t, testServer(), `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`)
	if len(r) != 1 {
		t.Fatalf("want 1 response, got %d", len(r))
	}
	res := r[0]["result"].(map[string]any)
	if res["protocolVersion"] != "2025-06-18" {
		t.Fatalf("should echo the client protocol version, got %v", res["protocolVersion"])
	}
	if _, ok := res["capabilities"].(map[string]any)["tools"]; !ok {
		t.Fatal("must advertise the tools capability")
	}
	if res["serverInfo"].(map[string]any)["name"] != "runlore-test" {
		t.Fatalf("serverInfo.name: %v", res["serverInfo"])
	}
}

func TestNotificationGetsNoResponse(t *testing.T) {
	r := run(t, testServer(), `{"jsonrpc":"2.0","method":"notifications/initialized"}`)
	if len(r) != 0 {
		t.Fatalf("a notification (no id) must not get a response, got %d", len(r))
	}
}

func TestToolsList(t *testing.T) {
	r := run(t, testServer(), `{"jsonrpc":"2.0","id":2,"method":"tools/list"}`)
	tools := r[0]["result"].(map[string]any)["tools"].([]any)
	if len(tools) != 2 || tools[0].(map[string]any)["name"] != "echo" {
		t.Fatalf("tools/list wrong: %v", tools)
	}
	if _, ok := tools[0].(map[string]any)["inputSchema"]; !ok {
		t.Fatal("tool must carry an inputSchema")
	}
}

func TestToolsCallSuccess(t *testing.T) {
	r := run(t, testServer(), `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"echo","arguments":{"msg":"hi"}}}`)
	res := r[0]["result"].(map[string]any)
	if res["isError"] != false {
		t.Fatalf("expected success, got isError=%v", res["isError"])
	}
	text := res["content"].([]any)[0].(map[string]any)["text"].(string)
	if !strings.Contains(text, `"msg":"hi"`) {
		t.Fatalf("echoed text missing args: %q", text)
	}
}

func TestToolsCallErrorsAreResults(t *testing.T) {
	// A handler error and an unknown tool are both MCP tool errors (isError result),
	// NOT JSON-RPC errors.
	boom := run(t, testServer(), `{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"boom","arguments":{}}}`)
	if boom[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("handler error should set isError=true")
	}
	unknown := run(t, testServer(), `{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"nope"}}`)
	if unknown[0]["result"].(map[string]any)["isError"] != true {
		t.Fatal("unknown tool should set isError=true")
	}
}

func TestUnknownMethodIsJSONRPCError(t *testing.T) {
	r := run(t, testServer(), `{"jsonrpc":"2.0","id":6,"method":"frobnicate"}`)
	if r[0]["error"] == nil {
		t.Fatal("unknown method should return a JSON-RPC error")
	}
	if int(r[0]["error"].(map[string]any)["code"].(float64)) != -32601 {
		t.Fatalf("want -32601, got %v", r[0]["error"])
	}
}
