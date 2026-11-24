// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"fmt"
	"strings"
	"sync"

	"github.com/Smana/runlore/internal/provenance"
	"github.com/Smana/runlore/internal/providers"
)

// maxProvenanceFiles caps how many changed-file permalinks are attached to a
// single hypothesis. Keeps detail views and KB PRs scannable.
const maxProvenanceFiles = 5

type observedFile struct {
	Path      string
	FirstLine int
}

type observedChange struct {
	RepoURL string
	FromRev string
	ToRev   string
	Commits []string // optional individual commit SHAs observed in source_diff
	Files   []observedFile
}

// observedChanges records GitOps and source-repo changes actually materialized
// server-side by what_changed and source_diff during a single investigation.
// Mirrors observedResources: context-scoped, concurrency-safe, and strictly
// server-populated so model-hallucinated or prompt-injected SHAs and URLs can
// never become clickable links.
type observedChanges struct {
	mu      sync.Mutex
	changes []observedChange
}

type observedChangesCtxKey struct{}

// WithObservedChanges derives a context carrying a per-investigation recorder
// of server-observed Git changes and diffs.
func WithObservedChanges(ctx context.Context) context.Context {
	return context.WithValue(ctx, observedChangesCtxKey{}, &observedChanges{})
}

func observedChangesFrom(ctx context.Context) *observedChanges {
	o, _ := ctx.Value(observedChangesCtxKey{}).(*observedChanges)
	return o
}

