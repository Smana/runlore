// SPDX-License-Identifier: Apache-2.0

// Package revertpr implements the pure-Go, deterministic Git-revert Draft PR/MR
// executor for the approve-only "revert_pr" action. It constructs an exact
// inverse commit over the forge API (no git CLI binary) for a single
// server-observed GitOps commit, failing closed on merge commits, root commits,
// oversized diffs (>50 files), protected paths, or overlapping later edits.
package revertpr

import (
	"context"
	"fmt"
	"log/slog"
	"path"
	"slices"
	"strings"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

const (
	// maxRevertFiles bounds the blast radius of a single deterministic revert.
	maxRevertFiles = 50
	// branchPrefix prefixes every branch created by revert_pr.
	branchPrefix = "runlore/revert-"
)

// OnOpenedFunc is an optional callback invoked after a Draft revert PR/MR is
// created or an existing open one is reused.
type OnOpenedFunc func(ctx context.Context, act providers.Action, prURL string, reused bool)

// Option configures an Executor.
type Option func(*Executor)

// WithAuditor attaches an audit logger for idempotency/reuse records.
func WithAuditor(aud audit.Auditor) Option {
	return func(e *Executor) { e.aud = aud }
}

// WithLogger attaches a structured logger.
func WithLogger(log *slog.Logger) Option {
	return func(e *Executor) { e.log = log }
}

// WithOnOpened registers a callback invoked with the opened (or reused) Draft PR URL.
func WithOnOpened(fn OnOpenedFunc) Option {
	return func(e *Executor) { e.onOpened = fn }
}

// Executor runs approved revert_pr actions deterministically over a forge API.
type Executor struct {
	forge          providers.RevertForge
	allowedRepos   []string
	protectedPaths []string
	aud            audit.Auditor
	log            *slog.Logger
	onOpened       OnOpenedFunc
}

var _ action.Executor = (*Executor)(nil)

// New builds a pure-Go revert_pr Executor. allowedRepos is enforced as defense
// in depth at execution time (empty permits nothing). protectedPaths are
// appended to the built-in protected path rules.
func New(forge providers.RevertForge, allowedRepos, protectedPaths []string, opts ...Option) *Executor {
	e := &Executor{
		forge:          forge,
		allowedRepos:   slices.Clone(allowedRepos),
		protectedPaths: slices.Clone(protectedPaths),
		aud:            audit.Nop{},
		log:            slog.Default(),
	}
	for _, opt := range opts {
		if opt != nil {
			opt(e)
		}
	}
	if e.aud == nil {
		e.aud = audit.Nop{}
	}
	if e.log == nil {
		e.log = slog.Default()
	}
	return e
}

// Execute validates all deterministic revert invariants and opens (or reuses) a
// Draft revert PR/MR for a single server-observed GitOps commit.
func (e *Executor) Execute(ctx context.Context, a providers.Action) error {
	if a.Op != "revert_pr" {
		return fmt.Errorf("revertpr: unsupported op %q", a.Op)
	}
	if e.forge == nil {
		return fmt.Errorf("revertpr: revert forge client is not configured")
	}
	if a.Revert == nil || strings.TrimSpace(a.Revert.Repo) == "" || strings.TrimSpace(a.Revert.CommitSHA) == "" {
		return fmt.Errorf("revertpr: missing server-resolved revert target")
	}
	repo := strings.TrimSpace(a.Revert.Repo)
	sha := strings.TrimSpace(a.Revert.CommitSHA)
	if !repoAllowed(repo, e.allowedRepos) {
		return fmt.Errorf("revertpr: repo %q is not in allowed_repos", repo)
	}
	if len(sha) < 7 {
		return fmt.Errorf("revertpr: commit SHA %q is too short", sha)
	}

	// 1. Resolve default branch + current HEAD.
	branch, headSHA, err := e.forge.DefaultBranch(ctx, repo)
	if err != nil {
		return fmt.Errorf("revertpr: resolve default branch for %s: %w", repo, err)
	}
	a.Revert.Branch = branch

	// 2. Idempotency: reuse an existing open PR/MR for this commit's revert branch.
	short := sha
	if len(short) > 12 {
		short = short[:12]
	}
	headBranch := branchPrefix + short
	existingURL, _, found, err := e.forge.FindOpenPRByHead(ctx, repo, headBranch)
	if err != nil {
		return fmt.Errorf("revertpr: check existing PR for %s: %w", headBranch, err)
	}
	if found && existingURL != "" {
		a.Revert.ResultURL = existingURL
		_ = e.aud.Log(audit.Record{
			Actor:    "revert_pr",
			Op:       a.Op,
			Target:   repo + "@" + short,
			Decision: audit.DecisionExecuted,
			Reason:   "reused_existing_pr: " + existingURL,
		})
		e.log.Info("revert_pr reused existing open Draft PR", "repo", repo, "branch", headBranch, "url", existingURL)
		if e.onOpened != nil {
			e.onOpened(ctx, a, existingURL, true)
		}
		return nil
	}

	// 3. Fetch commit detail and enforce single-parent, size cap, and protected paths.
	detail, err := e.forge.CommitDetail(ctx, repo, sha)
	if err != nil {
		return fmt.Errorf("revertpr: fetch commit %s in %s: %w", sha, repo, err)
	}
	switch len(detail.Parents) {
	case 0:
		return fmt.Errorf("revertpr: refusing commit %s with 0 parents (root commit)", short)
	case 1:
		// clean single-parent commit
	default:
		return fmt.Errorf("revertpr: refusing merge commit %s (%d parents)", short, len(detail.Parents))
	}
	if len(detail.Files) == 0 {
		return fmt.Errorf("revertpr: refusing commit %s with no changed files", short)
	}
	if len(detail.Files) > maxRevertFiles {
		return fmt.Errorf("revertpr: refusing commit %s touching %d files (exceeds safety cap of %d)", short, len(detail.Files), maxRevertFiles)
	}
	for _, f := range detail.Files {
		if reason, blocked := isProtectedPath(f.Path, e.protectedPaths); blocked {
			return fmt.Errorf("revertpr: refusing commit %s: file %q matches protected path (%s)", short, f.Path, reason)
		}
		if f.PreviousPath != "" {
			if reason, blocked := isProtectedPath(f.PreviousPath, e.protectedPaths); blocked {
				return fmt.Errorf("revertpr: refusing commit %s: file %q matches protected path (%s)", short, f.PreviousPath, reason)
			}
		}
	}

	// 4. Clean-head invariant: fail closed if any touched file changed between C and HEAD.
	if headSHA != "" && !strings.EqualFold(sha, headSHA) {
		later, err := e.forge.CompareChangedFiles(ctx, repo, sha, headSHA)
		if err != nil {
			return fmt.Errorf("revertpr: compare %s..%s in %s: %w", short, headSHA, repo, err)
		}
		touched := make(map[string]bool, len(detail.Files)*2)
		for _, f := range detail.Files {
			touched[cleanSlashPath(f.Path)] = true
			if f.PreviousPath != "" {
				touched[cleanSlashPath(f.PreviousPath)] = true
			}
		}
		var conflicts []string
		seenConflict := map[string]bool{}
		for _, lf := range later {
			norm := cleanSlashPath(lf)
			if touched[norm] && !seenConflict[norm] {
				seenConflict[norm] = true
				conflicts = append(conflicts, norm)
			}
		}
		if len(conflicts) > 0 {
			slices.Sort(conflicts)
			return fmt.Errorf("revertpr: refusing commit %s: overlapping later edits on %s for %s", short, branch, strings.Join(conflicts, ", "))
		}
	}

	// 5. Materialize exact inverse file actions from parent P.
	parentSHA := detail.Parents[0]
	actions, err := e.buildInverseActions(ctx, repo, parentSHA, detail.Files)
	if err != nil {
		return err
	}

	// 6. Create atomic inverse commit + Draft PR/MR.
	subject := commitSubject(detail.Message, short)
	req := providers.RevertPRRequest{
		Repo:          repo,
		BaseBranch:    branch,
		BaseSHA:       headSHA,
		HeadBranch:    headBranch,
		CommitMessage: fmt.Sprintf("Revert %q\n\nThis reverts commit %s.", subject, sha),
		Title:         fmt.Sprintf("Revert %q", subject),
		Body:          renderPRBody(a, sha, subject),
		Files:         actions,
	}
	prURL, _, err := e.forge.CreateRevertDraftPR(ctx, req)
	if err != nil {
		return fmt.Errorf("revertpr: create draft revert PR in %s: %w", repo, err)
	}
	a.Revert.ResultURL = prURL
	e.log.Info("revert_pr opened Draft PR", "repo", repo, "branch", headBranch, "url", prURL)
	if e.onOpened != nil {
		e.onOpened(ctx, a, prURL, false)
	}
	return nil
}

func (e *Executor) buildInverseActions(ctx context.Context, repo, parentSHA string, files []providers.CommitFile) ([]providers.RevertFileAction, error) {
	out := make([]providers.RevertFileAction, 0, len(files))
	for _, f := range files {
		switch strings.ToLower(strings.TrimSpace(f.Status)) {
		case "added":
			out = append(out, providers.RevertFileAction{Path: f.Path, Delete: true})
		case "modified", "removed":
			blob, found, err := e.forge.FileBlob(ctx, repo, parentSHA, f.Path)
			if err != nil {
				return nil, fmt.Errorf("revertpr: fetch parent blob %s@%s: %w", f.Path, parentSHA, err)
			}
			if !found {
				return nil, fmt.Errorf("revertpr: parent blob %s not found at %s", f.Path, parentSHA)
			}
			out = append(out, providers.RevertFileAction{Path: f.Path, Content: blob})
		case "renamed":
			if f.PreviousPath == "" {
				return nil, fmt.Errorf("revertpr: renamed file %q missing previous_path", f.Path)
			}
			out = append(out, providers.RevertFileAction{Path: f.Path, Delete: true})
			blob, found, err := e.forge.FileBlob(ctx, repo, parentSHA, f.PreviousPath)
			if err != nil {
				return nil, fmt.Errorf("revertpr: fetch parent blob %s@%s: %w", f.PreviousPath, parentSHA, err)
			}
			if !found {
				return nil, fmt.Errorf("revertpr: parent blob %s not found at %s", f.PreviousPath, parentSHA)
			}
			out = append(out, providers.RevertFileAction{Path: f.PreviousPath, Content: blob})
		default:
			return nil, fmt.Errorf("revertpr: unsupported file status %q for %q", f.Status, f.Path)
		}
	}
	return out, nil
}

func renderPRBody(a providers.Action, sha, subject string) string {
	var b strings.Builder
	b.WriteString("## Deterministic GitOps Revert (Draft)\n\n")
	fmt.Fprintf(&b, "This Draft PR deterministically reverts commit `%s` (%s) by restoring each touched file to its exact parent blob.\n\n", sha, redact.Secrets(subject))
	if a.Revert != nil {
		if a.Revert.ProvenanceURL != "" {
			fmt.Fprintf(&b, "- **Reverted commit:** [%s](%s)\n", sha, a.Revert.ProvenanceURL)
		} else {
			fmt.Fprintf(&b, "- **Reverted commit:** `%s`\n", sha)
		}
		if a.Revert.IncidentTitle != "" {
			fmt.Fprintf(&b, "- **Incident:** %s\n", redact.Secrets(a.Revert.IncidentTitle))
		}
		if a.Revert.IncidentSummary != "" {
			fmt.Fprintf(&b, "- **Root cause summary:** %s\n", redact.Secrets(a.Revert.IncidentSummary))
		}
		if a.ApprovalID != "" {
			fmt.Fprintf(&b, "- **Approval ID:** `%s`\n", a.ApprovalID)
		}
		if a.Revert.AuditID != "" {
			fmt.Fprintf(&b, "- **Audit ID:** `%s`\n", a.Revert.AuditID)
		}
		if len(a.Revert.Evidence) > 0 {
			b.WriteString("\n### Evidence\n\n")
			for _, ev := range a.Revert.Evidence {
				fmt.Fprintf(&b, "- %s\n", redact.Secrets(ev))
			}
		}
	}
	b.WriteString("\n---\n*Opened by RunLore (`revert_pr`, approve-only). Review the diff carefully before marking ready and merging.*\n")
	return b.String()
}

func commitSubject(msg, fallback string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return fallback
	}
	return line
}

