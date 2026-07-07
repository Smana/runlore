package app

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/outcome"
)

// RunKB dispatches the human-facing knowledge-base read commands. `lore catalog
// sync` remains the machine/ops write surface; `lore kb` is how a person asks
// "what do we already know about this?" without an MCP client or a cluster.
func RunKB(args []string) error {
	const usage = "usage: lore kb search <query> [flags] | lore kb show <entry> [flags]"
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "search":
		return runKBSearch(args[1:], os.Stdout)
	case "show":
		return runKBShow(args[1:], os.Stdout)
	}
	return fmt.Errorf("unknown kb subcommand %q\n%s", args[0], usage)
}

// loadKBCatalog opens the catalog for the read commands: an explicit --dir
// wins; otherwise config catalog.dir. The CLI never clones — a git-synced
// catalog is materialized by `lore catalog sync` (or a running agent), so the
// error message points there instead of failing cryptically.
func loadKBCatalog(cfgPath, dir string) (*catalog.Catalog, error) {
	if dir == "" {
		cfg, err := config.Load(cfgPath)
		if err != nil {
			return nil, fmt.Errorf("load config: %w (or pass --dir <catalog>)", err)
		}
		dir = cfg.Catalog.Dir
		if dir == "" {
			return nil, fmt.Errorf("no catalog configured (set catalog.dir or pass --dir <catalog>)")
		}
	}
	cat, err := catalog.New(dir)
	if err != nil {
		return nil, fmt.Errorf("load catalog %s: %w (for a git-synced catalog, run `lore catalog sync` first)", dir, err)
	}
	if cat.Len() == 0 {
		return nil, fmt.Errorf("catalog %s has no entries (for a git-synced catalog, run `lore catalog sync` first)", dir)
	}
	return cat, nil
}

func runKBSearch(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("kb search", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	dir := fs.String("dir", "", "catalog directory (overrides config catalog.dir)")
	k := fs.Int("k", 10, "maximum results")
	asJSON := fs.Bool("json", false, "emit results as JSON")
	ledgerPath := fs.String("ledger", "", "outcome ledger JSONL; adds the RESOLVE column")
	if err := fs.Parse(args); err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if query == "" {
		return fmt.Errorf("usage: lore kb search <query> [--dir <catalog>] [-k 10] [--json] [--ledger <jsonl>]")
	}
	cat, err := loadKBCatalog(*cfgPath, *dir)
	if err != nil {
		return err
	}
	hits, err := cat.SearchScored(query, *k)
	if err != nil {
		return err
	}
	if len(hits) == 0 {
		return fmt.Errorf("no entries match %q", query)
	}
	counts := ledgerCounts(*ledgerPath, w)
	if *asJSON {
		return writeHitsJSON(w, hits, counts)
	}
	writeHitsTable(w, hits, counts, *ledgerPath != "")
	return nil
}

// ledgerCounts loads per-entry recall/resolve aggregates from an optional
// ledger file. The ledger lives in-cluster, so this is opt-in for humans who
// copied it locally; a missing/unreadable file warns and omits the column —
// never fails the search, and never CREATES the file (outcome.New would).
func ledgerCounts(path string, w io.Writer) map[string]outcome.Aggregate {
	if path == "" {
		return nil
	}
	if _, err := os.Stat(path); err != nil {
		_, _ = fmt.Fprintf(w, "warning: ledger %s unreadable (%v); RESOLVE column omitted\n", path, err)
		return nil
	}
	l, err := outcome.New(path)
	if err != nil {
		_, _ = fmt.Fprintf(w, "warning: ledger %s unreadable (%v); RESOLVE column omitted\n", path, err)
		return nil
	}
	counts, _ := l.OpenCounts() // documented always-nil error
	return counts
}

func writeHitsTable(w io.Writer, hits []catalog.ScoredEntry, counts map[string]outcome.Aggregate, withResolve bool) {
	tw := tabwriter.NewWriter(w, 2, 4, 2, ' ', 0)
	head := "SCORE\tENTRY\tTITLE\tRESOURCE\tLAST SEEN"
	if withResolve {
		head += "\tRESOLVE"
	}
	_, _ = fmt.Fprintln(tw, head)
	for _, h := range hits {
		row := fmt.Sprintf("%.2f\t%s\t%s\t%s\t%s",
			h.Score, h.Entry.Path, truncateCell(h.Entry.Title, 60), h.Entry.Resource, relAge(h.Entry.Timestamp))
		if withResolve {
			// Resolve-rate is "resolved/recalled" per catalog entry; "-" for an
			// entry the ledger has never seen recalled.
			if agg := counts[h.Entry.Path]; agg.Recalls > 0 {
				row += fmt.Sprintf("\t%d/%d", agg.Resolved, agg.Recalls)
			} else {
				row += "\t-"
			}
		}
		_, _ = fmt.Fprintln(tw, row)
	}
	_ = tw.Flush()
}

// truncateCell keeps table rows scannable: cap a free-text cell at maxLen runes.
func truncateCell(s string, maxLen int) string {
	r := []rune(s)
	if len(r) <= maxLen {
		return s
	}
	return strings.TrimRight(string(r[:maxLen]), " ") + "…"
}

// relAge renders an entry's RFC3339 timestamp as a coarse relative age ("12d
// ago") — what a human scans for ("is this knowledge fresh?"). "" for absent,
// malformed, or future timestamps (hand-written entries carry none).
func relAge(ts string) string {
	t, err := time.Parse(time.RFC3339, ts)
	if err != nil {
		return ""
	}
	d := time.Since(t)
	switch {
	case d < 0:
		return ""
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours()/24))
	}
}

