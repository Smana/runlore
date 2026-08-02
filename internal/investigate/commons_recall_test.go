// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
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
