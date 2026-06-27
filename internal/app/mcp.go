package app

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/mcp"
)

// RunMCP serves RunLore's GitOps what-changed capability over the Model Context
// Protocol (stdio JSON-RPC), so an MCP client (e.g. HolmesGPT) can call it as a
// toolset. stdout is the protocol channel; logs go to stderr. Read-only. version
// is injected at build time and stays in package main; it is passed through to the
// MCP server's advertised server info.
func RunMCP(version string, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)
	gp := GitOpsFromKube(cfg, log)
	if gp == nil {
		return fmt.Errorf("what_changed needs a Kubernetes client + config.gitops")
	}
	tool := investigate.WhatChangedTool{GitOps: gp}
	srv := mcp.NewServer("runlore-whatchanged", version, log)
	srv.AddTool(mcp.Tool{
		Name:        "gitops_what_changed",
		Description: tool.Description(),
		InputSchema: json.RawMessage(tool.Schema()),
		Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
			return tool.Call(ctx, string(args))
		},
	})
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("runlore mcp server ready (stdio)", "tool", "gitops_what_changed")
	return srv.Serve(ctx, os.Stdin, os.Stdout)
}