// runKBShow prints one entry in full: the frontmatter card, then the body. The
// argument is a bundle-relative path or a bare filename; when neither matches
// exactly, a search fallback accepts a UNIQUE hit and otherwise lists the
// candidates instead of guessing — showing the wrong runbook is worse than
// asking the human to pick.
func runKBShow(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("kb show", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	dir := fs.String("dir", "", "catalog directory (overrides config catalog.dir)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	arg := strings.TrimSpace(strings.Join(fs.Args(), " "))
	if arg == "" {
		return fmt.Errorf("usage: lore kb show <entry-path | filename | query> [--dir <catalog>]")
	}
	cat, err := loadKBCatalog(*cfgPath, *dir)
	if err != nil {
		return err
	}
	e, ok := findEntry(cat, arg)
	if !ok {
		hits, serr := cat.SearchScored(arg, 5)
		if serr != nil {
			return serr
		}
		switch len(hits) {
		case 0:
			return fmt.Errorf("no entry matches %q", arg)
		case 1:
			e = hits[0].Entry
		default:
			var b strings.Builder
			for _, h := range hits {
				_, _ = fmt.Fprintf(&b, "  %s — %s\n", h.Entry.Path, h.Entry.Title)
			}
			return fmt.Errorf("no exact match for %q; candidates:\n%s", arg, strings.TrimRight(b.String(), "\n"))
		}
	}
	writeEntry(w, e)
	return nil
}

// findEntry matches by exact bundle-relative path, then by bare filename (with
// or without the .md suffix).
func findEntry(cat *catalog.Catalog, arg string) (catalog.Entry, bool) {
	base := strings.TrimSuffix(arg, ".md")
	for _, e := range cat.Entries() {
		if e.Path == arg || strings.TrimSuffix(filepath.Base(e.Path), ".md") == base {
			return e, true
		}
	}
	return catalog.Entry{}, false
}

// writeEntry prints the frontmatter card then the markdown body — the same
// information a reviewer sees on the file, without leaving the terminal.
func writeEntry(w io.Writer, e catalog.Entry) {
	_, _ = fmt.Fprintf(w, "# %s\n\n", e.Title)
	card := [][2]string{
		{"path", e.Path}, {"type", e.Type}, {"description", e.Description},
		{"resource", e.Resource}, {"tags", strings.Join(e.Tags, ", ")},
		{"last seen", relAge(e.Timestamp)}, {"fingerprint", shortFP(e.Fingerprint)},
	}
	for _, kv := range card {
		if kv[1] != "" {
			_, _ = fmt.Fprintf(w, "%s: %s\n", kv[0], kv[1])
		}
	}
	_, _ = fmt.Fprintf(w, "\n%s\n", strings.TrimSpace(e.Body))
}

// shortFP abbreviates the 64-hex dup fingerprint for display; identity checks
// belong to machines, humans only need "has one / which one roughly".
func shortFP(fp string) string {
	if len(fp) > 12 {
		return fp[:12] + "…"
	}
	return fp
}

// writeHitsJSON is implemented in Task 4 of the plan.
func writeHitsJSON(w io.Writer, hits []catalog.ScoredEntry, counts map[string]outcome.Aggregate) error { //nolint:revive // hits/counts kept: Task 4 replaces this stub with the real signature
	_ = json.NewEncoder(w)
	return fmt.Errorf("kb search --json: not implemented yet")
}
