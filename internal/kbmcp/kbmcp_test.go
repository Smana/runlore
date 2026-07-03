package kbmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
)

func writeEntry(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func testCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	dir := t.TempDir()
	writeEntry(t, dir, "helmrelease.md", `---
type: Playbook
title: HelmRelease upgrade failure
description: Helm chart bump leaves the release Ready=False.
tags: [flux, helmrelease, upgrade]
---
A chart bump that adds a DB migration can stall the release.
`)
	writeEntry(t, dir, "network.md", `---
type: Playbook
title: CiliumNetworkPolicy drops
description: Connectivity timeouts caused by a default-deny policy.
tags: [cilium, network]
---
Check Hubble for DROPPED verdicts.
`)
	c, err := catalog.New(dir)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	return c
}

func tool(t *testing.T, name string) func(context.Context, json.RawMessage) (string, error) {
	t.Helper()
	for _, tl := range Tools(testCatalog(t)) {
		if tl.Name == name {
			return tl.Handler
		}
	}
	t.Fatalf("tool %q not registered", name)
	return nil
}

func TestToolsMetadata(t *testing.T) {
	tools := Tools(testCatalog(t))
	if len(tools) != 2 || tools[0].Name != "kb_search" || tools[1].Name != "kb_get" {
		t.Fatalf("want [kb_search kb_get], got %+v", tools)
	}
	for _, tl := range tools {
		if tl.Description == "" {
			t.Fatalf("%s: empty description", tl.Name)
		}
		var js map[string]any
		if err := json.Unmarshal(tl.InputSchema, &js); err != nil {
			t.Fatalf("%s: schema is not valid JSON: %v", tl.Name, err)
		}
	}
}

func TestKBSearchReturnsScoredHits(t *testing.T) {
	out, err := tool(t, "kb_search")(context.Background(), json.RawMessage(`{"query":"helmrelease chart migration"}`))
	if err != nil {
		t.Fatalf("kb_search: %v", err)
	}
	var hits []struct {
		Path        string   `json:"path"`
		Type        string   `json:"type"`
		Title       string   `json:"title"`
		Description string   `json:"description"`
		Tags        []string `json:"tags"`
		Score       float64  `json:"score"`
	}
	if err := json.Unmarshal([]byte(out), &hits); err != nil {
		t.Fatalf("kb_search output is not JSON: %v\n%s", err, out)
	}
	if len(hits) == 0 || hits[0].Title != "HelmRelease upgrade failure" {
		t.Fatalf("expected the HelmRelease playbook first, got %+v", hits)
	}
	h := hits[0]
	if h.Path != "helmrelease.md" || h.Type != "Playbook" || h.Description == "" || len(h.Tags) != 3 || h.Score <= 0 {
		t.Fatalf("hit is missing fields: %+v", h)
	}
}

func TestKBSearchRequiresQuery(t *testing.T) {
	if _, err := tool(t, "kb_search")(context.Background(), json.RawMessage(`{}`)); err == nil {
		t.Fatal("missing query must error")
	}
	if _, err := tool(t, "kb_search")(context.Background(), json.RawMessage(`not json`)); err == nil {
		t.Fatal("malformed args must error")
	}
}

func TestKBSearchClampsK(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < 25; i++ {
		writeEntry(t, dir, fmt.Sprintf("e%d.md", i), fmt.Sprintf(`---
type: Concept
title: entry %d about kafka
description: kafka broker note %d
---
kafka broker lag entry %d
`, i, i, i))
	}
	c, err := catalog.New(dir)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	var search func(context.Context, json.RawMessage) (string, error)
	for _, tl := range Tools(c) {
		if tl.Name == "kb_search" {
			search = tl.Handler
		}
	}

	count := func(args string) int {
		t.Helper()
		out, err := search(context.Background(), json.RawMessage(args))
		if err != nil {
			t.Fatalf("kb_search %s: %v", args, err)
		}
		var hits []map[string]any
		if err := json.Unmarshal([]byte(out), &hits); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return len(hits)
	}
	if n := count(`{"query":"kafka"}`); n != 5 {
		t.Fatalf("k omitted must default to 5, got %d", n)
	}
	if n := count(`{"query":"kafka","k":-3}`); n != 5 {
		t.Fatalf("k<=0 must default to 5, got %d", n)
	}
	if n := count(`{"query":"kafka","k":100}`); n != 20 {
		t.Fatalf("k must cap at 20, got %d", n)
	}
}

func TestKBGetReturnsFullEntry(t *testing.T) {
	out, err := tool(t, "kb_get")(context.Background(), json.RawMessage(`{"path":"helmrelease.md"}`))
	if err != nil {
		t.Fatalf("kb_get: %v", err)
	}
	var e struct {
		Path  string   `json:"path"`
		Type  string   `json:"type"`
		Title string   `json:"title"`
		Tags  []string `json:"tags"`
		Body  string   `json:"body"`
	}
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("kb_get output is not JSON: %v\n%s", err, out)
	}
	if e.Type != "Playbook" || e.Title != "HelmRelease upgrade failure" || !strings.Contains(e.Body, "DB migration") {
		t.Fatalf("entry incomplete: %+v", e)
	}
}

func TestKBGetRejectsBadPaths(t *testing.T) {
	for _, args := range []string{
		`{}`,                        // missing path
		`{"path":"../secrets.md"}`,  // traversal
		`{"path":"/etc/passwd"}`,    // absolute
		`{"path":"nonexistent.md"}`, // unknown entry
	} {
		if _, err := tool(t, "kb_get")(context.Background(), json.RawMessage(args)); err == nil {
			t.Fatalf("args %s must error", args)
		}
	}
}
