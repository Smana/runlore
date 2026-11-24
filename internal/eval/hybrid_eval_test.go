// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/embed"
	"github.com/Smana/runlore/internal/providers"
)

// semanticFakeEmbedder is a deterministic fake catalog.Embedder for testing
// hybrid recall wiring in eval.Runner: texts about memory/OOM/RAM map to [1, 0],
// everything else maps to [0, 1], and every call charges promptTokens to any
// embed.UsageSink on ctx (mirroring embed.Client).
type semanticFakeEmbedder struct {
	tokensPerCall int
}

func (f *semanticFakeEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if sink := embed.UsageSinkFrom(ctx); sink != nil {
		sink.Record(f.tokensPerCall)
	}
	out := make([][]float32, len(texts))
	for i, s := range texts {
		lo := strings.ToLower(s)
		if strings.Contains(lo, "oom") || strings.Contains(lo, "ram") || strings.Contains(lo, "memory") {
			out[i] = []float32{1, 0}
		} else {
			out[i] = []float32{0, 1}
		}
	}
	return out, nil
}

// TestEvalHybridRecall confirms that eval.Runner wires a configured HybridRecallConfig
// into the per-case catalog, fires hybrid instant recall on a lexical-mismatch alert
// when cosine clears HybridMinScore/HybridMarginGap, records RecallFired and
// RecallShortCircuit, and attributes embedding tokens into Result.Usage.InputTokens.
func TestEvalHybridRecall(t *testing.T) {
	dir := t.TempDir()
	kbDir := filepath.Join(dir, "kb")
	if err := os.MkdirAll(kbDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oomEntry := `---
type: Incident
title: billing-api OOMKilled exit 137 under burst traffic
description: billing-api container terminated with OOMKilled exit code 137; raise memory limit.
resource: payments/billing-api
tags: [oom, memory, billing-api]
---

# Root cause
Container hit its cgroup memory limit and was terminated with OOMKilled exit 137.
`
	distractor := `---
type: Incident
title: billing-api TLS handshake timeout
description: billing-api upstream TLS certificate expired causing handshake failures.
resource: payments/billing-api
tags: [tls, certificate, billing-api]
---

# Root cause
Upstream TLS certificate expired.
`
	if err := os.WriteFile(filepath.Join(kbDir, "oom.md"), []byte(oomEntry), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kbDir, "tls.md"), []byte(distractor), 0o600); err != nil {
		t.Fatal(err)
	}

	c := Case{
		Name:       "paraphrase-oom",
		AlertTitle: "ContainerTerminated",
		Prompt:     "container killed due to RAM ceiling in payments/billing-api",
		Workload:   &CaseWorkload{Namespace: "payments", Name: "billing-api"},
		CatalogDir: "kb",
		Tools: map[string]string{
			"pod_status": "billing-api-0  CrashLoopBackOff  lastState=OOMKilled exit=137",
		},
		Expected:     Expected{MustContain: []string{"oom"}},
		ExpectRecall: "short_circuit",
		dir:          dir,
	}

	// Verify pass keeps the recalled entry; verify response carries 40 input / 10 output tokens.
	verifyKeep := providers.CompletionResponse{
		ToolCalls: []providers.ToolCall{
			{ID: "v", Name: "submit_verdicts", Args: `{"verdicts":[{"index":0,"verdict":"keep","confidence":0.9,"reason":"matches OOMKilled"}]}`},
		},
		Usage: providers.Usage{InputTokens: 40, OutputTokens: 10},
	}
	counting := &CountingModel{Inner: &seqModel{resp: []providers.CompletionResponse{verifyKeep}}}
	r := &Runner{
		Model: counting,
		Log:   discardLog(),
		Hybrid: &HybridRecallConfig{
			Embedder:  &semanticFakeEmbedder{tokensPerCall: 25},
			MinScore:  0.80,
			MarginGap: 0.05,
		},
	}

	camp := r.RunN(context.Background(), []Case{c}, 1)
	if len(camp.Aggregates) != 1 {
		t.Fatalf("want 1 aggregate, got %d", len(camp.Aggregates))
	}
	agg := camp.Aggregates[0]
	if agg.RecallFired != 1 || agg.RecallShortCircuit != 1 {
		t.Fatalf("hybrid recall must fire and short-circuit on lexical-mismatch alert: fired=%d short=%d missing=%v",
			agg.RecallFired, agg.RecallShortCircuit, agg.Missing)
	}
	if !agg.Reached {
		t.Fatalf("hybrid recall case should reach pass bar: %+v", agg)
	}
	// Total input tokens must include verify (40) + catalog reload embed (25) + recall query embed (25) = 90.
	if agg.InputTokens != 90 {
		t.Fatalf("InputTokens = %d, want 90 (40 verify + 25 reload embed + 25 query embed)", agg.InputTokens)
	}
	if agg.OutputTokens != 10 {
		t.Fatalf("OutputTokens = %d, want 10", agg.OutputTokens)
	}
}

// TestExamplesEvalCasesLoad validates that all 17 hermetic replay cases under
// examples/eval/ load cleanly, declare valid source groups when ground_truth is
// present, and build any referenced catalog fixtures without error.
func TestExamplesEvalCasesLoad(t *testing.T) {
	cases, err := Load(filepath.Join("..", "..", "examples", "eval"))
	if err != nil {
		t.Fatalf("Load examples/eval: %v", err)
	}
	if len(cases) != 17 {
		t.Fatalf("expected 17 replay cases in examples/eval, got %d", len(cases))
	}
	for _, c := range cases {
		t.Run(c.Name, func(t *testing.T) {
			if c.Prompt == "" {
				t.Error("empty prompt")
			}
			if len(c.Tools) == 0 {
				t.Error("no tools recorded")
			}
			if c.GroundTruth != nil {
				if err := ValidateSourceGroups("expected_sources", c.GroundTruth.ExpectedSources); err != nil {
					t.Errorf("invalid expected_sources: %v", err)
				}
				if err := ValidateSourceGroups("optional_sources", c.GroundTruth.OptionalSources); err != nil {
					t.Errorf("invalid optional_sources: %v", err)
				}
			}
			if c.hasCatalog() {
				cat, err := c.buildCatalog(context.Background())
				if err != nil {
					t.Fatalf("buildCatalog: %v", err)
				}
				if c.CatalogDir != "" && cat.Len() == 0 {
					t.Fatalf("catalog_dir %q indexed 0 entries", c.CatalogDir)
				}
			}
		})
	}
}
