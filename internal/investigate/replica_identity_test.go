// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestResourceAgreesAcrossStatefulSetReplicas is #513 on the READ side. Live: 56
// recall evaluations over 7 days, 0 accepted, 38 of them no_resource_match — a KB
// holding observability/vmagent-vmagent-0 and -1 as two entries that an incident on
// -2 could match neither of. A pod-scoped request folds its replica ordinal, and an
// entry is read both as written and folded once, so an entry filed under a sibling
// replica (legacy) and one filed under the StatefulSet (new) both agree.
//
// The Kind is the evidence: a kind-less request keeps its ordinal, so two databases
// that differ only by a trailing digit stay two resources.
func TestResourceAgreesAcrossStatefulSetReplicas(t *testing.T) {
	pod := func(name string) providers.Workload {
		return providers.Workload{Kind: "Pod", Namespace: "observability", Name: name}
	}
	cases := []struct {
		name      string
		reqW      providers.Workload
		entry     string
		requireWL bool
		want      matchStrength
	}{
		{"a replica matches an entry filed under a sibling replica",
			pod("vmagent-vmagent-2"), "observability/vmagent-vmagent-0", false, matchExact},
		{"a replica matches an entry filed under the StatefulSet",
			pod("vmagent-vmagent-2"), "observability/vmagent-vmagent", false, matchExact},
		{"strict mode agrees too — it is one workload",
			pod("vmagent-vmagent-2"), "observability/vmagent-vmagent-0", true, matchExact},
		{"a StatefulSet whose own name ends in a digit is not folded past itself",
			pod("shard-1-0"), "observability/shard-1", false, matchExact},
		{"the same replica in another namespace is another workload",
			pod("vmagent-vmagent-2"), "other/vmagent-vmagent-0", false, matchNone},
		{"a replica does not agree with a different StatefulSet",
			pod("vmagent-vmagent-2"), "observability/vmalert-vmalert-0", false, matchNone},
		{"without a Kind the ordinal is the name: two databases stay two",
			providers.Workload{Namespace: "observability", Name: "aurora-serverless-postgres-old-1"},
			"observability/aurora-serverless-postgres-old-2", false, matchNone},
		{"without a Kind a numbered name matches only itself",
			providers.Workload{Namespace: "observability", Name: "vmagent-vmagent-2"},
			"observability/vmagent-vmagent-0", false, matchNone},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := resourceAgrees(c.reqW, c.entry, c.requireWL); got != c.want {
				t.Errorf("resourceAgrees(%+v, %q, %v) = %v, want %v", c.reqW, c.entry, c.requireWL, got, c.want)
			}
		})
	}
}
