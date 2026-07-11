// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

// capturingScored is a ScoredSearcher that records the last query it was asked and
// returns fixed hits — used to assert server-side kb_search enrichment.
type capturingScored struct {
	hits      []catalog.ScoredEntry
	lastQuery string
}

func (c *capturingScored) SearchScored(q string, _ int) ([]catalog.ScoredEntry, error) {
	c.lastQuery = q
	return c.hits, nil
}

// Search satisfies catalog.Searcher; the scored path is preferred by KBSearchTool,
// so this is a fallback that records the query the same way.
func (c *capturingScored) Search(q string, _ int) ([]catalog.Entry, error) {
	c.lastQuery = q
	es := make([]catalog.Entry, len(c.hits))
	for i, h := range c.hits {
		es[i] = h.Entry
	}
	return es, nil
}

// nearMissRecall builds a Recall whose single structurally-agreeing candidate scores
// BELOW the fire thresholds, so the confidence gate never fires but the structural
// pre-filter still finds it — the exact near-miss condition C2 targets.
func nearMissRecall() *Recall {
	return &Recall{
		MinScore: 4.0, MarginGap: 2.0, SoloFloor: 4.0,
		Catalog: fakeScored{hits: []catalog.ScoredEntry{{
			Entry: catalog.Entry{
				Title:    "Harbor Registry Down due to IAM Access Key Quota Limit",
				Path:     "harbor.md",
				Resource: "tooling/harbor-registry",
				Body:     "## Cause\nThe registry's IAM access key hit its quota.\n\n## Resolution\nRotate the access key and raise the quota.\n",
			},
			Score: 0.096, // measured live: far below the 4.0 solo_floor → recall never fires
		}}},
	}
}

func nearMissReq() Request {
	return Request{
		Title:       "KubePodNotReady",
		Fingerprint: "fp-nm",
		Workload:    providers.Workload{Namespace: "tooling", Name: "harbor-registry-59598dbd57-ltkzw"},
		Labels:      map[string]string{"alertname": "KubePodNotReady"},
	}
}

