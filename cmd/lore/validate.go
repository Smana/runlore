package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Smana/runlore/internal/app"
	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/kbvalidate"
	"github.com/Smana/runlore/internal/providers"
)

// runValidateKB is the `lore validate-kb` subcommand: it structurally validates
// every OKF entry under a catalog dir (the merge gate) and, with --semantic,
// adds the advisory review. Exits non-zero when any structural Error is found.
func runValidateKB(args []string) error {
	fs := flag.NewFlagSet("validate-kb", flag.ContinueOnError)
	format := fs.String("format", "text", "output format: text|github")
	semantic := fs.Bool("semantic", false, "run the LLM semantic advisory (needs a model in --config)")
	cfgPath := fs.String("config", "runlore.yaml", "config path (for the model when --semantic)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	dir := fs.Arg(0)
	if dir == "" {
		return fmt.Errorf("usage: lore validate-kb [--semantic] [--format text|github] <catalog-dir>")
	}

	var model providers.ModelProvider
	if *semantic {
		if cfg, err := config.Load(*cfgPath); err == nil && app.ModelConfigured(cfg) {
			model = app.BuildModel(cfg, os.Getenv(cfg.Model.APIKeyEnv))
		} else {
			fmt.Fprintln(os.Stderr, "validate-kb: --semantic set but no usable model in config; running structural only")
		}
	}

	hadError, err := validateKB(os.Stdout, dir, *format, model)
	if err != nil {
		return err
	}
	if hadError {
		return fmt.Errorf("KB validation failed: structural errors found")
	}
	return nil
}

// validateKB loads dir, structurally validates every entry, and (when model is
// non-nil) appends the semantic advisory. It returns whether any Error was found.
func validateKB(w io.Writer, dir, format string, model providers.ModelProvider) (bool, error) {
	entries, skipped, err := catalog.Load(dir)
	if err != nil {
		return false, err
	}
	hadError := false

	// Unparseable files are hard errors here (unlike catalog.Load, which skips
	// them): at PR time we want a malformed entry to block the merge.
	for _, s := range skipped {
		emitIssue(w, format, "", kbvalidate.Issue{Severity: kbvalidate.SeverityError, Field: "frontmatter", Message: "failed to parse: " + s})
		hadError = true
	}

	for _, e := range entries {
		issues := kbvalidate.ValidateStructural(e)
		for _, iss := range issues {
			emitIssue(w, format, e.Path, iss)
		}
		if kbvalidate.HasErrors(issues) {
			hadError = true
		}
		if model != nil {
			adv, aerr := kbvalidate.ReviewSemantic(context.Background(), e, model)
			if aerr != nil {
				_, _ = fmt.Fprintf(w, "advisory\t%s\tsemantic review skipped: %v\n", e.Path, aerr)
			} else if !adv.Skipped {
				emitAdvisory(w, e.Path, adv)
			}
		}
	}
	return hadError, nil
}

func emitIssue(w io.Writer, format, path string, iss kbvalidate.Issue) {
	if format == "github" {
		lvl := "warning"
		if iss.Severity == kbvalidate.SeverityError {
			lvl = "error"
		}
		_, _ = fmt.Fprintf(w, "::%s file=%s::%s: %s\n", lvl, path, iss.Field, iss.Message)
		return
	}
	_, _ = fmt.Fprintf(w, "%s\t%s\t%s: %s\n", iss.Severity, path, iss.Field, iss.Message)
}

func emitAdvisory(w io.Writer, path string, adv kbvalidate.Advisory) {
	mark := func(v kbvalidate.Verdict) string {
		if v.OK {
			return "ok"
		}
		return "REVIEW"
	}
	_, _ = fmt.Fprintf(w, "advisory\t%s\tcause-explains-symptom: %s (%s); durable: %s (%s)\n",
		path, mark(adv.CauseExplainsSymptom), adv.CauseExplainsSymptom.Rationale,
		mark(adv.Durable), adv.Durable.Rationale)
}
