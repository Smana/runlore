package app

import (
	"net/http"
	"testing"
)

// TestNewHTTPServer asserts the serving http.Server is built with every inbound
// timeout/size bound set (non-zero) — Go's defaults are zero (unlimited), the
// Slowloris/DoS gap R9(a) closes.
func TestNewHTTPServer(t *testing.T) {
	s := NewHTTPServer(":0", http.NewServeMux())
	if s.ReadHeaderTimeout == 0 {
		t.Error("ReadHeaderTimeout is zero (unbounded slow-header read)")
	}
	if s.ReadTimeout == 0 {
		t.Error("ReadTimeout is zero (unbounded slow-body read)")
	}
	if s.WriteTimeout == 0 {
		t.Error("WriteTimeout is zero (unbounded slow write)")
	}
	if s.IdleTimeout == 0 {
		t.Error("IdleTimeout is zero (unbounded idle keep-alive)")
	}
	if s.MaxHeaderBytes == 0 {
		t.Error("MaxHeaderBytes is zero (defaults to 1MB but should be explicit)")
	}
}
