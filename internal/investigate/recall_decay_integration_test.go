// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/outcome"
	"github.com/Smana/runlore/internal/providers"
)

// TestRecallDecayGateRejectsLowOutcome is an end-to-end test of Gate 3 (outcome decay)
// over the REAL Catalog + Recall stack (not the unit test on outcomeFactor). It seeds a
// one-entry catalog whose stored resource agrees with the incident workload and clears
// the structural + margin gates — proven by the healthy-history sub-case, where recall
// FIRES. It then supplies a ledger history of 4 recalls / 0 resolves for that same
// entry: the outcome factor drops below the 0.5 floor, so recall must be REJECTED and
// fall through to a fresh investigation.
//
// The 4-recalls/0-resolves history is deliberately below the floor under BOTH the
// current outcomeFactor formula (resolved+k)/(recalls+k) = 2/6 = 0.33 and the pending
// Beta(1,1) variant (resolved+k/2)/(recalls+k) = 1/6 = 0.17 — so this test is stable
// across that parallel change and pins the decay behaviour, not a specific formula.
func TestRecallDecayGateRejectsLowOutcome(t *testing.T) {
	dir := t.TempDir()
	entry := `---
type: Incident
title: worker OOMKilled after memory limit drop
description: apps/worker pods OOMKilled; raise the container memory limit
resource: apps/worker
tags: [oom, memory, worker]
---

# Symptom
apps/worker pods are OOMKilled shortly after a values change lowered the memory limit.
`
	if err := os.WriteFile(filepath.Join(dir, "worker-oom.md"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(dir)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	path := cat.Entries()[0].Path // ledger keys by the entry's catalog Path

	req := Request{
		Title:    "WorkerOOM",
		Message:  "apps/worker pods OOMKilled after memory limit drop",
		Workload: providers.Workload{Namespace: "apps", Name: "worker"},
	}
	// Gates tuned low (tiny single-entry catalog → small BM25 scores) so the test
	// isolates Gate 3; the structural gate still requires the workload to agree.
	newRecall := func(o OutcomeStats) *Recall {
		return &Recall{
			Catalog:      cat,
			MinScore:     0.001,
			SoloFloor:    0.001,
			MarginGap:    0.001,
			Outcome:      o,
			OutcomePrior: 2.0,
			OutcomeFloor: 0.5,
		}
	}

	// Baseline: healthy history (recalls==resolves) → factor 1.0 → recall FIRES. This
	// proves gates 1 (structural) and 2 (margin) pass, so any rejection below is Gate 3.
	healthy := newRecall(fakeOutcome{counts: map[string]outcome.Aggregate{path: {Recalls: 5, Resolved: 5}}})
	if e, conf := healthy.lookup(context.Background(), req); e == nil {
		t.Fatalf("baseline recall must FIRE (gates 1+2 pass, healthy outcome); got nil")
	} else if conf <= 0 {
		t.Fatalf("fired recall must carry a positive confidence, got %v", conf)
	}

	// Decayed: 4 recalls / 0 resolves → factor below the 0.5 floor → Gate 3 REJECTS.
	decayed := newRecall(fakeOutcome{counts: map[string]outcome.Aggregate{path: {Recalls: 4, Resolved: 0}}})
	if e, _ := decayed.lookup(context.Background(), req); e != nil {
		t.Fatalf("decayed recall must be REJECTED by the outcome-decay gate, but fired: %+v", e)
	}

	// Guard the sub-floor premise under both formulas, so the fixture stays valid if the
	// parallel outcomeFactor change lands.
	if f := outcomeFactor(4, 0, 0, 0, 0, 2.0); f >= 0.5 {
		t.Fatalf("fixture invalid: outcomeFactor(4,0,0,0,0,2)=%v is not below the 0.5 floor", f)
	}
}

// TestRecallRecoversAfterConfirmations pins the 👎 recovery contract end to end
// over the real Recall gate: one standing 👎 rejects the recall; one machine
// confirmation still rejects (human outranks a single confirm); the second
// confirmation recovers it.
func TestRecallRecoversAfterConfirmations(t *testing.T) {
	dir := t.TempDir()
	entry := `---
type: Incident
title: worker OOMKilled after memory limit drop
description: apps/worker pods OOMKilled; raise the container memory limit
resource: apps/worker
---

# Symptom
apps/worker pods are OOMKilled shortly after a values change lowered the memory limit.
`
	if err := os.WriteFile(filepath.Join(dir, "worker-oom.md"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}
	cat, err := catalog.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	path := cat.Entries()[0].Path
	req := Request{Title: "WorkerOOM", Message: "apps/worker pods OOMKilled after memory limit drop",
		Workload: providers.Workload{Namespace: "apps", Name: "worker"}}
	newRecall := func(agg outcome.Aggregate) *Recall {
		return &Recall{Catalog: cat, MinScore: 0.001, SoloFloor: 0.001, MarginGap: 0.001,
			Outcome:      fakeOutcome{counts: map[string]outcome.Aggregate{path: agg}},
			OutcomePrior: 2.0, OutcomeFloor: 0.5}
	}
	if e, _ := newRecall(outcome.Aggregate{FeedbackDown: 1}).lookup(context.Background(), req); e != nil {
		t.Fatal("one standing 👎 must reject the recall")
	}
	if e, _ := newRecall(outcome.Aggregate{FeedbackDown: 1, Confirms: 1}).lookup(context.Background(), req); e != nil {
		t.Fatal("one machine confirmation must NOT overcome a human 👎")
	}
	if e, _ := newRecall(outcome.Aggregate{FeedbackDown: 1, Confirms: 2}).lookup(context.Background(), req); e == nil {
		t.Fatal("two confirmations must recover the recall (factor back at the floor)")
	}
}
