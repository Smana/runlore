// SPDX-License-Identifier: Apache-2.0

package app

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

// TestKBImportReportsWhatModelEnrichmentSpent pins the visibility gap on
// `lore kb import --model`: kbimport.Enrich makes ONE model call PER IMPORTED FILE,
// and nothing counted them — not an investigation's usage totals, not any budget,
// not a metric. Seeding a KB from an existing runbook repo is exactly the case where
// that is hundreds of calls in one unattended command.
//
// The report goes to the command's own output because this is a one-shot CLI:
// telemetry.Setup (the Prometheus exporter) runs only under `lore serve`, so an OTel
// counter here would record into the global no-op meter, in a process that exits
// seconds later, and never be scraped by anything.
func TestKBImportReportsWhatModelEnrichmentSpent(t *testing.T) {
	srv := mockModelServer(t)
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "runlore.yaml")
	cfgBody := fmt.Sprintf("model:\n  provider: openai\n  model: mock\n  base_url: %s/v1\n", srv.URL)
	if err := os.WriteFile(cfgPath, []byte(cfgBody), 0o600); err != nil {
		t.Fatal(err)
	}

	src, kb := t.TempDir(), t.TempDir()
	writeFile(t, src, "redis-failover.md", "# Redis failover\n\nHow to fail over redis.\n")
	writeFile(t, src, "etcd-defrag.md", "# Etcd defrag\n\nHow to defragment etcd.\n")

	var buf bytes.Buffer
	if err := runKBImport([]string{src, "--into", kb, "--config", cfgPath, "--model", "--dry-run"}, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "model spend:") {
		t.Fatalf("`kb import --model` must report what the enrichment calls cost; got:\n%s", out)
	}
	// The mock reports 1000 input / 100 output per completion, one per source file.
	if !strings.Contains(out, "2000 input") || !strings.Contains(out, "200 output") {
		t.Errorf("the reported spend must be the real accumulated usage over both files "+
			"(2000 input / 200 output); got:\n%s", out)
	}
}

// TestKBImportWithoutAModelReportsNoSpendLine pins the control: the deterministic
// import makes no model calls, so it must not print a spend line at all. A line
// reading "0 input / 0 output" on a run that never had a model would be the same
// mistake as a $0.00 cost on an unpriced eval — it makes an absent thing look
// measured.
func TestKBImportWithoutAModelReportsNoSpendLine(t *testing.T) {
	src, kb := t.TempDir(), t.TempDir()
	writeFile(t, src, "redis-failover.md", "# Redis failover\n\nHow to fail over redis.\n")
	var buf bytes.Buffer
	if err := runKBImport([]string{src, "--into", kb, "--dry-run"}, &buf); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "model spend:") {
		t.Fatalf("a deterministic import makes no model calls and must claim no spend; got:\n%s", buf.String())
	}
}

// TestModelSpendLineOmitsCostWhenUnpriced pins the shared renderer both KB commands
// and the eval judge report through: it states the tokens always, and a dollar figure
// only when model.pricing supplies rates. An unpriced deployment must not be told
// "~$0.0000" — that reads as "this was free", not as "nobody priced it".
func TestModelSpendLineOmitsCostWhenUnpriced(t *testing.T) {
	u := providers.Usage{InputTokens: 1_000_000, OutputTokens: 200_000}

	unpriced := ModelSpendLine(configForAbsentFile(), "kb import", u)
	if !strings.Contains(unpriced, "1000000 input") || !strings.Contains(unpriced, "200000 output") {
		t.Errorf("the token counts must always be stated; got %q", unpriced)
	}
	if strings.Contains(unpriced, "$") {
		t.Errorf("an unpriced config must not claim a dollar figure; got %q", unpriced)
	}

	cfg := configForAbsentFile()
	cfg.Model.Pricing = &config.Pricing{InputUSDPerMTok: 3, OutputUSDPerMTok: 15}
	priced := ModelSpendLine(cfg, "kb import", u)
	// 1M in x $3 + 0.2M out x $15 = $6.00
	if !strings.Contains(priced, "$6.00") {
		t.Errorf("a priced config must state the estimated cost ($6.00); got %q", priced)
	}
}