// TestNearMissInjectedIntoSeed proves that when recall does NOT fire but a
// structurally-agreeing candidate exists, the seed prompt carries a clearly-framed
// UNVERIFIED near-miss block, and that it does not change the delivered verdict
// machinery (the block only shapes the prompt).
func TestNearMissInjectedIntoSeed(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
			Args: `{"confidence":0.8,"verdict":"action_suggested","root_causes":[{"summary":"fresh finding","confidence":0.8}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall:     nearMissRecall(),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	if err := li.Investigate(context.Background(), nearMissReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// The loop must have run (recall did not fire).
	if model.i == 0 {
		t.Fatal("model not called; a below-threshold recall must run the full loop")
	}
	// The seed (first user message) must carry the framed UNVERIFIED near-miss block.
	seed := model.reqs[0].Messages[0].Content
	for _, want := range []string{
		"possibly-related past incident",
		"UNVERIFIED",
		"Harbor Registry Down due to IAM Access Key Quota Limit",
		"Cause: The registry's IAM access key hit its quota.",
		"Resolution: Rotate the access key and raise the quota.",
	} {
		if !strings.Contains(seed, want) {
			t.Fatalf("seed missing near-miss fragment %q; seed:\n%s", want, seed)
		}
	}
	// The block only shapes the prompt: the delivered verdict/finding comes from the
	// model, not the near-miss entry.
	if got == nil || len(got.RootCauses) != 1 || got.RootCauses[0].Summary != "fresh finding" {
		t.Fatalf("near-miss must not alter the delivered finding: %+v", got)
	}
	if got.Recalled {
		t.Fatal("a near-miss injection is not a recall; Recalled must stay false")
	}
	if got.Verdict != providers.VerdictActionSuggested {
		t.Fatalf("verdict must come from the model, got %q", got.Verdict)
	}
	if got.RecalledEntry != "" {
		t.Fatalf("near-miss must not stamp RecalledEntry, got %q", got.RecalledEntry)
	}
}

// TestNearMissDisabledUnderAuto proves the near-miss injection is gated off under
// actions.mode=auto exactly like instant recall: a poisoned KB entry must never
// shape a prompt that could drive an auto-executed action.
func TestNearMissDisabledUnderAuto(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
			Args: `{"confidence":0.8,"root_causes":[{"summary":"fresh finding","confidence":0.8}]}`}}},
	}}
	pol := action.New(config.ActionPolicy{
		Mode:  config.ActionAuto,
		Allow: config.ActionAllow{ReversibleOnly: true, Namespaces: []string{"tooling"}},
	})
	li := &LoopInvestigator{
		Model:      model,
		Actions:    pol,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall:     nearMissRecall(),
		OnComplete: func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), nearMissReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	seed := model.reqs[0].Messages[0].Content
	if strings.Contains(seed, "possibly-related past incident") {
		t.Fatalf("near-miss must be disabled under actions.mode=auto; seed:\n%s", seed)
	}
}

// TestNoNearMissWhenNoStructuralAgreement proves the block is omitted when no
// candidate structurally agrees with the workload (nothing to inject).
func TestNoNearMissWhenNoStructuralAgreement(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
			Args: `{"confidence":0.8,"root_causes":[{"summary":"fresh","confidence":0.8}]}`}}},
	}}
	li := &LoopInvestigator{
		Model: model,
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Recall: &Recall{MinScore: 4.0, SoloFloor: 4.0, Catalog: fakeScored{hits: []catalog.ScoredEntry{{
			Entry: catalog.Entry{Title: "unrelated", Path: "u.md", Resource: "other-ns/other"}, Score: 0.5,
		}}}},
		OnComplete: func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), nearMissReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if strings.Contains(model.reqs[0].Messages[0].Content, "possibly-related past incident") {
		t.Fatal("no structurally-agreeing candidate must inject no near-miss block")
	}
}

// TestKBSearchQueryEnriched proves the per-investigation kb_search tool folds the
// request's normalized workload ref + alertname into the model's query server-side
// (the buildRecallQuery enrichment), and that the tool Description reflects it.
func TestKBSearchQueryEnriched(t *testing.T) {
	cap := &capturingScored{hits: []catalog.ScoredEntry{{
		Entry: catalog.Entry{Title: "runbook", Path: "r.md"}, Score: 1.0,
	}}}
	model := &scriptModel{responses: []providers.CompletionResponse{
		// Turn 1: the model searches by PLAIN symptom text (no workload terms).
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: "kb_search", Args: `{"query":"pods not ready"}`}}},
		// Turn 2: conclude.
		{ToolCalls: []providers.ToolCall{{ID: "2", Name: submitFindingsName,
			Args: `{"confidence":0.8,"root_causes":[{"summary":"x","confidence":0.8}]}`}}},
	}}
	li := &LoopInvestigator{
		Model:      model,
		Tools:      []Tool{KBSearchTool{Catalog: cap}},
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(providers.Investigation) {},
	}
	if err := li.Investigate(context.Background(), nearMissReq()); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	// The query the catalog actually saw must carry the model's symptom PLUS the
	// server-side enrichment: namespace, NORMALIZED workload name (pod-hash stripped),
	// and the alertname.
	q := cap.lastQuery
	if !strings.Contains(q, "pods not ready") {
		t.Fatalf("enriched query dropped the model's own text: %q", q)
	}
	for _, want := range []string{"tooling", "harbor-registry", "KubePodNotReady"} {
		if !strings.Contains(q, want) {
			t.Fatalf("enriched query missing %q: %q", want, q)
		}
	}
	// The pod-hash must have been normalized away (matches the controller family).
	if strings.Contains(q, "59598dbd57") {
		t.Fatalf("workload name was not normalized before enrichment: %q", q)
	}
	// The tool's Description, once bound, must advertise the enrichment.
	desc := KBSearchTool{Catalog: cap}.withEnrichment(kbSearchEnrichment(nearMissReq())).Description()
	if !strings.Contains(desc, "automatically added") {
		t.Fatalf("bound Description does not reflect enrichment: %q", desc)
	}
	// An unbound tool's Description stays the plain base (no enrichment claim).
	if strings.Contains(KBSearchTool{Catalog: cap}.Description(), "automatically added") {
		t.Fatal("unbound tool must not claim enrichment")
	}
}
