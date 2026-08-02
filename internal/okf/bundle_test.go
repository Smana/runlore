// SPDX-License-Identifier: Apache-2.0

package okf

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func bundleEntry() providers.KBEntry {
	return providers.KBEntry{
		Type: "Incident", Title: "Harbor down",
		Description: "valkey down", Fingerprint: "deadbeefcafebabe",
	}
}

func TestUpdateIndexAppendsToTypeSection(t *testing.T) {
	existing := `---
okf_version: "0.1"
type: Index
---
# Catalog

## Playbooks

- [HelmRelease upgrade failure](playbooks/helmrelease.md)

## Incidents

- [Old incident](incidents/old.md)
`
	got := string(UpdateIndex([]byte(existing), bundleEntry(), "incidents/harbor-down-deadbeef.md"))
	want := "- [Harbor down](incidents/harbor-down-deadbeef.md) — valkey down"
	if !strings.Contains(got, want) {
		t.Fatalf("index missing %q:\n%s", want, got)
	}
	// The new line must land inside the Incidents section, not after Playbooks.
	if strings.Index(got, want) < strings.Index(got, "## Incidents") {
		t.Fatalf("entry landed outside its type section:\n%s", got)
	}
	// The existing Playbooks section must be untouched.
	if !strings.Contains(got, "- [HelmRelease upgrade failure](playbooks/helmrelease.md)") {
		t.Fatalf("existing sections must be preserved:\n%s", got)
	}
}

func TestUpdateIndexCreatesMissingSection(t *testing.T) {
	existing := "# Catalog\n\n## Playbooks\n\n- [P](p.md)\n"
	got := string(UpdateIndex([]byte(existing), bundleEntry(), "incidents/h.md"))
	if !strings.Contains(got, "## Incidents") {
		t.Fatalf("missing new ## Incidents section:\n%s", got)
	}
	if !strings.Contains(got, "- [Harbor down](incidents/h.md) — valkey down") {
		t.Fatalf("missing entry line:\n%s", got)
	}
}

func TestUpdateLogCreatesAndPrepends(t *testing.T) {
	// No log yet → a fresh OKF log: H1 title, newest-first date heading, bold
	// action word.
	got := string(UpdateLog(nil, bundleEntry(), "incidents/h.md", "2026-07-03"))
	for _, want := range []string{"# ", "## 2026-07-03", "* **Creation**: Added [Harbor down](incidents/h.md)."} {
		if !strings.Contains(got, want) {
			t.Fatalf("fresh log missing %q:\n%s", want, got)
		}
	}

	// Existing log with an older date → the new date heading goes FIRST (newest
	// first), older entries preserved below.
	existing := "# Catalog update log\n\n## 2026-06-20\n\n* **Creation**: Added [Old](o.md).\n"
	got = string(UpdateLog([]byte(existing), bundleEntry(), "incidents/h.md", "2026-07-03"))
	i, j := strings.Index(got, "## 2026-07-03"), strings.Index(got, "## 2026-06-20")
	if i < 0 || j < 0 || i > j {
		t.Fatalf("dates must be newest-first:\n%s", got)
	}

	// Same-day second entry → reuse the existing date heading, no duplicate.
	got = string(UpdateLog([]byte(got), bundleEntry(), "incidents/h2.md", "2026-07-03"))
	if strings.Count(got, "## 2026-07-03") != 1 {
		t.Fatalf("same-day entries must share one date heading:\n%s", got)
	}
	if !strings.Contains(got, "(incidents/h2.md)") {
		t.Fatalf("second same-day entry missing:\n%s", got)
	}
}
