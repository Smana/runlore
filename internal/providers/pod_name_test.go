// SPDX-License-Identifier: Apache-2.0

package providers_test

import (
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestNormalizePodNameFoldsStatefulSetOrdinal pins the rule NormalizeWorkloadName
// deliberately does NOT apply (#513): a StatefulSet pod is <name>-<ordinal>, and for
// a name KNOWN to be a Pod's that ordinal is a per-replica suffix like a pod hash.
// Only the Kind can say so — the same shape on a kind-less name is the name
// (aurora-serverless-postgres-old-1 and -2 are two databases), which is why the
// kind-less TestNormalizeWorkloadName keeps "vmagent-vmagent-0" and this does not.
//
// Exactly ONE ordinal is folded, not a fixed point: a StatefulSet named sts-0 has
// pods sts-0-N, and folding to a fixed point would erase the StatefulSet's own name.
// The function is therefore NOT idempotent on such a family, and the recall gate
// compares an entry both as written and folded once rather than folding blindly.
func TestNormalizePodNameFoldsStatefulSetOrdinal(t *testing.T) {
	cases := map[string]string{
		"vmagent-vmagent-0":  "vmagent-vmagent",
		"vmagent-vmagent-1":  "vmagent-vmagent",
		"vmagent-vmagent-12": "vmagent-vmagent",
		"vmagent-vmagent":    "vmagent-vmagent", // the StatefulSet itself
		"sts-0-3":            "sts-0",           // one ordinal: the family keeps its own digit

		// Everything NormalizeWorkloadName already folds still folds, first.
		"web-7d9c8b6f5-abcde":                       "web",
		"harbor-registry-59598dbd57-ltkzw":          "harbor-registry",
		"github-teams-sync-aqemia-29787720-3-mdft8": "github-teams-sync-aqemia", // Indexed CronJob pod

		// A strip must never leave debris — the same guard as every other rule.
		"x-0": "x-0",
		"-0":  "-0",
		"":    "",
	}
	for in, want := range cases {
		if got := providers.NormalizePodName(in); got != want {
			t.Errorf("NormalizePodName(%q) = %q, want %q", in, got, want)
		}
	}
}
