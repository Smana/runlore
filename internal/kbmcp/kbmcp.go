// SPDX-License-Identifier: Apache-2.0

// Package kbmcp exposes the OKF knowledge catalog as MCP tools (kb_search,
// kb_get) for the `lore mcp` stdio server — so any MCP client (Claude Code,
// HolmesGPT, editors) can recall RunLore's learned incident knowledge without a
// cluster or a model. Read-only by construction: handlers only query the
// in-memory catalog index.
package kbmcp

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/mcp"
)

// Search result bounds: defaultK when the caller doesn't ask, maxK as a hard cap
// so one call can't dump an entire large catalog into a model context.
const (
	defaultK = 5
	maxK     = 20
)

// searchHit is the kb_search result shape: the frontmatter card plus the BM25
// score — enough to pick an entry without pulling every body; kb_get returns the
// rest.
type searchHit struct {
	Path        string   `json:"path"`
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Resource    string   `json:"resource,omitempty"`
	Tags        []string `json:"tags,omitempty"`
	// Status is the entry's lifecycle state (retired/draft/… ; omitted when active/
	// absent). Retired entries stay searchable BY DESIGN — the consumer sees the
	// state and judges; recall is where the firing ban lives, not here.
	Status        string  `json:"status,omitempty"`
	LastValidated string  `json:"last_validated,omitempty"`
	Score         float64 `json:"score"`
}

// fullEntry is the kb_get result shape: the whole entry, body included.
// Timestamp and Fingerprint are the curated-entry provenance (last-change stamp,
// dedup identity) — surfaced so clients can judge freshness and identity; both
// are absent on hand-written entries.
type fullEntry struct {
	Path        string   `json:"path"`
	Type        string   `json:"type"`
	Title       string   `json:"title"`
	Description string   `json:"description,omitempty"`
	Resource    string   `json:"resource,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	Timestamp     string   `json:"timestamp,omitempty"`
	Fingerprint   string   `json:"fingerprint,omitempty"`
	Status        string   `json:"status,omitempty"`
	LastValidated string   `json:"last_validated,omitempty"`
	Body          string   `json:"body"`
}

// Tools returns the KB tool set backed by cat, ready for mcp.Server.AddTool.
func Tools(cat *catalog.Catalog) []mcp.Tool {
	return []mcp.Tool{
		{
			Name: "kb_search",
			Description: "Search the SRE knowledge base (BM25 over incident/playbook/concept entries). " +
				"Returns up to k scored hits with path, type, title, description, resource, and tags; " +
				"call kb_get with a hit's path for the full entry.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "query": {"type": "string", "description": "free-text search: symptom, resource, alert name, …"},
    "k": {"type": "integer", "description": "max results (default 5, cap 20)"}
  },
  "required": ["query"]
}`),
			Handler: func(_ context.Context, args json.RawMessage) (string, error) {
				return search(cat, args)
			},
		},
		{
			Name: "kb_get",
			Description: "Fetch one knowledge-base entry in full (frontmatter + markdown body) " +
				"by its bundle-relative path, as returned by kb_search.",
			InputSchema: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "bundle-relative entry path, e.g. incidents/harbor-down.md"}
  },
  "required": ["path"]
}`),
			Handler: func(_ context.Context, args json.RawMessage) (string, error) {
				return get(cat, args)
			},
		},
	}
}

func search(cat *catalog.Catalog, args json.RawMessage) (string, error) {
	var in struct {
		Query string `json:"query"`
		K     int    `json:"k"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("kb_search args: %w", err)
	}
	if in.Query == "" {
		return "", fmt.Errorf("kb_search: `query` is required")
	}
	k := in.K
	if k <= 0 {
		k = defaultK
	}
	if k > maxK {
		k = maxK
	}
	scored, err := cat.SearchScored(in.Query, k)
	if err != nil {
		return "", fmt.Errorf("kb_search: %w", err)
	}
	hits := make([]searchHit, 0, len(scored))
	for _, s := range scored {
		hits = append(hits, searchHit{
			Path: s.Entry.Path, Type: s.Entry.Type, Title: s.Entry.Title,
			Description: s.Entry.Description, Resource: s.Entry.Resource,
			Tags:   s.Entry.Tags,
			Status: s.Entry.Status, LastValidated: s.Entry.LastValidated,
			Score: s.Score,
		})
	}
	out, err := json.MarshalIndent(hits, "", "  ")
	if err != nil {
		return "", fmt.Errorf("kb_search encode: %w", err)
	}
	return string(out), nil
}

func get(cat *catalog.Catalog, args json.RawMessage) (string, error) {
	var in struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal(args, &in); err != nil {
		return "", fmt.Errorf("kb_get args: %w", err)
	}
	if in.Path == "" {
		return "", fmt.Errorf("kb_get: `path` is required")
	}
	// Lookup is by the indexed bundle-relative Path — never a filesystem read —
	// so traversal cannot escape the catalog; anything not indexed is simply
	// unknown.
	for _, e := range cat.Entries() {
		if e.Path != in.Path {
			continue
		}
		out, err := json.MarshalIndent(fullEntry{
			Path: e.Path, Type: e.Type, Title: e.Title, Description: e.Description,
			Resource: e.Resource, Tags: e.Tags,
			Timestamp: e.Timestamp, Fingerprint: e.Fingerprint,
			Status: e.Status, LastValidated: e.LastValidated, Body: e.Body,
		}, "", "  ")
		if err != nil {
			return "", fmt.Errorf("kb_get encode: %w", err)
		}
		return string(out), nil
	}
	return "", fmt.Errorf("kb_get: no entry at %q (use kb_search to discover paths)", in.Path)
}