func (o *observedChanges) add(ch observedChange) {
	if ch.RepoURL == "" || (ch.ToRev == "" && len(ch.Commits) == 0) {
		return
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	o.changes = append(o.changes, ch)
}

func (o *observedChanges) snapshot() []observedChange {
	if o == nil {
		return nil
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	cp := make([]observedChange, len(o.changes))
	copy(cp, o.changes)
	return cp
}

// recordObservedChange records a server-observed Git change and its diff into
// the investigation's context-scoped collector. No-op when ctx carries none.
func recordObservedChange(ctx context.Context, repoURL, fromRev, toRev string, diff providers.Diff, commitSHAs ...string) {
	o := observedChangesFrom(ctx)
	if o == nil {
		return
	}
	files := make([]observedFile, 0, len(diff.Files))
	for _, f := range diff.Files {
		if f.Path == "" {
			continue
		}
		files = append(files, observedFile{
			Path:      f.Path,
			FirstLine: provenance.FirstAddedLine(f.Patch),
		})
	}
	o.add(observedChange{
		RepoURL: repoURL,
		FromRev: fromRev,
		ToRev:   toRev,
		Commits: commitSHAs,
		Files:   files,
	})
}

// attachProvenance matches each hypothesis's ChangeRef against server-observed
// Git changes and stamps verified HTTPS ChangeURL + up to 5 changed-file
// Provenance links. Unobserved SHAs and unknown hosts fail closed (left empty).
func attachProvenance(inv *providers.Investigation, obs *observedChanges, styles provenance.HostStyles) {
	if inv == nil || obs == nil {
		return
	}
	changes := obs.snapshot()
	if len(changes) == 0 {
		return
	}
	for i := range inv.RootCauses {
		rc := &inv.RootCauses[i]
		ref := strings.TrimSpace(rc.ChangeRef)
		if ref == "" {
			continue
		}
		matched, commitSHA, ok := matchObservedChange(ref, changes)
		if !ok {
			continue
		}
		base, style, ok := provenance.NormalizeRepoHTTPS(matched.RepoURL, styles)
		if !ok {
			continue
		}
		var changeURL string
		switch {
		case commitSHA != "":
			changeURL = provenance.CommitURL(base, style, commitSHA)
		case matched.FromRev != "" && matched.ToRev != "" && matched.FromRev != matched.ToRev:
			changeURL = provenance.CompareURL(base, style, matched.FromRev, matched.ToRev)
		case matched.ToRev != "":
			changeURL = provenance.CommitURL(base, style, matched.ToRev)
		}
		if changeURL != "" {
			rc.ChangeURL = changeURL
		}
		fileRev := matched.ToRev
		if fileRev == "" {
			fileRev = commitSHA
		}
		if fileRev != "" && len(matched.Files) > 0 {
			rc.Provenance = buildFileProvenance(base, style, fileRev, matched.Files, rc.ChangeRef, rc.Evidence)
		}
	}
}

// matchObservedChange finds the first observed change corroborated by changeRef.
// When matched by a specific source_diff commit SHA (rather than FromRev..ToRev
// or ToRev), commitSHA is returned non-empty so callers can link directly to
// that commit.
func matchObservedChange(changeRef string, changes []observedChange) (observedChange, string, bool) {
	lowerRef := strings.ToLower(changeRef)
	// First pass: prefer explicit range or ToRev matches.
	for _, ch := range changes {
		if ch.FromRev != "" && ch.ToRev != "" {
			if containsRev(lowerRef, ch.FromRev+".."+ch.ToRev) ||
				containsRev(lowerRef, ch.FromRev+"..."+ch.ToRev) ||
				containsRev(lowerRef, shortSHA(ch.FromRev)+".."+shortSHA(ch.ToRev)) {
				return ch, "", true
			}
		}
		if ch.ToRev != "" && matchesRevToken(lowerRef, ch.ToRev) {
			return ch, "", true
		}
	}
	// Second pass: match individual commit SHAs observed in source_diff.
	for _, ch := range changes {
		for _, sha := range ch.Commits {
			if matchesRevToken(lowerRef, sha) {
				return ch, sha, true
			}
		}
	}
	return observedChange{}, "", false
}

func containsRev(lowerRef, needle string) bool {
	if needle == "" {
		return false
	}
	return strings.Contains(lowerRef, strings.ToLower(needle))
}

func matchesRevToken(lowerRef, rev string) bool {
	if rev == "" {
		return false
	}
	if strings.Contains(lowerRef, strings.ToLower(rev)) {
		return true
	}
	if short := shortSHA(rev); len(short) >= 7 {
		return strings.Contains(lowerRef, strings.ToLower(short))
	}
	return false
}

func shortSHA(rev string) string {
	if len(rev) >= 7 {
		return rev[:7]
	}
	return rev
}

// buildFileProvenance builds up to maxProvenanceFiles file permalinks, ranking
// files mentioned in changeRef or evidence ahead of other diff files while
// preserving diff order within each tier.
func buildFileProvenance(base string, style provenance.ForgeStyle, rev string, files []observedFile, changeRef string, evidence []string) []providers.ProvenanceLink {
	corpus := strings.ToLower(changeRef + "\n" + strings.Join(evidence, "\n"))
	var prioritized, rest []observedFile
	seen := make(map[string]bool, len(files))
	for _, f := range files {
		if seen[f.Path] {
			continue
		}
		seen[f.Path] = true
		if strings.Contains(corpus, strings.ToLower(f.Path)) {
			prioritized = append(prioritized, f)
		} else {
			rest = append(rest, f)
		}
	}
	ordered := make([]observedFile, 0, len(prioritized)+len(rest))
	ordered = append(ordered, prioritized...)
	ordered = append(ordered, rest...)
	links := make([]providers.ProvenanceLink, 0, min(len(ordered), maxProvenanceFiles))
	for _, f := range ordered {
		if len(links) >= maxProvenanceFiles {
			break
		}
		u := provenance.FileURL(base, style, rev, f.Path, f.FirstLine)
		if u == "" {
			continue
		}
		label := f.Path
		if f.FirstLine > 0 {
			label = fmt.Sprintf("%s#L%d", f.Path, f.FirstLine)
		}
		links = append(links, providers.ProvenanceLink{
			Label: label,
			URL:   u,
			Kind:  "file",
		})
	}
	if len(links) == 0 {
		return nil
	}
	return links
}
