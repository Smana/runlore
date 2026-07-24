// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"

	"github.com/Smana/runlore/internal/sourcerepo"
	"github.com/Smana/runlore/internal/whatchanged"
)

// sourceDiffer is the whatchanged capability source_diff needs, narrowed to an
// interface so the tool is testable without real clones.
type sourceDiffer interface {
	Source(ctx context.Context, url, fromRef, toRef string, maxCommits int) (whatchanged.SourceChanges, error)
}

// sourceDiffFile is the per-file record used during rendering: path, raw
// patch, computed +/- counts, and whether the file is generated/vendored.
type sourceDiffFile struct {
	path      string
	patch     string
	add, del  int
	generated bool
}

// Output-shaping caps. In code, not config (see the design spec): sized so a
// default call is comparable to the other summary tools, with paths-zoom as
// the sanctioned way to read more. The loop's MaxToolOutputBytes remains the
// global backstop.
const (
	sourceDiffMaxCommits   = 50
	sourceDiffMaxDiffstat  = 200      // diffstat lines before the tail is folded into "and N more"
	sourceDiffSummaryBytes = 8 << 10  // hunks budget in a summary response
	sourceDiffZoomBytes    = 16 << 10 // hunks budget in a paths-zoom response
)

// sourceDiffMaxDistinctRepos bounds how many DISTINCT repos one investigation
// may clone through source_diff. It caps a prompt-injected loop from walking the
// whole allowlist (each distinct URL is a full-history mirror clone), and matches
// the mirror cache's default capacity so a single investigation can't thrash it.
const sourceDiffMaxDistinctRepos = 10

// SourceDiffTool diffs an application/module source repo between two versions
// the model found in evidence (an image-tag or module-ref bump), closing the
// gap between "the image bumped v1.2.2→v1.2.3" and the commit that explains
// the symptom. Summary-first: commits + full diffstat + the biggest
// non-generated hunks; a second call with paths=[…] zooms into specific
// files. The allowlist match is the security boundary — the model can only
// make RunLore clone repos the operator listed (see internal/sourcerepo).
//
// registered in internal/app/investigate.go when source_repos.allow is set.
type SourceDiffTool struct {
	Source sourceDiffer
	Allow  *sourcerepo.Allowlist
	// cloned tracks the distinct clone URLs used this investigation, to enforce
	// sourceDiffMaxDistinctRepos. It is populated per-investigation by
	// withIncidentNamespace (via the loop's scopeTools); the shared registered
	// instance has nil cloned ⇒ unbounded, but it is never called directly.
	cloned map[string]bool
}

// withIncidentNamespace gives each investigation a fresh SourceDiffTool with its
// own distinct-clone budget. source_diff does not use the incident namespace,
// but implementing incidentScoped is how a tool gets per-investigation state:
// scopeTools calls this once per run, so the returned copy's cloned set counts
// only this investigation's clones.
func (t SourceDiffTool) withIncidentNamespace(string) Tool {
	t.cloned = make(map[string]bool)
	return t
}

// Name returns the tool name.
func (t SourceDiffTool) Name() string { return "source_diff" }

// Description returns the tool description.
func (t SourceDiffTool) Description() string {
	return "Diff an APPLICATION or MODULE source repo between two versions — use when what_changed shows an " +
		"image or module version bump (e.g. v1.2.2→v1.2.3) to read the actual code change behind it: commit " +
		"subjects, a per-file diffstat, and the largest hunks. Call again with paths=[…] to read specific " +
		"files' full hunks. Pick repo from the allowed list, matching the image/module name: " +
		describePatterns(t.Allow.Patterns())
}

// describePatterns renders the allowlist for the tool description, capping the
// list so a large allowlist can't bloat the per-step standing prompt cost. The
// full set is always named in the mismatch error (paid only on a miss).
func describePatterns(patterns []string) string {
	const maxListed = 8
	if len(patterns) <= maxListed {
		return strings.Join(patterns, ", ")
	}
	return strings.Join(patterns[:maxListed], ", ") +
		fmt.Sprintf(", … and %d more (a mismatch error lists all)", len(patterns)-maxListed)
}

// Schema returns the JSON schema for the arguments.
func (t SourceDiffTool) Schema() string {
	return `{"type":"object","properties":{` +
		`"repo":{"type":"string","description":"repository as host/org/name, e.g. github.com/acme/checkout — must match the allowed list"},` +
		`"from":{"type":"string","description":"older version: a git tag or SHA (bare image tags work — a v prefix is tried automatically)"},` +
		`"to":{"type":"string","description":"newer version: a git tag or SHA"},` +
		`"paths":{"type":"array","items":{"type":"string"},"description":"zoom: return full hunks for these exact file paths from a prior call's file list"}},` +
		`"required":["repo","from","to"]}`
}

