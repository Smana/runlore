package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/config"
)

// TestBuildModelAndToolsSmoke is a wiring smoke test for the shared
// model+tools+recall assembly used by serve/investigate. With a representative
// minimal config — a configured model, no gitops, no catalog, no
// metrics/logs/network/cloud — it must build a non-nil model and a (possibly
// empty) tool slice without panicking, and not enable instant recall absent a
// catalog. KUBECONFIG is pointed at a nonexistent file so the in-cluster/kube
// probe fails fast and deterministically (the cluster-backed tools are simply
// omitted) rather than depending on the host's ambient kube context.
func TestBuildModelAndToolsSmoke(t *testing.T) {
	t.Setenv("KUBECONFIG", filepath.Join(t.TempDir(), "nonexistent-kubeconfig"))

	for _, provider := range []string{"openai", "anthropic", "gemini"} {
		t.Run(provider, func(t *testing.T) {
			cfg := &config.Config{Model: config.Model{Provider: provider, BaseURL: "http://vllm:8000/v1", Model: "test-model"}}
			log := slog.New(slog.NewTextHandler(io.Discard, nil))

			model, tools, recall, cat := buildModelAndTools(context.Background(), cfg, nil, nil, log)
			if model == nil {
				t.Fatal("buildModelAndTools returned a nil model")
			}
			// A nil/empty tool slice is acceptable (nothing wired here); each present
			// tool must just be usable.
			for i, tl := range tools {
				if tl == nil {
					t.Fatalf("tool %d is nil", i)
				}
				if tl.Name() == "" {
					t.Fatalf("tool %d has an empty name", i)
				}
			}
			if recall != nil {
				t.Fatal("instant recall must be off without a catalog")
			}
			if cat != nil {
				t.Fatal("no catalog configured, want nil catalog")
			}
		})
	}
}

// TestNewHTTPServer asserts the serving http.Server is built with every inbound
// timeout/size bound set (non-zero) — Go's defaults are zero (unlimited), the
// Slowloris/DoS gap R9(a) closes.
func TestNewHTTPServer(t *testing.T) {
	s := newHTTPServer(":0", http.NewServeMux())
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
