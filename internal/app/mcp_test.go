package app

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
)

func testKBCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	dir := t.TempDir()
	entry := "---\ntype: Playbook\ntitle: T\ndescription: d\n---\nbody\n"
	if err := os.WriteFile(filepath.Join(dir, "t.md"), []byte(entry), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := catalog.New(dir)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	return c
}

// assembleMCPTools decides what `lore mcp` serves: the what-changed tool needs a
// GitOps provider, the KB tools need a catalog — each optional, but an empty
// server is a misconfiguration, not a working state.
func TestAssembleMCPTools(t *testing.T) {
	var gp providers.GitOpsProvider = fakeGitOps{}

	tests := []struct {
		name    string
		gitops  providers.GitOpsProvider
		catalog *catalog.Catalog
		want    []string
		wantErr bool
	}{
		{"both", gp, testKBCatalog(t), []string{"gitops_what_changed", "kb_search", "kb_get"}, false},
		{"kb only", nil, testKBCatalog(t), []string{"kb_search", "kb_get"}, false},
		{"gitops only", gp, nil, []string{"gitops_what_changed"}, false},
		{"neither", nil, nil, nil, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			tools, err := assembleMCPTools(tc.gitops, tc.catalog)
			if tc.wantErr {
				if err == nil {
					t.Fatal("want error for an empty tool set")
				}
				return
			}
			if err != nil {
				t.Fatalf("assembleMCPTools: %v", err)
			}
			var names []string
			for _, tl := range tools {
				names = append(names, tl.Name)
			}
			if len(names) != len(tc.want) {
				t.Fatalf("tools = %v, want %v", names, tc.want)
			}
			for i := range tc.want {
				if names[i] != tc.want[i] {
					t.Fatalf("tools = %v, want %v", names, tc.want)
				}
			}
		})
	}
}

// fakeGitOps is the minimal GitOpsProvider stand-in for tool assembly (the
// handler is never called here).
type fakeGitOps struct{ providers.GitOpsProvider }

var _ investigate.Tool = &investigate.WhatChangedTool{}
