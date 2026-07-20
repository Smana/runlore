// SPDX-License-Identifier: Apache-2.0

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
timestamp: "2026-06-20T00:00:00Z"
fingerprint: deadbeefcafebabe
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
		Path        string   `json:"path"`
		Type        string   `json:"type"`
		Title       string   `json:"title"`
		Tags        []string `json:"tags"`
		Timestamp   string   `json:"timestamp"`
		Fingerprint string   `json:"fingerprint"`
		Body        string   `json:"body"`
	}
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("kb_get output is not JSON: %v\n%s", err, out)
	}
	if e.Type != "Playbook" || e.Title != "HelmRelease upgrade failure" || !strings.Contains(e.Body, "DB migration") {
		t.Fatalf("entry incomplete: %+v", e)
	}
	// Curated entries carry a change stamp + dedup identity in frontmatter;
	// kb_get must surface both so clients can judge freshness and identity.
	if e.Timestamp != "2026-06-20T00:00:00Z" || e.Fingerprint != "deadbeefcafebabe" {
		t.Fatalf("timestamp/fingerprint not surfaced: %+v", e)
	}

	// Hand-written entries have neither — the keys must be omitted, not "".
	out, err = tool(t, "kb_get")(context.Background(), json.RawMessage(`{"path":"network.md"}`))
	if err != nil {
		t.Fatalf("kb_get network.md: %v", err)
	}
	if strings.Contains(out, `"timestamp"`) || strings.Contains(out, `"fingerprint"`) {
		t.Fatalf("absent timestamp/fingerprint must be omitted:\n%s", out)
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

// TestKBSurfacesStatus: retired/draft entries stay searchable and fetchable BY
// DESIGN — MCP consumers doing KB archaeology see the lifecycle state and judge for
// themselves; recall is where the firing ban lives. status/last_validated are
// surfaced when present, omitted (not "") when absent.
func TestKBSurfacesStatus(t *testing.T) {
	dir := t.TempDir()
	writeEntry(t, dir, "retired.md", `---
type: Incident
title: retired harbor incident
description: superseded runbook
resource: tooling/harbor
status: retired
last_validated: 2026-01-10
---
Old body.
`)
	writeEntry(t, dir, "active.md", `---
type: Playbook
title: active playbook current guidance
description: current guidance
---
Body.
`)
	c, err := catalog.New(dir)
	if err != nil {
		t.Fatalf("catalog.New: %v", err)
	}
	handler := func(name string) func(context.Context, json.RawMessage) (string, error) {
		for _, tl := range Tools(c) {
			if tl.Name == name {
				return tl.Handler
			}
		}
		t.Fatalf("tool %q not registered", name)
		return nil
	}

	// kb_search surfaces the retired status (retired knowledge stays findable).
	out, err := handler("kb_search")(context.Background(), json.RawMessage(`{"query":"retired harbor incident"}`))
	if err != nil {
		t.Fatalf("kb_search: %v", err)
	}
	if !strings.Contains(out, `"status": "retired"`) {
		t.Fatalf("kb_search must surface the retired status:\n%s", out)
	}

	// kb_get surfaces status + last_validated.
	out, err = handler("kb_get")(context.Background(), json.RawMessage(`{"path":"retired.md"}`))
	if err != nil {
		t.Fatalf("kb_get: %v", err)
	}
	var e struct {
		Status        string `json:"status"`
		LastValidated string `json:"last_validated"`
	}
	if err := json.Unmarshal([]byte(out), &e); err != nil {
		t.Fatalf("kb_get output is not JSON: %v\n%s", err, out)
	}
	if e.Status != "retired" || e.LastValidated != "2026-01-10" {
		t.Fatalf("status/last_validated not surfaced: %+v", e)
	}

	// An entry without the fields omits the keys entirely (not "").
	out, err = handler("kb_get")(context.Background(), json.RawMessage(`{"path":"active.md"}`))
	if err != nil {
		t.Fatalf("kb_get active.md: %v", err)
	}
	if strings.Contains(out, `"status"`) || strings.Contains(out, `"last_validated"`) {
		t.Fatalf("absent status/last_validated must be omitted:\n%s", out)
	}
}
