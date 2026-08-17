// SPDX-License-Identifier: Apache-2.0

package thread

import (
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/kbvalidate"
)

var noteAt = time.Date(2026, 8, 14, 10, 30, 0, 0, time.UTC)

func TestNoteBodyCarriesProvenanceAndVerbatimText(t *testing.T) {
	tc := Context{Transport: "slack", Title: "ImageGalleryUnavailable", TriggerKey: "tk-1"}
	got := NoteBody(tc, "alice", "the real cause was a spot reclaim", noteAt)

	for _, want := range []string{"alice", "slack", "2026-08-14", "the real cause was a spot reclaim"} {
		if !strings.Contains(got, want) {
			t.Errorf("NoteBody missing %q:\n%s", want, got)
		}
	}
}

func TestNoteBodyNeutralisesImageMarkdown(t *testing.T) {
	got := NoteBody(Context{}, "alice", "look ![x](https://evil.example/track.png) here", noteAt)
	if strings.Contains(got, "![") {
		t.Fatalf("image markdown must be neutralised:\n%s", got)
	}
	if !strings.Contains(got, "https://evil.example/track.png") {
		t.Fatal("the URL must survive as text — neutralised, not censored")
	}
}

func TestConceptEntryPassesTheMergeGate(t *testing.T) {
	tests := []struct {
		name string
		tc   Context
	}{
		{"full context", Context{Title: "ImageGalleryUnavailable", Resource: "apps/gallery", TriggerKey: "tk-1", RecalledEntry: "incidents/foo.md"}},
		{"no resource", Context{Title: "ImageGalleryUnavailable", TriggerKey: "tk-1"}},
		{"no title", Context{TriggerKey: "tk-1"}},
		{"empty context", Context{}},
		// tc.Title comes from raw, untrusted alert text (inv.Title). A title
		// carrying an embedded newline must still clear the merge gate: nothing
		// upstream of ConceptEntry guarantees a single-line title.
		{"title with embedded newline", Context{Title: "ImageGalleryUnavailable\r\nX-Injected: header"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ConceptEntry(tt.tc, "alice", "the real cause was a spot reclaim", noteAt)
			if e.Type != "Concept" {
				t.Fatalf("Type = %q, want Concept", e.Type)
			}
			issues := kbvalidate.ValidateStructural(catalog.Entry{
				Type: e.Type, Title: e.Title, Description: e.Description,
				Resource: e.Resource, Tags: e.Tags, Body: e.Body,
			})
			for _, is := range issues {
				if is.Severity == kbvalidate.SeverityError {
					t.Errorf("entry fails the merge gate: %s: %s", is.Field, is.Message)
				}
			}
		})
	}
}

func TestConceptEntryLinksTheRecalledEntry(t *testing.T) {
	tc := Context{Title: "ImageGalleryUnavailable", RecalledEntry: "incidents/foo.md"}
	e := ConceptEntry(tc, "alice", "this resolution is stale", noteAt)
	if !strings.Contains(e.Body, "incidents/foo.md") {
		t.Fatalf("body must link the entry the note corrects:\n%s", e.Body)
	}
}

func TestConceptEntryCarriesTriggerKeyNotFingerprint(t *testing.T) {
	// The dedup fingerprint identifies a CURATED FINDING. An operator note is not
	// a finding, so stamping it would make the note collide with the real entry in
	// curator dedup and in ByFingerprint lookups.
	e := ConceptEntry(Context{TriggerKey: "tk-1", DupFingerprint: "fp-1"}, "alice", "x", noteAt)
	if e.Fingerprint != "" {
		t.Fatalf("Fingerprint = %q, want empty on an operator note", e.Fingerprint)
	}
	if e.Confidence != 0 {
		t.Fatalf("Confidence = %v, want 0 — a note carries no model confidence", e.Confidence)
	}
}

// TestConceptEntryCarriesTheNoteForgeLabel pins that every ConceptEntry PR
// carries noteForgeLabel — the label internal/curate's isOperatorNote reads
// back to exclude a standalone operator note from every auto-closing pass.
// Without it, two notes on the same recurring incident (identical "Operator
// note: <title>" PR titles, no dedup fingerprint) would be paired and closed
// by RunLore's own dedup sweep.
func TestConceptEntryCarriesTheNoteForgeLabel(t *testing.T) {
	e := ConceptEntry(Context{Title: "ImageGalleryUnavailable"}, "alice", "x", noteAt)
	found := false
	for _, l := range e.ExtraLabels {
		if l == noteForgeLabel {
			found = true
		}
	}
	if !found {
		t.Fatalf("ExtraLabels = %v, want it to contain %q", e.ExtraLabels, noteForgeLabel)
	}
}

func TestConceptEntryTitleIsBounded(t *testing.T) {
	tests := []struct {
		name  string
		title string
	}{
		{"ascii", strings.Repeat("x", 400)},
		// A repeated 3-byte CJK rune forces the truncation cut to land inside a
		// multi-byte sequence unless the isRuneStart walk correctly backs off it.
		{"multibyte rune boundary", strings.Repeat("国", 400)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			e := ConceptEntry(Context{Title: tt.title}, "alice", "y", noteAt)
			if !utf8.ValidString(e.Title) {
				t.Fatalf("title is not valid UTF-8 after truncation: %q", e.Title)
			}
			issues := kbvalidate.ValidateStructural(catalog.Entry{
				Type: e.Type, Title: e.Title, Description: e.Description, Body: e.Body,
			})
			for _, is := range issues {
				if is.Severity == kbvalidate.SeverityError {
					t.Errorf("long source title must not break the gate: %s: %s", is.Field, is.Message)
				}
			}
		})
	}
}
