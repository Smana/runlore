// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func TestNewPayloadMapping(t *testing.T) {
	inv := providers.Investigation{
		Title:            "CrashLoopBackOff payments",
		Confidence:       0.72,
		Resource:         providers.Workload{Namespace: "payments", Name: "api"},
		Verdict:          providers.VerdictActionSuggested,
		StartedAt:        time.Date(2026, 7, 20, 10, 0, 0, 0, time.UTC),
		Prior:            &providers.PriorKnowledge{Cause: "bad rollout", Resolution: "rollback", EntryPath: "e.md", Recalls: 3, Resolved: 2},
		MatchedKnowledge: &providers.MatchedEntry{Path: "m.md", Title: "seen", URL: "u", Score: 0.9},
	}
	p := NewPayload(inv)
	if p.Title != inv.Title || p.Namespace != "payments" || p.Resource != "api" {
		t.Errorf("identity fields: %+v", p)
	}
	if p.StartedAt != "2026-07-20T10:00:00Z" {
		t.Errorf("StartedAt = %q, want RFC3339", p.StartedAt)
	}
	if p.Prior == nil || p.Prior.Cause != "bad rollout" || p.Prior.Recalls != 3 {
		t.Errorf("Prior = %+v", p.Prior)
	}
	// The shared Format-text guard: matched knowledge is suppressed when Prior is set,
	// so the structured field never disagrees with the rendered text (webhook.go:98-104).
	if p.MatchedKnowledge != nil {
		t.Errorf("MatchedKnowledge must be nil when Prior != nil, got %+v", p.MatchedKnowledge)
	}
	if p.Text == "" {
		t.Error("Text must carry Format(inv)")
	}

	inv.Prior = nil
	if q := NewPayload(inv); q.MatchedKnowledge == nil || q.MatchedKnowledge.Path != "m.md" {
		t.Errorf("MatchedKnowledge must surface when Prior == nil, got %+v", q.MatchedKnowledge)
	}
	if q := NewPayload(providers.Investigation{}); q.StartedAt != "" {
		t.Errorf("zero StartedAt must render empty, got %q", q.StartedAt)
	}
}
