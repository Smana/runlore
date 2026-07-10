// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
)

// runAudit is the `lore audit` subcommand dispatcher. Today it has a single verb,
// `verify`, which re-walks the hash-chained action audit log and reports the first
// broken link. It is the operator-facing counterpart to the verify-on-open check
// that gates startup (see internal/app.BuildAuditor).
func runAudit(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: lore audit verify --path <audit.jsonl> | --config <runlore.yaml>")
	}
	switch args[0] {
	case "verify":
		return runAuditVerify(args[1:])
	default:
		return fmt.Errorf("unknown audit verb %q (want: verify)", args[0])
	}
}

// runAuditVerify parses flags for `lore audit verify` and resolves the audit-log
// path: --path is the simple required form; --config reads
// actions.audit_log_path from a RunLore config instead. It exits non-zero (via a
// returned error) when the chain is broken.
func runAuditVerify(args []string) error {
	fs := flag.NewFlagSet("audit verify", flag.ContinueOnError)
	path := fs.String("path", "", "path to the hash-chained audit log (JSONL)")
	cfgPath := fs.String("config", "", "read actions.audit_log_path from this RunLore config instead of --path")
	if err := fs.Parse(args); err != nil {
		return err
	}

	p := *path
	if p == "" && *cfgPath != "" {
		cfg, err := config.Load(*cfgPath)
		if err != nil {
			return fmt.Errorf("load config: %w", err)
		}
		p = cfg.Actions.AuditLogPath
		if p == "" {
			return fmt.Errorf("config %s has no actions.audit_log_path", *cfgPath)
		}
	}
	return auditVerify(os.Stdout, p)
}

// auditVerify opens the audit log at path, re-walks the chain, and writes the
// result to w: "OK: chain intact (<N> records)" on success, or returns the
// broken-link error from audit.Verify. An empty path is a usage error. The
// caller maps a non-nil error to a non-zero exit.
func auditVerify(w io.Writer, path string) error {
	if path == "" {
		return fmt.Errorf("audit verify: --path (or a --config with actions.audit_log_path) is required")
	}
	f, err := os.Open(path) //nolint:gosec // G304: operator-supplied audit-log path
	if err != nil {
		return fmt.Errorf("audit verify: open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	if err := audit.Verify(f); err != nil {
		return fmt.Errorf("audit verify: %w", err)
	}
	// Verify consumed the file; rewind to count the (now-trusted) records for the
	// success line.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return fmt.Errorf("audit verify: seek: %w", err)
	}
	n := countRecords(f)
	_, _ = fmt.Fprintf(w, "OK: chain intact (%d records)\n", n)
	return nil
}

// countRecords counts non-empty lines (records) in an already-verified chain.
func countRecords(r io.Reader) int {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	n := 0
	for sc.Scan() {
		if sc.Text() != "" {
			n++
		}
	}
	return n
}
