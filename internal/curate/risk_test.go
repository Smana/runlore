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
		want      []Risk
	}{
		{"verified, nothing else", providers.CuratedIssue{Number: 1, Body: marked}, nil, map[string]bool{"fp": true}, nil},
		{"no observation", providers.CuratedIssue{Number: 1, Body: marked}, nil, nil, []Risk{RiskUnverified}},
		{"latest failed", providers.CuratedIssue{Number: 1, Body: marked}, nil, map[string]bool{"fp": false}, []Risk{RiskUnverified}},
		{"no marker", providers.CuratedIssue{Number: 1}, nil, map[string]bool{"fp": true}, []Risk{RiskUnverified}},
		{"a held fingerprint of another PR", providers.CuratedIssue{Number: 1, Body: providers.FingerprintMarker("other")}, nil, map[string]bool{"fp": true}, []Risk{RiskUnverified}},
		{"contested", providers.CuratedIssue{Number: 1, Body: marked}, map[int]bool{1: true}, map[string]bool{"fp": true}, []Risk{RiskContested}},
		{"another PR contested", providers.CuratedIssue{Number: 1, Body: marked}, map[int]bool{2: true}, map[string]bool{"fp": true}, nil},
		{"contradicts", providers.CuratedIssue{Number: 1, Body: dup}, nil, map[string]bool{"fp": true}, []Risk{RiskContradicts}},
		{"security", providers.CuratedIssue{Number: 1, Body: marked, Labels: []string{"security"}}, nil, map[string]bool{"fp": true}, []Risk{RiskSecurity}},
		{
			"every risk, in KP13's order",
			providers.CuratedIssue{Number: 1, Body: dup, Labels: []string{"security"}},
			map[int]bool{1: true}, nil,
			[]Risk{RiskContested, RiskContradicts, RiskSecurity, RiskUnverified},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := Risks(tc.pr, tc.contested, tc.verified); !slices.Equal(got, tc.want) {
				t.Fatalf("Risks = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestRiskString(t *testing.T) {
	for r, want := range map[Risk]string{
		RiskContested: "contested", RiskContradicts: "contradicts", RiskSecurity: "security", RiskUnverified: "unverified",
	} {
		if got := r.String(); got != want {
			t.Errorf("Risk(%d) = %q, want %q", int(r), got, want)
		}
	}
}
