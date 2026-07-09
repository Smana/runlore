// SPDX-License-Identifier: Apache-2.0

// Package mcp is a minimal Model Context Protocol server over the stdio transport
// (newline-delimited JSON-RPC 2.0) — enough to expose RunLore tools to MCP clients
// such as HolmesGPT, kagent, or Claude Desktop. It implements initialize, tools/list,
// tools/call, and ping; tool handlers return plain text. Deliberately dependency-free
// (no SDK) to keep the single static binary.
package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
)

// defaultProtocolVersion is returned when the client doesn't request one. The handful
// of methods here behave identically across MCP revisions, so the server echoes the
// client's requested version when present (maximizing handshake compatibility).
const defaultProtocolVersion = "2024-11-05"

// Tool is an MCP tool: a name, a description, a JSON-Schema for its arguments, and a
// handler returning text (an error is surfaced to the client as an MCP tool error).
type Tool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	Handler     func(ctx context.Context, args json.RawMessage) (string, error)
}

// Server is a minimal MCP server. Construct with NewServer, AddTool, then Serve.
type Server struct {
	name, version string
	log           *slog.Logger
	tools         map[string]Tool
	order         []string
}

// NewServer creates a server advertising name/version. A nil logger discards logs
// (logs MUST NOT go to the stdout MCP channel — pass a stderr logger).
func NewServer(name, version string, log *slog.Logger) *Server {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Server{name: name, version: version, log: log, tools: map[string]Tool{}}
}

// AddTool registers (or replaces) a tool, preserving first-seen order for tools/list.
func (s *Server) AddTool(t Tool) {
	if _, ok := s.tools[t.Name]; !ok {
		s.order = append(s.order, t.Name)
	}
	s.tools[t.Name] = t
}

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"` // absent ⇒ notification (no response)
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Serve reads newline-delimited JSON-RPC requests from in and writes one response line
// per request to out, until in is exhausted or ctx is cancelled. Notifications (no id)
// get no response. json.Encoder writes compact, single-line JSON + a newline — exactly
// the stdio framing MCP expects.
func (s *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024) // tolerate large request lines
	enc := json.NewEncoder(out)
	for sc.Scan() {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var req rpcRequest
		if err := json.Unmarshal(sc.Bytes(), &req); err != nil {
			s.log.Warn("mcp: dropping unparseable request line", "err", err)
			continue // can't form a response without an id
		}
		resp, respond := s.dispatch(ctx, &req)
		if !respond {
			continue
		}
		if err := enc.Encode(resp); err != nil {
			return fmt.Errorf("mcp: write response: %w", err)
		}
	}
	return sc.Err()
}

// dispatch handles one request; respond=false for notifications (and unknown
// notifications), which get no reply.
func (s *Server) dispatch(ctx context.Context, req *rpcRequest) (rpcResponse, bool) {
	if len(req.ID) == 0 { // notification: act on known ones, never reply
		return rpcResponse{}, false
	}
	resp := rpcResponse{JSONRPC: "2.0", ID: req.ID}
	switch req.Method {
	case "initialize":
		resp.Result = s.initializeResult(req.Params)
	case "ping":
		resp.Result = map[string]any{}
	case "tools/list":
		resp.Result = s.toolsList()
	case "tools/call":
		resp.Result = s.toolsCall(ctx, req.Params)
	default:
		resp.Error = &rpcError{Code: -32601, Message: "method not found: " + req.Method}
	}
	return resp, true
}

func (s *Server) initializeResult(params json.RawMessage) map[string]any {
	version := defaultProtocolVersion
	var p struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if len(params) > 0 && json.Unmarshal(params, &p) == nil && p.ProtocolVersion != "" {
		version = p.ProtocolVersion
	}
	return map[string]any{
		"protocolVersion": version,
		"capabilities":    map[string]any{"tools": map[string]any{}},
		"serverInfo":      map[string]any{"name": s.name, "version": s.version},
	}
}

func (s *Server) toolsList() map[string]any {
	list := make([]map[string]any, 0, len(s.order))
	for _, name := range s.order {
		t := s.tools[name]
		schema := t.InputSchema
		if len(schema) == 0 {
			schema = json.RawMessage(`{"type":"object"}`)
		}
		list = append(list, map[string]any{
			"name": t.Name, "description": t.Description, "inputSchema": schema,
		})
	}
	return map[string]any{"tools": list}
}

func (s *Server) toolsCall(ctx context.Context, params json.RawMessage) map[string]any {
	var p struct {
		Name      string          `json:"name"`
		Arguments json.RawMessage `json:"arguments"`
	}
	if err := json.Unmarshal(params, &p); err != nil {
		return toolError("invalid tools/call params: " + err.Error())
	}
	t, ok := s.tools[p.Name]
	if !ok {
		return toolError("unknown tool: " + p.Name)
	}
	args := p.Arguments
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	out, err := t.Handler(ctx, args)
	if err != nil {
		return toolError(err.Error())
	}
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": out}},
		"isError": false,
	}
}

// toolError is the MCP convention for a failed tool call: a normal result with
// isError=true (not a JSON-RPC error), so the model sees the failure as tool output.
func toolError(msg string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": msg}},
		"isError": true,
	}
}
