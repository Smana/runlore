// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/logging"
)

// RunCatalog dispatches catalog subcommands.
func RunCatalog(args []string) error {
	if len(args) == 0 || args[0] != "sync" {
		return fmt.Errorf("usage: lore catalog sync [--config <path>]")
	}
	return RunCatalogSync(args[1:])
}

// RunCatalogSync clones/pulls the catalog Git repo into the mirror and reports the
// indexed entry count — a one-shot of the background sync that serve runs.
func RunCatalogSync(args []string) error {
	fs := flag.NewFlagSet("catalog sync", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		return err
	}
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)

	g := cfg.Catalog.Git
	if g.URL == "" {
		if cfg.Catalog.Dir == "" {
			return fmt.Errorf("no catalog configured (set catalog.git.url or catalog.dir)")
		}
		cat, err := catalog.New(cfg.Catalog.Dir)
		if err != nil {
			return err
		}
		fmt.Printf("catalog: %d entries at %s (no git-sync configured)\n", cat.Len(), cfg.Catalog.Dir)
		return nil
	}
	dir := cfg.Catalog.Dir
	if dir == "" {
		dir = "/var/lib/runlore/catalog"
	}
	var token catalog.TokenFunc
	if env := g.TokenEnv; env != "" {
		if t := os.Getenv(env); t != "" {
			token = func(context.Context) (string, error) { return t, nil }
		}
	} else if ft := BuildForgeTokenSource(cfg, log); ft != nil {
		token = catalog.TokenFunc(ft)
	}
	syncer := &catalog.Syncer{URL: g.URL, Branch: g.Branch, Dir: dir, Token: token, Log: log}
	if _, err := syncer.Sync(context.Background()); err != nil {
		return fmt.Errorf("sync: %w", err)
	}
	cat, err := catalog.New(dir)
	if err != nil {
		return err
	}
	fmt.Printf("synced %s -> %s (%d entries)\n", g.URL, dir, cat.Len())
	return nil
}
