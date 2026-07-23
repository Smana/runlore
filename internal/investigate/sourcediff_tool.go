// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"path"
	"sort"
	"strings"
	"unicode/utf8"

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
	sourceDiffSummaryBytes = 8 << 10  // hunks budget in a summary response
	sourceDiffZoomBytes    = 16 << 10 // hunks budget in a paths-zoom response
)

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
}

// Name returns the tool name.
func (t SourceDiffTool) Name() string { return "source_diff" }

// Description returns the tool description.
func (t SourceDiffTool) Description() string {
	return "Diff an APPLICATION or MODULE source repo between two versions — use when what_changed shows an " +
		"image or module version bump (e.g. v1.2.2→v1.2.3) to read the actual code change behind it: commit " +
		"subjects, a per-file diffstat, and the largest hunks. Call again with paths=[…] to read specific " +
		"files' full hunks. Pick repo from the allowed list, matching the image/module name: " +
		strings.Join(t.Allow.Patterns(), ", ")
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
	files := make([]sourceDiffFile, 0, len(sc.Diff.Files))
	for _, f := range sc.Diff.Files {
		add, del := patchCounts(f.Patch)
		files = append(files, sourceDiffFile{f.Path, f.Patch, add, del, generatedPath(f.Path)})
	}
	b.WriteString("files:\n")
	for _, f := range files {
		note := ""
		if f.generated {
			note = "  (generated — hunks skipped; zoom with paths to read)"
		}
		fmt.Fprintf(&b, "  %s +%d -%d%s\n", f.path, f.add, f.del, note)
	}
	if len(zoom) > 0 {
		renderZoom(&b, files, zoom)
		return b.String()
	}
	renderSummaryHunks(&b, files)
	return b.String()
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
	real := make([]int, 0, len(files))
	for i, f := range files {
		if !f.generated {
			real = append(real, i)
		}
	}
	sort.Slice(real, func(i, j int) bool {
		a, c := files[real[i]], files[real[j]]
		return a.add+a.del > c.add+c.del
	})
	b.WriteString("hunks (largest changes first):\n")
	budget, omitted := sourceDiffSummaryBytes, 0
	for _, i := range real {
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
		b.WriteString(runeSafeCut(patch, budget))
		b.WriteString("\n[truncated — use paths to zoom]\n")
	} else {
		b.WriteString(patch)
	}
	return b.Len() - before
}

// runeSafeCut returns the longest prefix of s that is at most maxBytes bytes
// and ends on a valid UTF-8 rune boundary (backing off continuation bytes so
// a multi-byte rune is never split). It follows the same idiom used by
// trimRow in timeline_tool.go and truncateOutput in truncate.go.
func runeSafeCut(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut]
}

// patchCounts counts added/removed lines in a unified patch (excluding the
// +++/--- headers) — a cheap diffstat without another go-git pass.
func patchCounts(patch string) (add, del int) {
	for _, line := range strings.Split(patch, "\n") {
		switch {
		case strings.HasPrefix(line, "+++"), strings.HasPrefix(line, "---"):
		case strings.HasPrefix(line, "+"):
			add++
		case strings.HasPrefix(line, "-"):
			del++
		}
	}
	return add, del
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
	for _, seg := range strings.Split(path.Dir(p), "/") {
		if seg == "vendor" || seg == "node_modules" || seg == "dist" {
			return true
		}
	}
	return false
}
