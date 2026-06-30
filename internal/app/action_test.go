package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
)

// writeIntactChain writes a fresh, intact audit chain to a file in dir and
// returns its path.
func writeIntactChain(t *testing.T, dir string) string {
	t.Helper()
	path := filepath.Join(dir, "audit.jsonl")
	l, err := audit.Open(path)
	if err != nil {
		t.Fatalf("seed open: %v", err)
	}
	for _, op := range []string{"suspend", "resume", "reconcile"} {
		if err := l.Log(audit.Record{Actor: "auto", Op: op, Target: "Kustomization/apps/web", Decision: audit.DecisionExecuted}); err != nil {
			t.Fatalf("seed log: %v", err)
		}
	}
	if err := l.Close(); err != nil {
		t.Fatalf("seed close: %v", err)
	}
	return path
}

// writeBrokenChain writes an intact chain, then tampers with a record's content
// so chain verification fails, and returns its path.
func writeBrokenChain(t *testing.T, dir string) string {
	t.Helper()
	path := writeIntactChain(t, dir)
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 {
		t.Fatalf("want 3 records, got %d", len(lines))
	}
	lines[1] = strings.Replace(lines[1], "apps", "flux-system", 1)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestBuildAuditorBrokenChainFailsClosedUnderApprove(t *testing.T) {
	path := writeBrokenChain(t, t.TempDir())
	cfg := &config.Config{}
	cfg.Actions.Mode = config.ActionApprove
	cfg.Actions.AuditLogPath = path

	_, closeFn, err := BuildAuditor(cfg, discardLog())
	if closeFn != nil {
		closeFn()
	}
	if err == nil {
		t.Fatal("approve over a broken chain must fail closed (error), got nil")
	}
}

func TestBuildAuditorBrokenChainFailsClosedUnderAuto(t *testing.T) {
	path := writeBrokenChain(t, t.TempDir())
	cfg := &config.Config{}
	cfg.Actions.Mode = config.ActionAuto
	cfg.Actions.AuditLogPath = path

	_, closeFn, err := BuildAuditor(cfg, discardLog())
	if closeFn != nil {
		closeFn()
	}
	if err == nil {
		t.Fatal("auto over a broken chain must fail closed (error), got nil")
	}
}

func TestBuildAuditorBrokenChainWarnsUnderSuggest(t *testing.T) {
	path := writeBrokenChain(t, t.TempDir())
	cfg := &config.Config{}
	cfg.Actions.Mode = config.ActionSuggest
	cfg.Actions.AuditLogPath = path

	aud, closeFn, err := BuildAuditor(cfg, discardLog())
	if closeFn != nil {
		defer closeFn()
	}
	if err != nil {
		t.Fatalf("suggest over a broken chain must proceed (warn), got error: %v", err)
	}
	if aud == nil {
		t.Fatal("suggest over a broken chain must still return an auditor")
	}
}

func TestBuildAuditorBrokenChainWarnsUnderOff(t *testing.T) {
	path := writeBrokenChain(t, t.TempDir())
	cfg := &config.Config{}
	cfg.Actions.Mode = config.ActionOff
	cfg.Actions.AuditLogPath = path

	aud, closeFn, err := BuildAuditor(cfg, discardLog())
	if closeFn != nil {
		defer closeFn()
	}
	if err != nil {
		t.Fatalf("off over a broken chain must proceed (warn), got error: %v", err)
	}
	if aud == nil {
		t.Fatal("off over a broken chain must still return an auditor")
	}
}

func TestBuildAuditorIntactChainUnderApprove(t *testing.T) {
	path := writeIntactChain(t, t.TempDir())
	cfg := &config.Config{}
	cfg.Actions.Mode = config.ActionApprove
	cfg.Actions.AuditLogPath = path

	aud, closeFn, err := BuildAuditor(cfg, discardLog())
	if closeFn != nil {
		defer closeFn()
	}
	if err != nil {
		t.Fatalf("approve over an intact chain must succeed, got: %v", err)
	}
	if aud == nil {
		t.Fatal("intact chain under approve must return an auditor")
	}
}

func TestBuildAuditorNoPathIsNop(t *testing.T) {
	cfg := &config.Config{}
	cfg.Actions.Mode = config.ActionApprove
	// no AuditLogPath
	aud, closeFn, err := BuildAuditor(cfg, discardLog())
	if closeFn != nil {
		defer closeFn()
	}
	if err != nil {
		t.Fatalf("no audit path must not error, got: %v", err)
	}
	if _, ok := aud.(audit.Nop); !ok {
		t.Fatalf("no audit path must yield a Nop auditor, got %T", aud)
	}
}
