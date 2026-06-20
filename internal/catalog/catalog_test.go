package catalog

import "testing"

func TestCatalogSearch(t *testing.T) {
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
tags: [cilium, network, dns]
---
Check Hubble for DROPPED verdicts.
`)

	c, err := New(dir)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if c.Len() != 2 {
		t.Fatalf("want 2 indexed, got %d", c.Len())
	}
	hits, err := c.Search("helmrelease chart migration", 3)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(hits) == 0 || hits[0].Title != "HelmRelease upgrade failure" {
		t.Fatalf("expected the HelmRelease playbook to rank first, got %v", titles(hits))
	}
}

func titles(es []Entry) []string {
	out := make([]string, len(es))
	for i, e := range es {
		out[i] = e.Title
	}
	return out
}
