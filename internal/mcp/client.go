package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/httpx"
)

const clientProtocolVersion = "2024-11-05"

// RemoteTool is a tool advertised by an MCP server's tools/list.
type RemoteTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
}

// Client is a minimal streamable-HTTP MCP client. It speaks JSON-RPC 2.0 over a single
// POST endpoint, handling both a JSON and an SSE response, and carries the server's
// session id (if any) across requests. All HTTP goes through httpx.SecureClient so the
// SSRF guard and cross-host key-stripping apply.
type Client struct {
	name      string
	url       string
	apiKey    string
	headers   map[string]string
	http      *http.Client
	log       *slog.Logger
	sessionID string
	nextID    int
}

// NewClient builds an MCP client for a server. A nil logger discards logs.
func NewClient(name, url, apiKey string, headers map[string]string, log *slog.Logger) *Client {
	if log == nil {
		log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return &Client{
		name: name, url: strings.TrimRight(url, "/"), apiKey: apiKey, headers: headers,
		http: httpx.SecureClient(30 * time.Second), log: log,
	}
}

// Name returns the human-readable name of this MCP server connection.
func (c *Client) Name() string { return c.name }

// Initialize performs the MCP handshake: initialize (capturing any session id) then the
// notifications/initialized notification.
func (c *Client) Initialize(ctx context.Context) error {
	params := map[string]any{
		"protocolVersion": clientProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "runlore", "version": "dev"},
	}
	if _, err := c.rpc(ctx, "initialize", params); err != nil {
		return fmt.Errorf("mcp initialize: %w", err)
	}
	// Best-effort lifecycle notification (no id, no response).
	if err := c.notify(ctx, "notifications/initialized"); err != nil {
		c.log.Warn("mcp: initialized notification failed (continuing)", "server", c.name, "err", err)
	}
	return nil
}

// ListTools returns the server's advertised tools.
func (c *Client) ListTools(ctx context.Context) ([]RemoteTool, error) {
	raw, err := c.rpc(ctx, "tools/list", map[string]any{})
	if err != nil {
		return nil, err
	}
	var res struct {
		Tools []struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"inputSchema"`
		} `json:"tools"`
		NextCursor string `json:"nextCursor"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return nil, fmt.Errorf("mcp tools/list decode: %w", err)
	}
	if res.NextCursor != "" {
		c.log.Warn("mcp: tools/list returned a nextCursor; pagination unsupported, list may be truncated", "server", c.name)
	}
	out := make([]RemoteTool, 0, len(res.Tools))
	for _, t := range res.Tools {
		out = append(out, RemoteTool{Name: t.Name, Description: t.Description, InputSchema: t.InputSchema})
	}
	return out, nil
}

// CallTool invokes a remote tool and returns its concatenated text content. An MCP
// tool error (isError) or a JSON-RPC error becomes a Go error.
func (c *Client) CallTool(ctx context.Context, name string, args json.RawMessage) (string, error) {
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	raw, err := c.rpc(ctx, "tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", err
	}
	var res struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", fmt.Errorf("mcp tools/call decode: %w", err)
	}
	var b strings.Builder
	for _, p := range res.Content {
		if p.Type == "text" {
			b.WriteString(p.Text)
		}
	}
	if res.IsError {
		return "", fmt.Errorf("mcp tool %q error: %s", name, b.String())
	}
	return b.String(), nil
}

// jsonrpcMessage is one decoded JSON-RPC response (result or error).
type jsonrpcMessage struct {
	ID     json.RawMessage `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

// rpc sends a JSON-RPC request and returns the raw result. It handles a JSON response
// and an SSE (text/event-stream) response, and captures a session id from initialize.
func (c *Client) rpc(ctx context.Context, method string, params any) (json.RawMessage, error) {
	c.nextID++
	id := c.nextID
	body, err := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params})
	if err != nil {
		return nil, err
	}
	resp, err := c.do(ctx, body)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
		return nil, fmt.Errorf("mcp %s status %d (request-id %q)", method, resp.StatusCode, httpx.RequestID(resp.Header))
	}
	if sid := resp.Header.Get("Mcp-Session-Id"); sid != "" {
		c.sessionID = sid
	}
	msg, err := c.readMessage(resp, id)
	if err != nil {
		return nil, err
	}
	if msg.Error != nil {
		return nil, fmt.Errorf("mcp %s rpc error %d: %s", method, msg.Error.Code, msg.Error.Message)
	}
	return msg.Result, nil
}

// readMessage reads either a single JSON body or an SSE stream, returning the JSON-RPC
// message whose id matches want (SSE may interleave other events).
func (c *Client) readMessage(resp *http.Response, want int) (jsonrpcMessage, error) {
	if strings.Contains(resp.Header.Get("Content-Type"), "text/event-stream") {
		for payload, err := range httpx.SSEData(resp.Body) {
			if err != nil {
				return jsonrpcMessage{}, fmt.Errorf("mcp sse read: %w", err)
			}
			var m jsonrpcMessage
			if json.Unmarshal(payload, &m) != nil {
				continue
			}
			var gotID int
			if json.Unmarshal(m.ID, &gotID) == nil && gotID == want {
				return m, nil
			}
		}
		return jsonrpcMessage{}, fmt.Errorf("mcp sse ended without a response for id %d", want)
	}
	var m jsonrpcMessage
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return jsonrpcMessage{}, fmt.Errorf("mcp json decode: %w", err)
	}
	return m, nil
}

// notify sends a JSON-RPC notification (no id, no response read).
func (c *Client) notify(ctx context.Context, method string) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "method": method})
	resp, err := c.do(ctx, body)
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
	_ = resp.Body.Close()
	return nil
}

func (c *Client) do(ctx context.Context, body []byte) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	if c.sessionID != "" {
		req.Header.Set("Mcp-Session-Id", c.sessionID)
	}
	for k, v := range c.headers {
		req.Header.Set(k, v)
	}
	return c.http.Do(req)
}
