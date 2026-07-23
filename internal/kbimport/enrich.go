// SPDX-License-Identifier: Apache-2.0

package kbimport

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/providers"
)

const submitFrontmatterName = "submit_frontmatter"

func submitFrontmatterSpec() providers.ToolSpec {
	return providers.ToolSpec{
		Name:        submitFrontmatterName,
		Description: "Submit refined OKF frontmatter for the runbook being imported.",
		Schema: `{
  "type": "object",
  "properties": {
    "title": {"type": "string", "description": "concise, specific, one line"},
    "description": {"type": "string", "description": "one-sentence summary of when this entry applies"},
    "tags": {"type": "array", "items": {"type": "string"}, "description": "lowercase recall keywords incl. alert names"},
    "type": {"type": "string", "enum": ["Incident", "Playbook", "Concept"]}
  }
}`,
	}
}

const enrichSystemPrompt = `You refine YAML frontmatter for an SRE runbook being imported into a knowledge
catalog. You are given the deterministic draft frontmatter and the document body.
Improve title/description/tags only where the draft is weak; keep them faithful
to the document — never invent facts. Answer ONLY via submit_frontmatter.`

// Enrich optionally refines a Result's frontmatter with the configured model.
// It NEVER gates (mirrors kbvalidate.ReviewSemantic): nil model, a model
// error, a missing tool call, or unusable output all return r unchanged apart
// from a warning — the deterministic Infer output is always a valid fallback.
// The model cannot set resource (passthrough-only), the title is re-capped to
// the merge gate's budget, and DestPath follows the refined type+title.
func Enrich(ctx context.Context, r Result, m providers.ModelProvider) Result {
	if m == nil {
		return r
	}
	prompt := fmt.Sprintf("Draft frontmatter:\ntype: %s\ntitle: %s\ndescription: %s\ntags: %s\n\nDocument body:\n\n%s",
		r.Entry.Type, r.Entry.Title, r.Entry.Description, strings.Join(r.Entry.Tags, ", "), r.Entry.Body)
	resp, err := m.Complete(ctx, providers.CompletionRequest{
		System:     enrichSystemPrompt,
		Messages:   []providers.Message{{Role: "user", Content: prompt}},
		Tools:      []providers.ToolSpec{submitFrontmatterSpec()},
		ToolChoice: submitFrontmatterName,
	})
	if err != nil {
		r.Warnings = append(r.Warnings, fmt.Sprintf("model enrichment skipped: %v", err))
		return r
	}
	for _, tc := range resp.ToolCalls {
		if tc.Name != submitFrontmatterName {
			continue
		}
		var raw struct {
			Title       string   `json:"title"`
			Description string   `json:"description"`
			Tags        []string `json:"tags"`
			Type        string   `json:"type"`
		}
		if err := json.Unmarshal([]byte(tc.Args), &raw); err != nil {
			r.Warnings = append(r.Warnings, fmt.Sprintf("model enrichment skipped: unparseable output: %v", err))
			return r
		}
		return apply(r, raw.Title, raw.Description, raw.Tags, raw.Type)
	}
	r.Warnings = append(r.Warnings, "model enrichment skipped: model answered without calling submit_frontmatter")
	return r
}

// apply merges model output over the deterministic result field by field,
// keeping every invariant Infer established.
func apply(r Result, title, description string, tags []string, typ string) Result {
	if t := curator.CapTitle(title); t != "" {
		r.Entry.Title = t
	}
	if d := strings.TrimSpace(description); d != "" {
		r.Entry.Description = capRunes(strings.Join(strings.Fields(d), " "), descriptionMaxRunes)
	}
	if validTypes[strings.TrimSpace(typ)] {
		// An Incident still needs a resource; without one, keep the inferred type.
		if strings.TrimSpace(typ) != "Incident" || r.Entry.Resource != "" {
			r.Entry.Type = strings.TrimSpace(typ)
		}
	}
	if len(tags) > 0 {
		r.Entry.Tags = inferTags(tags, "", r.Entry.Type) // rebuild: constant pair + model tags, deduped/capped
	}
	r.DestPath = fmt.Sprintf("%ss/%s.md", strings.ToLower(r.Entry.Type), slugOf(r.Entry.Title))
	return r
}
