// SPDX-License-Identifier: Apache-2.0

package eval

import (
	"strings"
	"testing"
)

// TestAnUnknownSourceGroupFailsLoudly pins the fix for a scoring regression that ran
// silently.
//
// A coverage group name is an untyped string shared between Go and YAML. The group was
// renamed "aws" -> "cloud" in toolSource — the right call, since the group names the LENS
// and its backing provider is an operator's choice — but two scenario files and the rubric
// kept saying "aws". Nothing validated the name at either end, so ScoreCoverage could
// never see group "aws" again: the one scenario that exists to exercise the cloud lens
// scored Ratio 0.0 with Missing=[aws] on every run, while the tools it names were being
// used correctly.
//
// Failing at LOAD is the point. A permanent silent zero in a coverage column is
// indistinguishable from a genuinely uncovered lens.
func TestAnUnknownSourceGroupFailsLoudly(t *testing.T) {
	err := ValidateSourceGroups("expected_sources", []string{"aws"})
	if err == nil {
		t.Fatal(`"aws" validated, so a renamed group can still score zero forever`)
	}
	// The message has to name the valid values, or the next thing an author writes is
	// another guess.
	for _, want := range []string{"aws", "expected_sources", "cloud"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error does not mention %q: %v", want, err)
		}
	}
	if err := ValidateSourceGroups("expected_sources", []string{"cloud", "gitops", "kubernetes"}); err != nil {
		t.Errorf("real groups were rejected: %v", err)
	}
}

// TestSourceGroupsIsDerivedFromTheToolMap: the valid set and the heatmap columns both
// come from toolSource, so a rename cannot leave a second hand-maintained copy behind.
func TestSourceGroupsIsDerivedFromTheToolMap(t *testing.T) {
	groups := SourceGroups()
	seen := map[string]bool{}
	for _, g := range groups {
		if seen[g] {
			t.Errorf("SourceGroups returned %q twice", g)
		}
		seen[g] = true
	}
	for tool, group := range toolSource {
		if group == "" {
			continue
		}
		if !seen[group] {
			t.Errorf("group %q (from tool %q) is not in SourceGroups", group, tool)
		}
	}
	if seen["aws"] {
		t.Error(`"aws" is still a group; the rename to "cloud" is incomplete`)
	}
	if !seen["cloud"] {
		t.Error(`"cloud" is not a group, so the cloud lens cannot be scored`)
	}
}

// TestTheShippedScenariosNameOnlyRealGroups is the regression test for the two files that
// were actually wrong. It loads them exactly as the runner does.
func TestTheShippedScenariosNameOnlyRealGroups(t *testing.T) {
	scns, err := LoadScenarios("../../eval/scenarios")
	if err != nil {
		t.Fatalf("the shipped scenarios do not load, which now includes source-group "+
			"validation: %v", err)
	}
	if len(scns) == 0 {
		t.Fatal("no scenarios loaded, so this test proves nothing")
	}
}