func repoAllowed(repo string, allowed []string) bool {
	target := strings.ToLower(strings.TrimSpace(repo))
	if target == "" {
		return false
	}
	for _, entry := range allowed {
		if strings.ToLower(strings.TrimSpace(entry)) == target {
			return true
		}
	}
	return false
}

func cleanSlashPath(p string) string {
	p = strings.ReplaceAll(p, "\\", "/")
	p = path.Clean("/" + p)
	return strings.TrimPrefix(p, "/")
}

// isProtectedPath checks built-in (non-shrinkable) and operator protected path
// rules against a repository-relative file path.
func isProtectedPath(filePath string, extra []string) (string, bool) {
	norm := cleanSlashPath(filePath)
	segs := strings.Split(norm, "/")
	for _, seg := range segs {
		if seg == "flux-system" || seg == "kube-system" {
			return "built-in " + seg, true
		}
	}
	if norm == ".github" || strings.HasPrefix(norm, ".github/") {
		return "built-in .github/**", true
	}
	if norm == ".gitlab-ci.yml" || norm == ".gitlab" || strings.HasPrefix(norm, ".gitlab/") {
		return "built-in .gitlab", true
	}
	for _, pat := range extra {
		p := strings.TrimSpace(pat)
		if p == "" {
			continue
		}
		if matchGlob(cleanSlashPath(p), norm) {
			return "operator pattern " + pat, true
		}
	}
	return "", false
}

// matchGlob matches pattern against name using '/' as separator, supporting
// "**" to match zero or more path segments in addition to path.Match wildcards.
func matchGlob(pattern, name string) bool {
	return matchSegments(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegments(pat, segs []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			// Collapse consecutive "**".
			for len(pat) > 1 && pat[1] == "**" {
				pat = pat[1:]
			}
			if len(pat) == 1 {
				return true
			}
			for i := 0; i <= len(segs); i++ {
				if matchSegments(pat[1:], segs[i:]) {
					return true
				}
			}
			return false
		}
		if len(segs) == 0 {
			return false
		}
		ok, err := path.Match(pat[0], segs[0])
		if err != nil || !ok {
			return false
		}
		pat = pat[1:]
		segs = segs[1:]
	}
	return len(segs) == 0
}
