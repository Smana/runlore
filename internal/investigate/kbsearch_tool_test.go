// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
)

type fakeCatalog struct{ entries []catalog.Entry }

func (f fakeCatalog) Search(_ string, _ int) ([]catalog.Entry, error) { return f.entries, nil }

func TestKBSearchTool(t *testing.T) {
	c := fakeCatalog{entries: []catalog.Entry{
		{Title: "HelmRelease upgrade failure", Description: "chart bump stalls", Tags: []string{"flux"}, Path: "helmrelease.md"},
	}}
	tool := KBSearchTool{Catalog: c}
	if tool.Name() != "kb_search" {
		t.Fatalf("name = %q", tool.Name())
	}
	out, err := tool.Call(context.Background(), `{"query":"harbor helmrelease"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "HelmRelease upgrade failure") || !strings.Contains(out, "helmrelease.md") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}
