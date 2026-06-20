package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
)

// KBSearchTool lets the model search the OKF knowledge catalog (runbooks, past
// incidents) to ground its reasoning.
type KBSearchTool struct {
	Catalog catalog.Searcher
}

// Name returns the tool name.
func (t KBSearchTool) Name() string { return "kb_search" }

// Description returns the tool description.
func (t KBSearchTool) Description() string {
	return "Search the knowledge catalog (runbooks, past incidents) for entries relevant to a query."
}

// Schema returns the JSON schema for the arguments.
func (t KBSearchTool) Schema() string {
	return `{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`
}

// Call searches the catalog and renders the top matches.
func (t KBSearchTool) Call(_ context.Context, args string) (string, error) {
	var in struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	hits, err := t.Catalog.Search(in.Query, 3)
	if err != nil {
		return "", err
	}
	if len(hits) == 0 {
		return "no matching catalog entries", nil
	}
	var b strings.Builder
	for _, e := range hits {
		fmt.Fprintf(&b, "## %s  (%s)\n%s\n%s\n\n", e.Title, e.Path, e.Description, e.Body)
	}
	return b.String(), nil
}
