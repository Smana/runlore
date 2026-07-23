// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/kbimport"
	"github.com/Smana/runlore/internal/kbvalidate"
	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

// runKBImport is `lore kb import <src-dir>`: the cold-start seeder. It
// converts existing markdown runbooks/postmortems into OKF entries in a local
// KB checkout — inferred frontmatter, merge-gate validation, dedup against
// the live catalog — and writes files for the HUMAN to review and commit.
// Nothing is committed or pushed; the review stays in Git, like every KB PR.
func runKBImport(args []string, w io.Writer) error {
	fset := flag.NewFlagSet("kb import", flag.ContinueOnError)
	cfgPath := fset.String("config", "runlore.yaml", "path to config file")
	into := fset.String("into", "", "local KB checkout to write into (default: config catalog.dir)")
	dryRun := fset.Bool("dry-run", false, "print the import plan; write nothing")
	useModel := fset.Bool("model", false, "refine frontmatter with the configured model (optional; import is deterministic without it)")
	rest, err := parseInterleaved(fset, args)
	if err != nil {
		return err
	}
	const usage = "usage: lore kb import <src-dir> [--into <kb-dir>] [--dry-run] [--model] [--config <path>]"
	if len(rest) != 1 {
		return fmt.Errorf("%s", usage)
	}
	src := rest[0]

	dest := *into
	if dest == "" {
		cfg, cerr := config.Load(*cfgPath)
		if cerr != nil {
			return fmt.Errorf("load config: %w (or pass --into <kb-dir>)", cerr)
		}
		dest = cfg.Catalog.Dir
		if dest == "" {
			return fmt.Errorf("no destination (set catalog.dir or pass --into <kb-dir>)")
		}
	}
	if st, serr := os.Stat(dest); serr != nil || !st.IsDir() {
		return fmt.Errorf("KB dir %s is not a directory (clone your KB repo checkout first): %v", dest, serr)
	}

	// Optional model — same opt-in shape as `lore validate-kb --semantic`:
	// no usable model config degrades to deterministic-only with a warning.
	var model providers.ModelProvider
	if *useModel {
		if cfg, cerr := config.Load(*cfgPath); cerr == nil && ModelConfigured(cfg) {
			model = BuildModel(cfg, os.Getenv(cfg.Model.APIKeyEnv))
		} else {
			fmt.Fprintln(os.Stderr, "kb import: --model set but no usable model in config; running deterministic only")
		}
	}

	// Existing catalog, loaded tolerantly: an EMPTY checkout is fine (that is
	// the cold start this command exists for), and one malformed existing
	// entry must not block seeding (catalog.Load already skip-warns).
	existing, skippedLoad, err := catalog.Load(dest)
	if err != nil {
		return fmt.Errorf("load existing catalog %s: %w", dest, err)
	}
	for _, s := range skippedLoad {
		fmt.Fprintf(os.Stderr, "warning: existing entry skipped during dedup scan: %s\n", s)
	}

	sources, err := collectSources(src)
	if err != nil {
		return err
	}
	if len(sources) == 0 {
		return fmt.Errorf("no importable .md files under %s", src)
	}

	results := make([]kbimport.Result, 0, len(sources))
	for _, p := range sources {
		data, rerr := os.ReadFile(p) //nolint:gosec // G304: user-passed import directory
		if rerr != nil {
			return rerr
		}
		rel, _ := filepath.Rel(src, p)
		r := kbimport.Infer(data, rel)
		if model != nil {
			r = kbimport.Enrich(context.Background(), r, model)
		}
		results = append(results, r)
	}

	actions := kbimport.Plan(results, existing)

	// Merge-gate validation: never write an entry `lore validate-kb` would
	// reject. Errors demote the action to a skip; warnings are reported only.
	for i := range actions {
		if actions[i].Skip {
			continue
		}
		issues := kbvalidate.ValidateStructural(toCatalogEntry(actions[i].Result))
		for _, iss := range issues {
			if iss.Severity == kbvalidate.SeverityWarning {
				actions[i].Warnings = append(actions[i].Warnings, fmt.Sprintf("%s: %s", iss.Field, iss.Message))
			}
		}
		if kbvalidate.HasErrors(issues) {
			first := firstError(issues)
			actions[i].Skip = true
			actions[i].Reason = fmt.Sprintf("fails validation: %s: %s", first.Field, first.Message)
		}
	}

	imported, skipped := 0, 0
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "ACTION\tSOURCE\tDEST\tTYPE\tTITLE / REASON")
	for _, a := range actions {
		for _, warn := range a.Warnings {
			fmt.Fprintf(os.Stderr, "warning: %s: %s\n", a.Source, warn)
		}
		if a.Skip {
			skipped++
			fmt.Fprintf(tw, "skip\t%s\t-\t-\t%s\n", a.Source, a.Reason)
			continue
		}
		imported++
		fmt.Fprintf(tw, "import\t%s\t%s\t%s\t%s\n", a.Source, a.DestPath, a.Entry.Type, truncateCell(a.Entry.Title, 60))
		if *dryRun {
			continue
		}
		out := filepath.Join(dest, filepath.FromSlash(a.DestPath))
		if merr := os.MkdirAll(filepath.Dir(out), 0o755); merr != nil {
			return merr
		}
		if werr := os.WriteFile(out, []byte(okf.Render(a.Entry, a.Meta)), 0o644); werr != nil { //nolint:gosec // G306: catalog files are world-readable docs
			return werr
		}
	}
	_ = tw.Flush()
	if *dryRun {
		fmt.Fprintf(w, "\ndry-run: would import %d, skip %d (of %d sources); nothing written\n", imported, skipped, len(actions))
		return nil
	}
	fmt.Fprintf(w, "\nimported %d, skipped %d (of %d sources) into %s — review the diff, then commit and push\n",
		imported, skipped, len(actions), dest)
	return nil
}

// collectSources walks src for importable markdown, mirroring catalog.Load's
// skip rules (internal/catalog/load.go): hidden files/dirs, non-.md, and the
// reserved index.md / log.md / README.md are not knowledge entries.
func collectSources(src string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(src, func(path string, d fs.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		base := d.Name()
		if d.IsDir() {
			if path != src && strings.HasPrefix(base, ".") {
				return filepath.SkipDir
			}
			return nil
		}
		if strings.HasPrefix(base, ".") || !strings.HasSuffix(base, ".md") {
			return nil
		}
		if base == "index.md" || base == "log.md" || strings.EqualFold(base, "readme.md") {
			return nil
		}
		out = append(out, path)
		return nil
	})
	return out, err
}

// toCatalogEntry adapts an import result to the validator's input type —
// the SAME struct the loader produces, so "valid at import" and "valid at
// merge" are one predicate.
func toCatalogEntry(r kbimport.Result) catalog.Entry {
	return catalog.Entry{
		Type: r.Entry.Type, Title: r.Entry.Title, Description: r.Entry.Description,
		Resource: r.Entry.Resource, Tags: r.Entry.Tags, Body: r.Entry.Body,
		Timestamp: r.Meta.Timestamp, Status: r.Meta.Status, LastValidated: r.Meta.LastValidated,
		Path: r.DestPath,
	}
}

func firstError(issues []kbvalidate.Issue) kbvalidate.Issue {
	for _, iss := range issues {
		if iss.Severity == kbvalidate.SeverityError {
			return iss
		}
	}
	return kbvalidate.Issue{Field: "unknown", Message: "validation error"}
}
