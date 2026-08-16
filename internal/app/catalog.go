// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"log/slog"
	"os"
	"path/filepath"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/embed"
	"github.com/Smana/runlore/internal/kbvalidate"
	"github.com/Smana/runlore/internal/telemetry"
)

// armCommons points the catalog at the shared, read-only commons root and keeps it
// synced, when catalog.commons is configured. No-op otherwise, so a deployment
// without it behaves exactly as before.
//
// It is deliberately a single function called from BOTH catalog-construction paths.
// Two hand-wired copies is how the investigator and reinvestigator ended up reading
// different indexes (#414); one function makes divergence impossible.
//
// It cannot use catalog.NewWithCommons — the catalog it arms is already built and
// already carries an embedder, a vector cache and a syncer, and reconstructing it here
// would drop all three. So the ordering that constructor encodes is honoured by hand:
// SetCommonsDir runs before the reload the syncer triggers, because Entry.Commons is
// stamped during that reload's commons pass and a root set afterwards would index the
// shared corpus UNMARKED — and an unmarked commons entry is free to fire instant
// recall.
//
// The commons syncer is separate from the operator's own: its own directory, its own
// (much longer) interval, and a failure that logs rather than propagates. A shared
// upstream corpus being briefly unavailable must never degrade the operator's own
// catalog, which is the one an incident actually depends on.
func armCommons(ctx context.Context, cfg *config.Config, cat *catalog.Catalog, primaryDir string, log *slog.Logger) {
	cc := cfg.Catalog.Commons
	if cc.URL == "" || cc.Dir == "" {
		return
	}
	cat.SetCommonsDir(cc.Dir)

	var token catalog.TokenFunc
	if cc.TokenEnv != "" {
		if t := os.Getenv(cc.TokenEnv); t != "" {
			token = func(context.Context) (string, error) { return t, nil }
		}
	}
	syncer := &catalog.Syncer{URL: cc.URL, Branch: cc.Branch, Dir: cc.Dir, Token: token, Log: log}
	go syncer.Run(ctx, cc.Interval.Std(), func(*catalog.SyncDelta) error {
		// Full reload, not a delta: the delta describes the COMMONS checkout, while
		// ReloadDelta mutates the index by the operator's own paths. Reloading the
		// primary root re-reads the commons alongside it, which is both correct and
		// cheap at this interval.
		if _, err := cat.ReloadContext(ctx, primaryDir); err != nil {
			log.Warn("commons catalog reload failed; keeping the previous index", "dir", cc.Dir, "err", err)
			return err
		}
		log.Info("commons catalog synced", "url", cc.URL, "entries", cat.Len())
		return nil
	})
	log.Info("commons catalog enabled (read-only, never curated into)",
		"url", cc.URL, "dir", cc.Dir, "interval", cc.Interval.Std(),
		"note", "grounds kb_search; does NOT fire instant recall — commons entries are resource-less by design")
}

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
		ec := embed.New(e.BaseURL, e.Model, key)
		// The embeddings endpoint bills like any other model call — once per
		// hybrid-recall query, and in bulk on every catalog reload — so give it the
		// shared metrics instance. It reports under provider="embed" on the same
		// instruments as the main/verify/rerank tiers. That makes the spend visible,
		// not bounded; see the inventory in docs/configuration/configuration.md.
		ec.Metrics = metrics
		embedder = ec
		log.Info("hybrid recall: embeddings endpoint configured", "base_url", e.BaseURL, "model", e.Model)
	}
	// armVecCache persists the embedding cache across restarts (default on with
	// hybrid). Only called where embedder != nil, so cfg.Model.Embeddings is set.
	armVecCache := func(cat *catalog.Catalog) {
		vc := cfg.Catalog.InstantRecall.VectorCache
		if !vc.IsEnabled() {
			return
		}
		vdir := vc.Dir
		if vdir == "" {
			vdir = filepath.Join(os.TempDir(), "runlore-veccache")
		}
		cat.EnableVectorCache(filepath.Join(vdir, "vectors.gob"), cfg.Model.Embeddings.Model)
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
			armVecCache(cat)
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
		go syncer.Run(ctx, cfg.Catalog.Git.Interval.Std(), func(delta *catalog.SyncDelta) error {
			skipped, err := cat.ReloadDelta(ctx, dir, delta)
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
		armCommons(ctx, cfg, cat, dir, log)
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
			armVecCache(cat)
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
		// Arm the commons BEFORE logging the count: armCommons triggers a reload that
		// pulls the shared root in, so logging first would report a number that is
		// already stale.
		armCommons(ctx, cfg, cat, cfg.Catalog.Dir, log)
		log.Info("catalog loaded", "dir", cfg.Catalog.Dir, "entries", cat.Len())
		warnInvalid(cat)
		return cat
	}
	return nil
}
