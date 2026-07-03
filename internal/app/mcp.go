package app

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/kbmcp"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/mcp"
	"github.com/Smana/runlore/internal/providers"
)

// RunMCP serves RunLore capabilities over the Model Context Protocol (stdio
// JSON-RPC): the GitOps what-changed tool (needs a Kubernetes client +
// config.gitops) and the knowledge-base tools kb_search/kb_get (need an OKF
// catalog dir). Each capability is optional — an MCP client on a laptop can
// serve just the KB from a local checkout — but an empty server is an error.
// stdout is the protocol channel; logs go to stderr. Read-only. version is
// injected at build time and stays in package main; it is passed through to the
// MCP server's advertised server info.
//
// Usage: lore mcp [--config runlore.yaml] [catalog-dir]
// The positional catalog-dir overrides config catalog.dir; when it is given, a
// missing config file is tolerated (KB-only mode needs no config).
func RunMCP(version string, args []string) error {
	flags := flag.NewFlagSet("mcp", flag.ContinueOnError)
	cfgPath := flags.String("config", "runlore.yaml", "path to config file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	dir := flags.Arg(0)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		// KB-only mode: with an explicit catalog dir, a merely-absent config file
		// is fine (no cluster, no gitops). A present-but-broken config still fails
		// loudly — silently ignoring a config the operator wrote hides mistakes.
		if dir == "" || !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		cfg = nil
	}

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	var gp providers.GitOpsProvider
	if cfg != nil {
		log = logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)
		gp = GitOpsFromKube(cfg, log)
		if dir == "" {
			dir = cfg.Catalog.Dir
		}
	}

	var cat *catalog.Catalog
	if dir != "" {
		c, cerr := catalog.New(dir)
		if cerr != nil {
			return fmt.Errorf("load catalog %s: %w", dir, cerr)
		}
		cat = c
		log.Info("knowledge catalog indexed", "dir", dir, "entries", c.Len())
	}

	tools, err := assembleMCPTools(gp, cat)
	if err != nil {
		return err
	}
	srv := mcp.NewServer("runlore", version, log)
	names := make([]string, 0, len(tools))
	for _, t := range tools {
		srv.AddTool(t)
		names = append(names, t.Name)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Info("runlore mcp server ready (stdio)", "tools", names)
	return srv.Serve(ctx, os.Stdin, os.Stdout)
}

// assembleMCPTools builds the served tool set from whichever capabilities are
// configured. Both absent is a misconfiguration: an MCP server with no tools
// would handshake fine and then be useless.
func assembleMCPTools(gp providers.GitOpsProvider, cat *catalog.Catalog) ([]mcp.Tool, error) {
	var tools []mcp.Tool
	if gp != nil {
		tool := investigate.WhatChangedTool{GitOps: gp}
		tools = append(tools, mcp.Tool{
			Name:        "gitops_what_changed",
			Description: tool.Description(),
			InputSchema: json.RawMessage(tool.Schema()),
			Handler: func(ctx context.Context, args json.RawMessage) (string, error) {
				return tool.Call(ctx, string(args))
			},
		})
	}
	if cat != nil {
		tools = append(tools, kbmcp.Tools(cat)...)
	}
	if len(tools) == 0 {
		return nil, fmt.Errorf("nothing to serve: what_changed needs a Kubernetes client + config.gitops, kb tools need catalog.dir (or `lore mcp <catalog-dir>`)")
	}
	return tools, nil
}
