// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/provenance"
	"github.com/Smana/runlore/internal/providers"
)

func TestObservedChangesAttachProvenance(t *testing.T) {
	ctx := WithObservedChanges(context.Background())
	recordObservedChange(ctx, "https://github.com/org/infra.git", "aaa1111dead", "bbb2222beef", providers.Diff{
		Files: []providers.FileDiff{
			{Path: "apps/a/values.yaml", Patch: "@@ -1,2 +5,3 @@\n+x\n"},
			{Path: "apps/b/values.yaml", Patch: "@@ -1,2 +8,3 @@\n+x\n"},
			{Path: "apps/c/values.yaml", Patch: "@@ -1,2 +10,3 @@\n+x\n"},
			{Path: "apps/d/values.yaml", Patch: "@@ -1,2 +14,3 @@\n+x\n"},
			{Path: "apps/e/values.yaml", Patch: "@@ -1,2 +20,3 @@\n+x\n"},
			{Path: "apps/harbor/values.yaml", Patch: "@@ -40,2 +42,4 @@\n+tag: v2\n"},
		},
	})

	inv := providers.Investigation{
		RootCauses: []providers.Hypothesis{{
			Summary:   "harbor tag bump broke startup",
			ChangeRef: "flux-system/apps: aaa1111..bbb2222",
			Evidence:  []string{"apps/harbor/values.yaml bumped tag to v2"},
		}},
	}
	attachProvenance(&inv, observedChangesFrom(ctx), provenance.DefaultHostStyles("", "", ""))

	rc := inv.RootCauses[0]
	wantCompare := "https://github.com/org/infra/compare/aaa1111dead...bbb2222beef"
	if rc.ChangeURL != wantCompare {
		t.Fatalf("ChangeURL = %q, want %q", rc.ChangeURL, wantCompare)
	}
	if len(rc.Provenance) != 5 {
		t.Fatalf("len(Provenance) = %d, want 5 (capped)", len(rc.Provenance))
	}
	if rc.Provenance[0].Label != "apps/harbor/values.yaml#L42" {
		t.Errorf("Provenance[0].Label = %q, want apps/harbor/values.yaml#L42 (evidence-ranked first)", rc.Provenance[0].Label)
	}
	wantFirstURL := "https://github.com/org/infra/blob/bbb2222beef/apps/harbor/values.yaml#L42"
	if rc.Provenance[0].URL != wantFirstURL {
		t.Errorf("Provenance[0].URL = %q, want %q", rc.Provenance[0].URL, wantFirstURL)
	}
	if rc.Provenance[0].Kind != "file" {
		t.Errorf("Provenance[0].Kind = %q, want file", rc.Provenance[0].Kind)
	}
}

func TestObservedChangesSingleCommit(t *testing.T) {
	ctx := WithObservedChanges(context.Background())
	recordObservedChange(ctx, "git@gitlab.com:grp/infra.git", "", "999888777666", providers.Diff{
		Files: []providers.FileDiff{{Path: "deploy.yaml", Patch: "@@ -1 +3 @@\n+x\n"}},
	})

	inv := providers.Investigation{
		RootCauses: []providers.Hypothesis{{
			Summary:   "single commit deploy",
			ChangeRef: "commit 9998887",
		}},
	}
	attachProvenance(&inv, observedChangesFrom(ctx), nil)

	rc := inv.RootCauses[0]
	wantCommit := "https://gitlab.com/grp/infra/-/commit/999888777666"
	if rc.ChangeURL != wantCommit {
		t.Fatalf("ChangeURL = %q, want %q", rc.ChangeURL, wantCommit)
	}
	if len(rc.Provenance) != 1 || rc.Provenance[0].URL != "https://gitlab.com/grp/infra/-/blob/999888777666/deploy.yaml#L3" {
		t.Fatalf("Provenance = %+v", rc.Provenance)
	}
}

func TestObservedChangesFailClosedOnUnobservedAndUnknownHost(t *testing.T) {
	ctx := WithObservedChanges(context.Background())
	recordObservedChange(ctx, "https://github.com/org/infra.git", "aaa1111", "bbb2222", providers.Diff{
		Files: []providers.FileDiff{{Path: "a.yaml", Patch: "@@ -1 +1 @@\n+x\n"}},
	})
	recordObservedChange(ctx, "https://git.unknown.example/org/infra.git", "ccc3333", "ddd4444", providers.Diff{
		Files: []providers.FileDiff{{Path: "b.yaml", Patch: "@@ -1 +1 @@\n+x\n"}},
	})

	inv := providers.Investigation{
		RootCauses: []providers.Hypothesis{
			{Summary: "hallucinated sha", ChangeRef: "deadbeef1234"},
			{Summary: "unknown host", ChangeRef: "ccc3333..ddd4444"},
		},
	}
	attachProvenance(&inv, observedChangesFrom(ctx), provenance.DefaultHostStyles("", "", ""))

	for i, rc := range inv.RootCauses {
		if rc.ChangeURL != "" || len(rc.Provenance) != 0 {
			t.Errorf("RootCauses[%d] (%s): got ChangeURL=%q Provenance=%+v, want empty (fail closed)",
				i, rc.Summary, rc.ChangeURL, rc.Provenance)
		}
	}
}

func TestKBSearchObserveTopHitSetsSectionAnchor(t *testing.T) {
	trWith := newKBHitTracker(1.0)
	toolWith := KBSearchTool{hits: trWith}
	toolWith.observeTopHit([]catalog.ScoredEntry{{
		Score: 5.0,
		Entry: catalog.Entry{
			Path:  "entries/harbor.md",
			Title: "Harbor down",
			Body:  "## Symptom\nCrash\n\n## Resolution\nRoll back to 1.14.2.\n",
		},
	}})
	if got := trWith.top(); got == nil || got.SectionAnchor != "#resolution" {
		t.Fatalf("with ## Resolution: top = %+v, want SectionAnchor=#resolution", got)
	}

	trWithout := newKBHitTracker(1.0)
	toolWithout := KBSearchTool{hits: trWithout}
	toolWithout.observeTopHit([]catalog.ScoredEntry{{
		Score: 5.0,
		Entry: catalog.Entry{
			Path:  "entries/notes.md",
			Title: "Notes",
			Body:  "## Symptom\nCrash\n",
		},
	}})
	if got := trWithout.top(); got == nil || got.SectionAnchor != "" {
		t.Fatalf("without ## Resolution: top = %+v, want empty SectionAnchor", got)
	}
}
