// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// recordingDecider keeps the state it was asked about, so a test can read what the
// decider was actually shown rather than what the caller meant to show it.
type recordingDecider struct {
	noul  float64
	state string
}

func (r *recordingDecider) Decide(_ context.Context, state string, qs []providers.Question) (providers.Answers, error) {
	r.state = state
	return providers.Answers{qs[0].ID: {Noul: r.noul}}, nil
}

// TestDedupDeciderIsShownBothResources pins what the dedup decider is asked against
// what it is shown. The question is "the same RESOURCE failing the same way for the
// same reason", but the state carried the finding's resource only through Fingerprint
// and the entry's not at all — title and description — so the decider could not
// answer the resource half from the data it was sent. A same-symptom finding on a
// different workload then read as a confident duplicate, the skip tier dropped a
// novel finding, and a Confirm was recorded against the wrong entry.
func TestDedupDeciderIsShownBothResources(t *testing.T) {
	hit := catalog.ScoredEntry{
		Entry: catalog.Entry{
			Path: "incidents/harbor-db-migration-lock.md", Title: "harbor-db migration lock",
			Description:   "CrashLoopBackOff on harbor-db while a schema migration holds the lock",
			Resource:      "harbor/harbor-db",
			AlertResource: "harbor/harbor",
		},
		Score: 0.49,
	}
	dec := &recordingDecider{noul: 0.20}
	c := newCurator(&fakeForge{}, multiScored{hits: []catalog.ScoredEntry{hit}})
	c.Decider = dec
	c.DedupSkipAbove, c.DedupAnnotateAbove = 0.85, 0.60

	inv := goodFinding()
	inv.Resource = providers.Workload{Kind: "Pod", Namespace: "harbor", Name: "harbor-core-59598dbd57-ltkzw"}
	if _, err := c.Curate(context.Background(), inv); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if dec.state == "" {
		t.Fatal("the decider was never asked")
	}
	// The finding's resource, normalized the way the drafted entry writes it so the two
	// sides are comparable; and the entry's, both of its recall indexes.
	for _, want := range []string{"harbor/harbor-core", hit.Entry.Resource, hit.Entry.AlertResource} {
		if !strings.Contains(dec.state, want) {
			t.Errorf("decider state lacks %q:\n%s", want, dec.state)
		}
	}
	if strings.Contains(dec.state, "59598dbd57") {
		t.Errorf("decider state carries the finding's volatile pod hash, which no entry ever names:\n%s", dec.state)
	}
}
