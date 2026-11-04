// SPDX-License-Identifier: Apache-2.0

package mcp

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
)

// maxHTTPBody caps a request body; a larger one is answered 413.
const maxHTTPBody = 1 << 20

// ServeHTTP serves the MCP server over StreamableHTTP in its simplest form: POST
// only, one JSON reply per request, no SSE and no session id. The protocol header is
// ignored (initialize echoes the client's version). A notification is answered 202
// with no body.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHTTPBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "read body", http.StatusBadRequest)
		return
	}
	var req rpcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeRPC(w, http.StatusBadRequest, rpcResponse{
			JSONRPC: "2.0", ID: json.RawMessage("null"),
			Error: &rpcError{Code: -32700, Message: "parse error"},
		})
		return
	}
	resp, respond := s.dispatch(r.Context(), &req)
	if !respond {
		w.WriteHeader(http.StatusAccepted)
		return
	}
	writeRPC(w, http.StatusOK, resp)
}

func writeRPC(w http.ResponseWriter, code int, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(resp)
}
