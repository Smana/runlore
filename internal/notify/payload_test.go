// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"encoding/json"
	"reflect"
	"strings"
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

// kbPayloadOmitted names every providers.KBUpdate field the webhook payload
// deliberately does NOT carry, with the reason. Stated here rather than derived,
// exactly as internal/providers states the untrusted/trusted split, so dropping a
// field from the record is a decision someone writes down.
//
// Delivery is a routing instruction for the chat sinks — where to post the
// announcement — not a fact about the write. A receiver storing the record has
// nothing to do with it, and shipping it would invite one to branch on RunLore's
// chat configuration.
var kbPayloadOmitted = map[string]string{
	"Delivery": "a chat routing instruction, not a fact about the write",
}

// TestKBUpdatePayloadCarriesEveryRecordedField is the webhook payload's version
// of the chat renderers' field guard, and it exists because this side had none.
//
// The chat announcement has a reflection test forcing every KBUpdate field to be
// rendered or explicitly withheld. The payload had only a whole-value comparison
// in the delivery test, which is structurally blind to a field the PAYLOAD TYPE
// lacks: a KBUpdate field with no counterpart here simply cannot appear in the
// expected value either, so both sides agree about a field neither carries and
// the test passes. Channel was added to the event and never reached the wire
// that way — a receiver got a thread root it could not resolve to a conversation.
//
// This walks providers.KBUpdate instead, so the direction is the one that bites:
// every field of the EVENT must be either present on the payload or listed in
// kbPayloadOmitted.
func TestKBUpdatePayloadCarriesEveryRecordedField(t *testing.T) {
	event := reflect.TypeOf(providers.KBUpdate{})
	payload := reflect.TypeOf(KBUpdatePayload{})

	for i := range event.NumField() {
		name := event.Field(i).Name
		_, omitted := kbPayloadOmitted[name]
		_, carried := payload.FieldByName(name)
		switch {
		case carried && omitted:
			t.Errorf("KBUpdatePayload carries %q while kbPayloadOmitted claims it is withheld — "+
				"one of the two is wrong", name)
		case !carried && !omitted:
			t.Errorf("providers.KBUpdate.%s reaches no webhook receiver and is not listed in "+
				"kbPayloadOmitted. Add it to KBUpdatePayload, or record why the record does not "+
				"include it — a field dropped silently is one a receiver cannot know it is missing.", name)
		}
	}
	for name := range kbPayloadOmitted {
		if _, ok := event.FieldByName(name); !ok {
			t.Errorf("kbPayloadOmitted names %q, which providers.KBUpdate no longer has — stale entry, "+
				"so nothing is checking whatever replaced it", name)
		}
	}
}

// TestNewKBUpdatePayloadPopulatesEveryFieldItDeclares closes the other half:
// a field can exist on the payload and still never be assigned. The guard above
// only compares TYPES, and NewKBUpdatePayload is a hand-written struct literal —
// exactly the shape where a new field is declared, documented, and then left at
// its zero value on the wire.
func TestNewKBUpdatePayloadPopulatesEveryFieldItDeclares(t *testing.T) {
	got := NewKBUpdatePayload(providers.KBUpdate{
		Transport: "slack", Root: "111.222", Channel: "C-ORIGIN",
		Route: providers.KBRouteOpenPR, PR: 99, URL: "https://github.com/o/r/pull/99",
		Title: "Operator note: OOM", Author: "sre-jane", ModelDrafted: true, Note: "a spot reclaim",
		At: time.Date(2026, 8, 16, 9, 0, 0, 0, time.UTC),
	})

	rv := reflect.ValueOf(got)
	for i := range rv.NumField() {
		if rv.Field(i).IsZero() {
			t.Errorf("KBUpdatePayload.%s is zero after mapping a fully populated KBUpdate — it is "+
				"declared but never assigned, so a receiver never sees it", rv.Type().Field(i).Name)
		}
	}
	if got.Channel != "C-ORIGIN" {
		t.Errorf("channel = %q, want the originating channel — root alone does not identify a "+
			"conversation, so a receiver cannot build a permalink without it", got.Channel)
	}
	// A record that says who wrote the note has to say it explicitly. With
	// omitempty, "a human wrote it" and "this producer does not report provenance"
	// are the same absent key, and the safe reading of an absent key is the wrong
	// one — so the field ships even when false.
	if b, err := json.Marshal(NewKBUpdatePayload(providers.KBUpdate{Author: "sre-jane"})); err != nil {
		t.Fatalf("marshal: %v", err)
	} else if !strings.Contains(string(b), `"model_drafted":false`) {
		t.Errorf("a human-authored note's payload omits model_drafted, so a receiver cannot tell it "+
			"from a producer that never reports provenance: %s", b)
	}
}
