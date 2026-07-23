// SPDX-License-Identifier: Apache-2.0

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
	const usage = "usage: lore kb search <query> [flags] | lore kb show <entry> [flags] | lore kb import <src-dir> [flags]"
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	switch args[0] {
	case "search":
		return runKBSearch(args[1:], os.Stdout)
	case "show":
		return runKBShow(args[1:], os.Stdout)
	case "import":
		return runKBImport(args[1:], os.Stdout)
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

// parseInterleaved parses fs against args while tolerating positionals BEFORE
// flags — stdlib flag stops at the first non-flag token, but a human types
// `lore kb search "crashloop web" --dir ./kb` (query first) as naturally as
// the reverse, and the usage strings promise both. Each round parses what it
// can, peels one positional, and re-parses the rest; returned positionals
// preserve their order.
// A bare `--` is the flag terminator: once seen, everything after it is a
// literal positional argument (never re-parsed as a flag), so a query like
// `-k` can be searched for literally with `lore kb search -- -k`.
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	head := args
	var tail []string
	for i, a := range args {
		if a == "--" {
			head = args[:i]
			tail = args[i+1:]
			break
		}
	}
	var positional []string
	for {
		if err := fs.Parse(head); err != nil {
			return nil, err
		}
		head = fs.Args()
		if len(head) == 0 {
			break
		}
		positional = append(positional, head[0])
		head = head[1:]
	}
	return append(positional, tail...), nil
}

func runKBSearch(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("kb search", flag.ContinueOnError)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	dir := fs.String("dir", "", "catalog directory (overrides config catalog.dir)")
	k := fs.Int("k", 10, "maximum results")
	asJSON := fs.Bool("json", false, "emit results as JSON")
	ledgerPath := fs.String("ledger", "", "outcome ledger JSONL; adds the RESOLVE column")
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	query := strings.TrimSpace(strings.Join(rest, " "))
	if query == "" {
		return fmt.Errorf("usage: lore kb search <query> [--config <path>] [--dir <catalog>] [-k 10] [--json] [--ledger <jsonl>]")
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
	counts := ledgerCounts(*ledgerPath, os.Stderr)
	if *asJSON {
		return writeHitsJSON(w, hits, counts)
	}
	writeHitsTable(w, hits, counts, *ledgerPath != "")
	return nil
}

// ledgerCounts loads per-entry recall/resolve aggregates from an optional
// ledger file. The ledger lives in-cluster, so this is opt-in for humans who
// copied it locally; a missing/unreadable file warns (to warn — kept off the
// results stream so --json stays clean) and omits the column — never fails
// the search, and never CREATES the file (outcome.New would).
func ledgerCounts(path string, warn io.Writer) map[string]outcome.Aggregate {
	if path == "" {
		return nil
	}
	var openErr error
	if _, err := os.Stat(path); err != nil {
		openErr = err
	} else if l, err := outcome.New(path); err != nil {
		openErr = err
	} else {
		counts, _ := l.OpenCounts() // documented always-nil error
		return counts
	}
	_, _ = fmt.Fprintf(warn, "warning: ledger %s unreadable (%v); RESOLVE column omitted\n", path, openErr)
	return nil
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
	rest, err := parseInterleaved(fs, args)
	if err != nil {
		return err
	}
	arg := strings.TrimSpace(strings.Join(rest, " "))
	if arg == "" {
		return fmt.Errorf("usage: lore kb show <entry-path | filename | query> [--config <path>] [--dir <catalog>]")
	}
	cat, err := loadKBCatalog(*cfgPath, *dir)
	if err != nil {
		return err
	}
	e, candidates, ok := findEntry(cat, arg)
	if !ok {
		if len(candidates) > 1 {
			return fmt.Errorf("ambiguous entry %q; candidates:\n%s", arg, renderCandidates(candidates))
		}
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
			entries := make([]catalog.Entry, len(hits))
			for i, h := range hits {
				entries[i] = h.Entry
			}
			return fmt.Errorf("no exact match for %q; candidates:\n%s", arg, renderCandidates(entries))
		}
	}
	writeEntry(w, e)
	return nil
}

// findEntry matches by exact bundle-relative path, then by bare filename (with
// or without the .md suffix). An exact path match wins immediately — paths are
// unique. Basename matches are collected in full: exactly one is an
// unambiguous hit, but two or more (e.g. incidents/foo.md and
// runbooks/foo.md) are ambiguous — the caller renders them as candidates
// instead of the command silently guessing the first one found.
func findEntry(cat *catalog.Catalog, arg string) (e catalog.Entry, candidates []catalog.Entry, ok bool) {
	base := strings.TrimSuffix(arg, ".md")
	for _, e := range cat.Entries() {
		if e.Path == arg {
			return e, nil, true
		}
		if strings.TrimSuffix(filepath.Base(e.Path), ".md") == base {
			candidates = append(candidates, e)
		}
	}
	if len(candidates) == 1 {
		return candidates[0], nil, true
	}
	return catalog.Entry{}, candidates, false
}

// renderCandidates formats a disambiguation list ("  path — title" per
// entry), shared by the search-fallback and basename-collision pickers.
func renderCandidates(entries []catalog.Entry) string {
	var b strings.Builder
	for _, e := range entries {
		_, _ = fmt.Fprintf(&b, "  %s — %s\n", e.Path, e.Title)
	}
	return strings.TrimRight(b.String(), "\n")
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

// kbHit is the machine-readable search result (the CLI counterpart of the
// kb_search MCP tool's hit shape, plus the optional ledger track record).
type kbHit struct {
	Path        string   `json:"path"`
	Type        string   `json:"type,omitempty"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Resource    string   `json:"resource,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	Score       float64  `json:"score"`
	LastSeen    string   `json:"last_seen,omitempty"` // frontmatter timestamp, RFC3339
	Recalls     int      `json:"recalls,omitempty"`
	Resolved    int      `json:"resolved,omitempty"`
}

func writeHitsJSON(w io.Writer, hits []catalog.ScoredEntry, counts map[string]outcome.Aggregate) error {
	out := make([]kbHit, 0, len(hits))
	for _, h := range hits {
		agg := counts[h.Entry.Path]
		out = append(out, kbHit{
			Path: h.Entry.Path, Type: h.Entry.Type, Title: h.Entry.Title,
			Description: h.Entry.Description, Resource: h.Entry.Resource, Tags: h.Entry.Tags,
			Score: h.Score, LastSeen: h.Entry.Timestamp,
			Recalls: agg.Recalls, Resolved: agg.Resolved,
		})
	}
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
