package mcp

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// rpcEnvelope decodes the request method/id/params on the server side.
type rpcEnvelope struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

func writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func TestClientInitializeAndSession(t *testing.T) {
	var sawSession string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req rpcEnvelope
		_ = json.Unmarshal(body, &req)
		switch req.Method {
		case "initialize":
			w.Header().Set("Mcp-Session-Id", "sess-123")
			writeResult(w, req.ID, map[string]any{"protocolVersion": "2024-11-05"})
		case "notifications/initialized":
			w.WriteHeader(http.StatusAccepted) // notification: no body
		case "tools/list":
			sawSession = r.Header.Get("Mcp-Session-Id") // must carry the session
			writeResult(w, req.ID, map[string]any{"tools": []map[string]any{
				{"name": "query", "description": "run SQL", "inputSchema": json.RawMessage(`{"type":"object"}`)},
			}})
		}
	}))
	defer srv.Close()

	c := NewClient("steampipe", srv.URL, "", nil, nil)
	if err := c.Initialize(context.Background()); err != nil {
		t.Fatalf("Initialize: %v", err)
	}
	tools, err := c.ListTools(context.Background())
	if err != nil {
		t.Fatalf("ListTools: %v", err)
	}
	if sawSession != "sess-123" {
		t.Fatalf("session id not carried on tools/list, got %q", sawSession)
	}
	if len(tools) != 1 || tools[0].Name != "query" {
		t.Fatalf("tools = %+v", tools)
	}
}

func TestClientCallToolText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcEnvelope
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		if req.Method == "tools/call" {
			writeResult(w, req.ID, map[string]any{
				"content": []map[string]any{{"type": "text", "text": "row1\n"}, {"type": "text", "text": "row2"}},
				"isError": false,
			})
			return
		}
		writeResult(w, req.ID, map[string]any{})
	}))
	defer srv.Close()
	c := NewClient("s", srv.URL, "", nil, nil)
	out, err := c.CallTool(context.Background(), "query", json.RawMessage(`{"sql":"select 1"}`))
	if err != nil {
		t.Fatalf("CallTool: %v", err)
	}
	if out != "row1\nrow2" {
		t.Fatalf("content concat = %q", out)
	}
}

func TestClientCallToolIsError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcEnvelope
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		writeResult(w, req.ID, map[string]any{
			"content": []map[string]any{{"type": "text", "text": "boom"}}, "isError": true,
		})
	}))
	defer srv.Close()
	c := NewClient("s", srv.URL, "", nil, nil)
	if _, err := c.CallTool(context.Background(), "x", nil); err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("isError must surface as error containing the text, got %v", err)
	}
}

func TestClientSSEResponse(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req rpcEnvelope
		b, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(b, &req)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		// one SSE event carrying the JSON-RPC result for this id
		msg, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": req.ID,
			"result": map[string]any{"content": []map[string]any{{"type": "text", "text": "streamed"}}}})
		_, _ = io.WriteString(w, "data: "+string(msg)+"\n\n")
	}))
	defer srv.Close()
	c := NewClient("s", srv.URL, "", nil, nil)
	out, err := c.CallTool(context.Background(), "x", nil)
	if err != nil || out != "streamed" {
		t.Fatalf("SSE response: out=%q err=%v", out, err)
	}
}

func TestClientNon2xxNoBodyEcho(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("X-Request-Id", "req-9")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, "SENSITIVE UPSTREAM BODY")
	}))
	defer srv.Close()
	c := NewClient("s", srv.URL, "", nil, nil)
	_, err := c.ListTools(context.Background())
	if err == nil || strings.Contains(err.Error(), "SENSITIVE") {
		t.Fatalf("non-2xx must error without echoing the body, got %v", err)
	}
}
