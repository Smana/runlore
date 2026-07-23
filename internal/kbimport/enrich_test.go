// SPDX-License-Identifier: Apache-2.0

package kbimport

import (
	"context"
	"errors"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// fakeModel returns a canned response (or error) for the single Complete call.
type fakeModel struct {
	resp providers.CompletionResponse
	err  error
}

func (f fakeModel) Complete(_ context.Context, _ providers.CompletionRequest) (providers.CompletionResponse, error) {
	return f.resp, f.err
}

func TestEnrichAppliesModelFields(t *testing.T) {
	r := res("pg vacuum tuning", "playbooks/pg-vacuum-tuning.md", "a.md")
	m := fakeModel{resp: providers.CompletionResponse{ToolCalls: []providers.ToolCall{{
		Name: "submit_frontmatter",
		Args: `{"title":"Postgres autovacuum tuning","description":"tune autovacuum thresholds before bloat","tags":["postgres","autovacuum"],"type":"Playbook"}`,
	}}}}
	got := Enrich(context.Background(), r, m)
	if got.Entry.Title != "Postgres autovacuum tuning" {
		t.Fatalf("title = %q", got.Entry.Title)
	}
	if got.Entry.Description != "tune autovacuum thresholds before bloat" {
		t.Fatalf("description = %q", got.Entry.Description)
	}
	if got.DestPath != "playbooks/postgres-autovacuum-tuning.md" {
		t.Fatalf("DestPath must follow the new title, got %q", got.DestPath)
	}
	want := map[string]bool{"imported": true, "playbook": true, "postgres": true, "autovacuum": true}
	for _, tag := range got.Entry.Tags {
		if !want[tag] {
			t.Fatalf("unexpected tag %q in %v", tag, got.Entry.Tags)
		}
	}
}

func TestEnrichNeverGates(t *testing.T) {
	r := res("pg vacuum tuning", "playbooks/pg-vacuum-tuning.md", "a.md")
	for name, m := range map[string]providers.ModelProvider{
		"nil model":    nil,
		"model error":  fakeModel{err: errors.New("boom")},
		"no tool call": fakeModel{resp: providers.CompletionResponse{Text: "prose"}},
		"bad json":     fakeModel{resp: providers.CompletionResponse{ToolCalls: []providers.ToolCall{{Name: "submit_frontmatter", Args: "{"}}}},
		"invalid type": fakeModel{resp: providers.CompletionResponse{ToolCalls: []providers.ToolCall{{Name: "submit_frontmatter", Args: `{"type":"Postmortem"}`}}}},
	} {
		got := Enrich(context.Background(), r, m)
		if got.Entry.Title != r.Entry.Title || got.Entry.Type != r.Entry.Type || got.DestPath != r.DestPath {
			t.Fatalf("%s: enrichment must fall back to the deterministic result, got %+v", name, got.Entry)
		}
	}
}

func TestEnrichNeverInventsResource(t *testing.T) {
	r := res("t", "playbooks/t.md", "a.md")
	m := fakeModel{resp: providers.CompletionResponse{ToolCalls: []providers.ToolCall{{
		Name: "submit_frontmatter", Args: `{"resource":"prod/db"}`,
	}}}}
	if got := Enrich(context.Background(), r, m); got.Entry.Resource != "" {
		t.Fatalf("model must not set resource, got %q", got.Entry.Resource)
	}
}
