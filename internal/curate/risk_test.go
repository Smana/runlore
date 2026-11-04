// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"slices"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

func TestRisks(t *testing.T) {
	marked := providers.FingerprintMarker("fp")
	dup := providers.PossibleDuplicateHeading + "\n\nflagged\n\n" + marked
	cases := []struct {
		name      string
		pr        providers.CuratedIssue
		contested map[int]bool
		verified  map[string]bool
		threats   []string
		want      []Risk
	}{
		{"verified, nothing else", providers.CuratedIssue{Number: 1, Body: marked}, nil, map[string]bool{"fp": true}, nil, nil},
		{"no observation", providers.CuratedIssue{Number: 1, Body: marked}, nil, nil, nil, []Risk{RiskUnverified}},
		{"latest failed", providers.CuratedIssue{Number: 1, Body: marked}, nil, map[string]bool{"fp": false}, nil, []Risk{RiskUnverified}},
		{"no marker", providers.CuratedIssue{Number: 1}, nil, map[string]bool{"fp": true}, nil, []Risk{RiskUnverified}},
		{"a held fingerprint of another PR", providers.CuratedIssue{Number: 1, Body: providers.FingerprintMarker("other")}, nil, map[string]bool{"fp": true}, nil, []Risk{RiskUnverified}},
		{"contested", providers.CuratedIssue{Number: 1, Body: marked}, map[int]bool{1: true}, map[string]bool{"fp": true}, nil, []Risk{RiskContested}},
		{"another PR contested", providers.CuratedIssue{Number: 1, Body: marked}, map[int]bool{2: true}, map[string]bool{"fp": true}, nil, nil},
		{"contradicts", providers.CuratedIssue{Number: 1, Body: dup}, nil, map[string]bool{"fp": true}, nil, []Risk{RiskContradicts}},
		{"security", providers.CuratedIssue{Number: 1, Body: marked, Labels: []string{"security"}}, nil, map[string]bool{"fp": true}, nil, []Risk{RiskSecurity}},
		{
			"every risk, in KP13's order",
			providers.CuratedIssue{Number: 1, Body: dup, Labels: []string{"security"}},
			map[int]bool{1: true}, nil, []string{"rule"},
			[]Risk{RiskThreat, RiskContested, RiskContradicts, RiskSecurity, RiskUnverified},
		},
		{"threat", providers.CuratedIssue{Number: 1, Body: marked}, nil, map[string]bool{"fp": true}, []string{"rule"}, []Risk{RiskThreat}},
		{"no rule hit", providers.CuratedIssue{Number: 1, Body: marked}, nil, map[string]bool{"fp": true}, []string{}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Risks(tc.pr, tc.contested, tc.verified, tc.threats); !slices.Equal(got, tc.want) {
				t.Fatalf("Risks = %v, want %v", got, tc.want)
			}
		})
	}
}

// KP13's order: conflict ranks after security and before unverified. A threat ranks above them all:
// Decide shows one risk per row, and a threat's row must say so.
func TestRiskRank(t *testing.T) {
	if RiskThreat >= RiskContested || RiskSecurity >= RiskConflict || RiskConflict >= RiskUnverified {
		t.Fatalf("threat=%d contested=%d security=%d conflict=%d unverified=%d", RiskThreat, RiskContested, RiskSecurity, RiskConflict, RiskUnverified)
	}
}

func TestRiskString(t *testing.T) {
	for r, want := range map[Risk]string{
		RiskThreat: "threat", RiskContested: "contested", RiskContradicts: "contradicts", RiskSecurity: "security", RiskConflict: "conflict", RiskUnverified: "unverified",
	} {
		if got := r.String(); got != want {
			t.Errorf("Risk(%d) = %q, want %q", int(r), got, want)
		}
	}
}
