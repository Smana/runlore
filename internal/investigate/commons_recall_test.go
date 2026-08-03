// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// TestCommonsEntryNeverFiresRecall pins the guarantee four separate surfaces
// promise — the concepts docs, values-full.yaml, the config comment, and the
// startup log line all state that a commons entry can never fire instant recall.
// Until this test existed, none of them was enforced by code.
//
// The reasoning everywhere was "a resource-less entry can never agree with a
// workload-carrying alert". True, but incomplete: resourceAgrees has a
// matchScopeless tier for requests carrying NO workload at all — PagerDuty, a
// generic webhook, Grafana without Kubernetes labels — and a resource-less
// commons entry agrees with exactly those. A generic playbook could therefore
// short-circuit a real incident with textbook advice, which is the one outcome
// the commons was designed never to produce.
//
// The request here deliberately carries no workload. Every pre-existing commons
// test used a workload-carrying request, so all of them passed with no guard at all.
func TestCommonsEntryNeverFiresRecall(t *testing.T) {
	own := t.TempDir()
	commons := t.TempDir()
	// A commons entry that matches the incident text as strongly as possible —
	// if provenance is not checked, this is exactly what wins.
	if err := os.WriteFile(filepath.Join(commons, "crashloop.md"), []byte(
		"---\ntype: Playbook\ntitle: Pod in CrashLoopBackOff after a deploy\n"+
			"description: container exits non-zero and is restarted with growing backoff\n"+
			"tags: [crashloop, deploy, KubePodCrashLooping]\n---\n"+
			"# Symptom\n\nCrashLoopBackOff after a deploy.\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cat := catalog.NewEmpty()
	cat.SetCommonsDir(commons)
	if _, err := cat.ReloadContext(context.Background(), own); err != nil {
		t.Fatal(err)
	}
	if cat.Len() != 1 {
		t.Fatalf("fixture: want the commons entry indexed, got %d entries", cat.Len())
	}

	r := &Recall{Catalog: cat, MinScore: 0.01, SoloFloor: 0.01}
	// A workload-LESS request: this is the scopeless tier, and it is the whole point.
	req := Request{
		Title:   "Pod in CrashLoopBackOff after a deploy",
		Message: "container exits non-zero and is restarted with growing backoff",
	}
	entry, score := r.lookup(context.Background(), req)
	if entry != nil {
		t.Fatalf("a COMMONS entry fired instant recall on a workload-less request: path=%q commons=%v score=%.2f\n"+
			"commons entries ground kb_search; they must never answer an incident",
			entry.Path, entry.Commons, score)
	}
}

// TestEnrichmentRanksTheOperatorsEntryAboveTheCommons pins the retrieval policy
// documented in website/content/docs/concepts/knowledge-commons.md ("Which corpus
// leads, and when") for APPLICATION workloads: once this platform has curated its own
// entry for the failing object, that entry outranks the commons entry for the same
// failure class.
//
// The mechanism is query enrichment, not a provenance rule. kb_search appends the
// incident's workload ref server-side (kbSearchEnrichment); enrichment knows nothing
// about provenance and lifts whichever entries contain those tokens. What makes the
// local entry the only one that CAN contain them is the object this fixture picked:
// payments/checkout-api is an application workload, and no generic playbook names it.
//
// That is a per-object property, NOT a general rule about the commons. The shipped
// playbooks name cert-manager, ArgoCD, CoreDNS and kube-system explicitly — in their
// tags and in the kubectl commands they carry — so an alert on one of THOSE workloads
// lifts the commons entry too, and it can legitimately take the top slot. Do not
// generalize this test into "enrichment can never lift a commons entry": it cannot
// here, only because of the object this fixture picked.
//
// The alertname half of the enrichment is generic, and both fixtures below carry it in
// their tags, so this test is not a strawman: the workload ref alone does the
// discriminating.
//
// The two arms are the whole test. The enriched arm alone would be vacuous if the
// fixture happened to favour the operator's entry for unrelated reasons, so the bare
// arm asserts the opposite order first: on the raw symptom query the commons entry
// actually WINS (its shorter document beats the operator's on BM25 length
// normalization). Enrichment is therefore demonstrably what flips the ranking —
// delete it and this test fails on the enriched arm.
//
// It does NOT cover the equal-score provenance tie-break; the enriched scores are far
// apart, so removing the tie-break leaves this green. That half is pinned separately
// by catalog.TestUsersOwnEntryWinsATie.
func TestEnrichmentRanksTheOperatorsEntryAboveTheCommons(t *testing.T) {
	const (
		title = "Pod in CrashLoopBackOff after a deploy"
		desc  = "container exits non-zero and is restarted with a growing backoff"
		tags  = "tags: [crashloop, deploy, KubePodCrashLooping]"
		body  = "# Symptom\n\nThe pod restarts repeatedly after a rollout.\n\n" +
			"# Checks\n\nInspect the previous container's exit code and logs.\n"
		ownPath     = "checkout-crashloop.md"
		commonsName = "crashloopbackoff.md"
		commonsPath = "commons/" + commonsName // catalog namespaces commons paths
	)
	// Identical symptom text and identical tags. The only difference that reaches the
	// index is the `resource` argument below — the line a commons entry may never carry.
	// (`type` differs too, but it is not part of the indexed text; see entryText.)
	own, commons := t.TempDir(), t.TempDir()
	write := func(dir, name, kind, resource string) {
		t.Helper()
		md := "---\ntype: " + kind + "\ntitle: " + title + "\ndescription: " + desc + "\n" +
			resource + tags + "\n---\n" + body
		if err := os.WriteFile(filepath.Join(dir, name), []byte(md), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(own, ownPath, "Incident", "resource: payments/checkout-api\n")
	write(commons, commonsName, "Playbook", "")

	cat := catalog.NewEmpty()
	cat.SetCommonsDir(commons)
	if _, err := cat.ReloadContext(context.Background(), own); err != nil {
		t.Fatal(err)
	}
	if cat.Len() != 2 {
		t.Fatalf("fixture: want both roots indexed, got %d entries", cat.Len())
	}

	// A label-derived alert: the title is the alertname, the identifying object lives
	// only in the labels — exactly the case enrichment exists for.
	req := Request{
		Title:    "KubePodCrashLooping",
		Message:  "pod is restarting",
		Workload: providers.Workload{Kind: "Deployment", Namespace: "payments", Name: "checkout-api-7d9f8b6c5d-xk2mn"},
		Labels:   map[string]string{"alertname": "KubePodCrashLooping"},
	}
	// The model searches by symptom — what the enriched tool description tells it to do.
	const args = `{"query":"crashloopbackoff after a deploy"}`
	// The two arms differ in exactly one thing: whether the tool is bound to the
	// request's enrichment. Same catalog, same model-issued query.
	tool, enrich := KBSearchTool{Catalog: cat}, kbSearchEnrichment(req)

	bare, err := tool.Call(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if rankOf(t, bare, commonsPath) > rankOf(t, bare, ownPath) {
		t.Fatalf("fixture has gone vacuous: on the UN-enriched query the commons entry must "+
			"outrank the operator's, or the enriched assertion below proves nothing about "+
			"enrichment. Re-derive the fixture.\n%s", bare)
	}

	enriched, err := tool.withEnrichment(enrich).Call(context.Background(), args)
	if err != nil {
		t.Fatal(err)
	}
	if rankOf(t, enriched, ownPath) > rankOf(t, enriched, commonsPath) {
		t.Errorf("the commons entry outranked the operator's own on an ENRICHED query.\n"+
			"enrichment = %q; those tokens name an APPLICATION workload that no generic "+
			"playbook contains, so only the local entry can match them here — a commons entry "+
			"winning this fixture means the workload ref stopped reaching the index.\n%s",
			enrich, enriched)
	}
}

// rankOf returns the position of an entry's path in rendered kb_search output.
// renderHits emits hits in ranking order, so a smaller offset means a higher rank.
func rankOf(t *testing.T, out, path string) int {
	t.Helper()
	i := strings.Index(out, path)
	if i < 0 {
		t.Fatalf("%q is missing from the kb_search output entirely:\n%s", path, out)
	}
	return i
}
