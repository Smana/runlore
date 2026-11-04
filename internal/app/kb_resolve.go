// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/kbresolve"
	"github.com/Smana/runlore/internal/logging"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

// ErrUsage marks a command-line mistake: main prints it and exits 2, not 1.
var ErrUsage = errors.New("usage")

const (
	kbResolveUsage   = "usage: lore kb resolve --pr <n> --fix <pull-request URL> --verdict resolved|persisting [--merge-sha <sha>] [--evidence <file>] [--config <path>]"
	maxEvidenceBytes = 16 * 1024
)

// newResolveForge builds the forge `lore kb resolve` writes through; tests swap it.
var newResolveForge = func(cfgPath string) (kbresolve.Forge, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, err
	}
	if cfg.Forge.KBRepo == "" {
		return nil, fmt.Errorf("kb resolve requires forge.kb_repo")
	}
	log := logging.FromConfig(os.Stderr, cfg.Logging.Format, cfg.Logging.Level)
	tok := BuildForgeTokenSource(cfg, log)
	if tok == nil {
		return nil, fmt.Errorf("kb resolve requires a configured GitHub App (forge.github_app)")
	}
	// The command is one short-lived call: the audit file closes with the process.
	aud, _, err := BuildAuditor(cfg, log)
	if err != nil {
		return nil, err
	}
	return buildGuardedForge(cfg, tok, false, aud, log)
}

// runKBResolve records an applied fix on the entry a pull request carries. It
// writes nothing to the outcome ledger (KP22): the entry is the record.
func runKBResolve(args []string, w io.Writer) error {
	fs := flag.NewFlagSet("kb resolve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	cfgPath := fs.String("config", "runlore.yaml", "path to config file")
	pr := fs.Int("pr", 0, "the knowledge-base pull request that carries the entry")
	fix := fs.String("fix", "", "URL of the pull request that applied the fix")
	sha := fs.String("merge-sha", "", "the fix's merge commit")
	verdict := fs.String("verdict", "", "resolved or persisting")
	evidence := fs.String("evidence", "", "file holding the soak evidence (up to 16 KiB)")
	if err := fs.Parse(args); err != nil {
		return fmt.Errorf("%w: %w\n%s", ErrUsage, err, kbResolveUsage)
	}
	usage := func(format string, a ...any) error {
		return fmt.Errorf("%w: %s\n%s", ErrUsage, fmt.Sprintf(format, a...), kbResolveUsage)
	}
	switch {
	case fs.NArg() > 0:
		return usage("unexpected argument %q", fs.Arg(0))
	case *pr <= 0:
		return usage("--pr is required")
	case *fix == "":
		return usage("--fix is required")
	case *verdict != "resolved" && *verdict != "persisting":
		return usage("--verdict must be resolved or persisting, not %q", *verdict)
	}
	var text string
	if *evidence != "" {
		raw, err := readLimited(*evidence, maxEvidenceBytes)
		if err != nil {
			return usage("%v", err)
		}
		text = redact.Secrets(string(raw))
	}
	r := providers.AppliedResolution{FixPR: *fix, MergeSHA: *sha, Verdict: *verdict, Evidence: text, VerifiedAt: time.Now().UTC()}
	if err := kbresolve.Validate(r); err != nil {
		return usage("%v", err)
	}
	f, err := newResolveForge(*cfgPath)
	if err != nil {
		return err
	}
	res, err := kbresolve.Resolve(context.Background(), f, kbresolve.Target{PR: *pr}, r)
	if err != nil {
		return err
	}
	switch res.Route {
	case "appended":
		_, err = fmt.Fprintf(w, "PR #%d: resolution appended\n", *pr)
	case "edit-pr":
		_, err = fmt.Fprintf(w, "PR #%d: already merged, resolution proposed in %s\n", *pr, res.URL)
	default:
		_, err = fmt.Fprintf(w, "PR #%d: resolution already recorded\n", *pr)
	}
	return err
}

// readLimited reads a file of at most limit bytes; a larger one is an error.
func readLimited(path string, limit int) ([]byte, error) {
	fh, err := os.Open(path) //nolint:gosec // G304: the operator-passed evidence file
	if err != nil {
		return nil, err
	}
	defer func() { _ = fh.Close() }()
	raw, err := io.ReadAll(io.LimitReader(fh, int64(limit)+1))
	if err != nil {
		return nil, err
	}
	if len(raw) > limit {
		return nil, fmt.Errorf("%s is over %d KiB", path, limit/1024)
	}
	return raw, nil
}