// Call gates the repo against the allowlist, fetches the range, and renders
// the summary (or a paths zoom). Errors are recovery-oriented: the allowlist
// rejection names the allowed patterns, and a ref miss (RefNotFoundError from
// whatchanged) lists nearby tags — both give the model a correction path.
func (t SourceDiffTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Repo  string   `json:"repo"`
		From  string   `json:"from"`
		To    string   `json:"to"`
		Paths []string `json:"paths"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	cloneURL, ok := t.Allow.Match(in.Repo)
	if !ok {
		return "", fmt.Errorf("repo %q is not in the source_repos allowlist; allowed: %s",
			in.Repo, strings.Join(t.Allow.Patterns(), ", "))
	}
	// Distinct-clone budget (per investigation). Re-diffing an already-cloned repo
	// is free; a NEW repo beyond the budget is refused so the loop can't walk the
	// whole allowlist. nil cloned (shared instance, never called directly) ⇒ off.
	if t.cloned != nil && !t.cloned[cloneURL] {
		if len(t.cloned) >= sourceDiffMaxDistinctRepos {
			return "", fmt.Errorf("source_diff clone budget reached (%d distinct repos this investigation); "+
				"re-diffing an already-fetched repo is fine, but no new repos — you have likely gathered enough source context",
				sourceDiffMaxDistinctRepos)
		}
		t.cloned[cloneURL] = true
	}
	sc, err := t.Source.Source(ctx, cloneURL, in.From, in.To, sourceDiffMaxCommits)
	if err != nil {
		return "", err
	}
	return renderSourceChanges(sc, in.Paths), nil
}

// renderSourceChanges renders the summary-first (or zoomed) tool output:
// header, commit list, full diffstat (generated files annotated, never
// hidden), then hunks — either the largest non-generated files within the
// summary budget, or exactly the zoomed paths within the zoom budget.
func renderSourceChanges(sc whatchanged.SourceChanges, zoom []string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "source diff %s..%s — %d commits", sc.FromRef, sc.ToRef, len(sc.Commits))
	if sc.CommitsCapped {
		fmt.Fprintf(&b, " (list capped at %d)", sourceDiffMaxCommits)
	}
	b.WriteString("\ncommits (newest first):\n")
	for _, c := range sc.Commits {
		fmt.Fprintf(&b, "  %.7s %s %s\n", c.SHA, c.When.UTC().Format("2006-01-02"), c.Subject)
	}
	if sc.FilesOmitted > 0 {
		fmt.Fprintf(&b, "note: diff too large — %d files omitted entirely; narrow the from..to range for full coverage\n", sc.FilesOmitted)
	}
	files := make([]sourceDiffFile, 0, len(sc.Diff.Files))
	for _, f := range sc.Diff.Files {
		// countChanges (whatchanged_tool.go) is the package's one diffstat counter.
		add, del := countChanges(strings.Split(f.Patch, "\n"))
		files = append(files, sourceDiffFile{f.Path, f.Patch, add, del, generatedPath(f.Path)})
	}
	renderDiffstat(&b, files)
	if len(zoom) > 0 {
		renderZoom(&b, files, zoom)
		return b.String()
	}
	renderSummaryHunks(&b, files)
	return b.String()
}

// renderDiffstat writes the per-file change summary, largest churn first, capped
// at sourceDiffMaxDiffstat lines. The diffstat is the ONE output section that
// otherwise scales with repo size (commits are capped, hunks byte-budgeted), so
// a monorepo release of thousands of files would blow the token budget and get
// blind-truncated by the loop's global backstop — taking the zoom index with it.
// The tail is folded into one "and N more files" line with aggregate churn, and
// every file stays reachable via paths zoom. Generated/vendored files carry a
// terse [gen] marker explained once by a legend rather than a full note per line.
func renderDiffstat(b *strings.Builder, files []sourceDiffFile) {
	sort.Slice(files, func(i, j int) bool {
		return files[i].add+files[i].del > files[j].add+files[j].del
	})
	hasGenerated := false
	for _, f := range files {
		if f.generated {
			hasGenerated = true
			break
		}
	}
	b.WriteString("files (largest first):\n")
	if hasGenerated {
		b.WriteString("  ([gen] = generated/vendored — hunks omitted from the summary; read via paths)\n")
	}
	shown := files
	if len(shown) > sourceDiffMaxDiffstat {
		shown = shown[:sourceDiffMaxDiffstat]
	}
	for _, f := range shown {
		marker := ""
		if f.generated {
			marker = " [gen]"
		}
		fmt.Fprintf(b, "  %s +%d -%d%s\n", f.path, f.add, f.del, marker)
	}
	if rest := files[len(shown):]; len(rest) > 0 {
		var add, del int
		for _, f := range rest {
			add += f.add
			del += f.del
		}
		fmt.Fprintf(b, "  … and %d more files (+%d -%d total) — zoom with paths to read any\n", len(rest), add, del)
	}
}

// renderZoom emits full hunks for the requested paths (generated or not — an
// explicit ask overrides the noise filter), noting any path not in this diff.
// When the 16 KiB budget is exhausted before a later requested path, those paths
// are counted and an omission notice is appended — consistent with renderSummaryHunks.
func renderZoom(b *strings.Builder, files []sourceDiffFile, zoom []string) {
	want := make(map[string]bool, len(zoom))
	for _, p := range zoom {
		want[p] = true
	}
	budget := sourceDiffZoomBytes
	omitted := 0
	b.WriteString("hunks (zoom):\n")
	for _, f := range files {
		if !want[f.path] {
			continue
		}
		delete(want, f.path)
		if budget <= 0 {
			omitted++
			continue
		}
		budget -= writeHunk(b, f.path, f.patch, budget)
	}
	if omitted > 0 {
		fmt.Fprintf(b, "[%d more requested files' hunks omitted — zoom fewer paths at once]\n", omitted)
	}
	for p := range want {
		fmt.Fprintf(b, "  %s: not in this diff (check the file list above)\n", p)
	}
}

// renderSummaryHunks emits the largest non-generated files' hunks within the
// summary budget, and tells the model how to read what was left out.
func renderSummaryHunks(b *strings.Builder, files []sourceDiffFile) {
	realFiles := make([]int, 0, len(files))
	for i, f := range files {
		if !f.generated {
			realFiles = append(realFiles, i)
		}
	}
	sort.Slice(realFiles, func(i, j int) bool {
		a, c := files[realFiles[i]], files[realFiles[j]]
		return a.add+a.del > c.add+c.del
	})
	b.WriteString("hunks (largest changes first):\n")
	budget, omitted := sourceDiffSummaryBytes, 0
	for _, i := range realFiles {
		if budget <= 0 {
			omitted++
			continue
		}
		budget -= writeHunk(b, files[i].path, files[i].patch, budget)
	}
	if omitted > 0 {
		fmt.Fprintf(b, "[%d more files' hunks omitted — call again with paths=[…] to zoom]\n", omitted)
	}
}

// writeHunk writes one file's patch within budget, truncating rune-safely
// with a zoom pointer when it does not fit. Returns the bytes written.
func writeHunk(b *strings.Builder, filePath, patch string, budget int) int {
	if budget <= 0 {
		return 0
	}
	before := b.Len()
	fmt.Fprintf(b, "--- %s\n", filePath)
	if len(patch) > budget {
		b.WriteString(patch[:runeAlignedCut(patch, budget)])
		b.WriteString("\n[truncated — use paths to zoom]\n")
	} else {
		b.WriteString(patch)
	}
	return b.Len() - before
}

// generatedPath reports whether a file is generated/vendored content whose
// hunks are token noise (routinely 50-90% of a release diff). Such files stay
// in the diffstat — nothing is hidden — but are excluded from summary hunks.
func generatedPath(p string) bool {
	base := path.Base(p)
	switch base {
	case "go.sum", "package-lock.json", "yarn.lock", "pnpm-lock.yaml", "Cargo.lock",
		"poetry.lock", "uv.lock", "composer.lock", "Gemfile.lock", "flake.lock":
		return true
	}
	if strings.HasSuffix(base, ".pb.go") || strings.HasSuffix(base, ".min.js") || strings.HasSuffix(base, ".min.css") {
		return true
	}
	// Segment-bounded contains: "/vendor/" can't match "adventurous".
	dir := "/" + path.Dir(p) + "/"
	return strings.Contains(dir, "/vendor/") || strings.Contains(dir, "/node_modules/") || strings.Contains(dir, "/dist/")
}
