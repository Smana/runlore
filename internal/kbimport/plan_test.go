// SPDX-License-Identifier: Apache-2.0

package kbimport

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/okf"
	"github.com/Smana/runlore/internal/providers"
)

func res(title, dest, source string) Result {
	return Result{
		Entry:    providers.KBEntry{Type: "Playbook", Title: title, Description: title, Body: "body"},
		DestPath: dest, Source: source,
	}
}

func TestPlanDedup(t *testing.T) {
	existing := []catalog.Entry{
		{Path: "playbooks/redis-failover.md", Title: "Redis failover"},
		{Path: "incidents/payments-outage.md", Title: "Payments API outage March 2024"},
	}
	retired := res("Old thing", "playbooks/old-thing.md", "old.md")
	retired.Meta = okf.Meta{Status: "retired"}
	in := []Result{
		res("Postgres vacuum tuning", "playbooks/postgres-vacuum-tuning.md", "a.md"),                   // novel → import
		res("Anything", "playbooks/redis-failover.md", "b.md"),                                         // path taken → skip
		res("Payments API outage — March 2024", "playbooks/payments-api-outage-march-2024.md", "c.md"), // fuzzy title dup → skip
		retired, // retired at source → skip
		res("Postgres vacuum tuning", "playbooks/postgres-vacuum-tuning.md", "e.md"), // batch collision → skip
	}
	got := Plan(in, existing)
	if len(got) != len(in) {
		t.Fatalf("Plan must return one action per result, got %d", len(got))
	}
	wantSkip := []struct {
		skip   bool
		reason string
	}{
		{false, ""},
		{true, "destination exists"},
		{true, "duplicate of incidents/payments-outage.md"},
		{true, "retired"},
		{true, "collides"},
	}
	for i, w := range wantSkip {
		if got[i].Skip != w.skip {
			t.Errorf("action %d (%s): skip = %v, want %v (reason %q)", i, in[i].Source, got[i].Skip, w.skip, got[i].Reason)
		}
		if w.reason != "" && !strings.Contains(got[i].Reason, w.reason) {
			t.Errorf("action %d: reason %q must mention %q", i, got[i].Reason, w.reason)
		}
	}
}
