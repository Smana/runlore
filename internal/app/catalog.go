// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"log/slog"
	"os"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/embed"
	"github.com/Smana/runlore/internal/kbvalidate"
	"github.com/Smana/runlore/internal/telemetry"
)

// BuildCatalog returns the kb_search backing store, or nil when no catalog is
// configured. With a Git URL it starts a background syncer (running on every
// replica, so a failover standby is already warm) that re-indexes after each pull;
// otherwise it loads a static mounted directory once.
func BuildCatalog(ctx context.Context, cfg *config.Config, forgeTok ForgeToken, metrics *telemetry.Metrics, log *slog.Logger) *catalog.Catalog {
	// warnInvalid surfaces structurally-invalid (but parseable) entries at load
	// time — a backstop for the CI gate. The entry is still indexed and served
	// (one bad entry never empties the catalog); we just log loudly + count it.
	warnInvalid := func(cat *catalog.Catalog) {
		kbvalidate.WarnInvalid(cat.Entries(), func(path string, errs []kbvalidate.Issue) {
			log.Warn("invalid KB entry indexed", "path", path,
				"issues", len(errs), "first", errs[0].Field+": "+errs[0].Message)
			if metrics != nil {
				metrics.CatalogInvalidEntries.Add(ctx, 1)
			}
		})
	}
	// Hybrid recall: build the embeddings client once and attach it BEFORE any Reload
	// so entry vectors are produced. Requires both the feature flag and a configured
	// endpoint; otherwise the catalog stays BM25-only (and recall stays BM25).
	var embedder catalog.Embedder
	if cfg.Catalog.InstantRecall.Hybrid && cfg.Model.Embeddings != nil {
		e := cfg.Model.Embeddings
		key := ""
		if e.APIKeyEnv != "" {
			key = os.Getenv(e.APIKeyEnv)
		}
		embedder = embed.New(e.BaseURL, e.Model, key)
		log.Info("hybrid recall: embeddings endpoint configured", "base_url", e.BaseURL, "model", e.Model)
	}
	if cfg.Catalog.Git.URL != "" {
		dir := cfg.Catalog.Dir
		if dir == "" {
			dir = "/var/lib/runlore/catalog"
		}
		cat := catalog.NewEmpty()
		cat.Log = log
		if embedder != nil {
			cat.SetEmbedder(embedder)
		}
		// Auth precedence: explicit token_env, else the shared forge GitHub App
		// identity (one credential for both curation writes and catalog reads).
		var token catalog.TokenFunc
		if env := cfg.Catalog.Git.TokenEnv; env != "" {
			if t := os.Getenv(env); t != "" {
				token = func(context.Context) (string, error) { return t, nil }
			}
		} else if forgeTok != nil {
			token = catalog.TokenFunc(forgeTok)
			log.Info("catalog git-sync using the forge GitHub App identity")
		}
		syncer := &catalog.Syncer{URL: cfg.Catalog.Git.URL, Branch: cfg.Catalog.Git.Branch, Dir: dir, Token: token, Log: log}
		go syncer.Run(ctx, cfg.Catalog.Git.Interval.Std(), func(_ *catalog.SyncDelta) error {
			skipped, err := cat.Reload(dir)
			if err != nil {
				log.Warn("catalog reload failed", "dir", dir, "err", err)
				return err
			}
			if len(skipped) > 0 {
				log.Warn("catalog entries skipped (unparseable)", "count", len(skipped), "files", skipped)
			}
			log.Info("catalog synced", "url", cfg.Catalog.Git.URL, "entries", cat.Len())
			if embedder != nil && !cat.HasVectors() {
				if metrics != nil {
					metrics.CatalogEmbedDegraded.Add(ctx, 1)
				}
			}
			warnInvalid(cat)
			return nil
		})
		log.Info("catalog git-sync enabled", "url", cfg.Catalog.Git.URL, "dir", dir)
		return cat
	}
	if cfg.Catalog.Dir != "" {
		var cat *catalog.Catalog
		if embedder != nil {
			// Embed on load: NewEmpty + SetEmbedder + ReloadContext (catalog.New can't
			// attach an embedder before its internal Reload).
			cat = catalog.NewEmpty()
			cat.Log = log
			cat.SetEmbedder(embedder)
			if _, err := cat.ReloadContext(ctx, cfg.Catalog.Dir); err != nil {
				log.Warn("catalog disabled", "dir", cfg.Catalog.Dir, "err", err)
				return nil
			}
			if embedder != nil && !cat.HasVectors() {
				if metrics != nil {
					metrics.CatalogEmbedDegraded.Add(ctx, 1)
				}
			}
		} else {
			c, err := catalog.New(cfg.Catalog.Dir)
			if err != nil {
				log.Warn("catalog disabled", "dir", cfg.Catalog.Dir, "err", err)
				return nil
			}
			cat = c
		}
		log.Info("catalog loaded", "dir", cfg.Catalog.Dir, "entries", cat.Len())
		warnInvalid(cat)
		return cat
	}
	return nil
}
