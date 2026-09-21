// SPDX-License-Identifier: Apache-2.0

package providers

import "testing"

// TestWorkloadIdentityFoldsReplicaOrdinalForPodsOnly pins #513 at the identity seam
// every key and comparison reads through: a StatefulSet replica ordinal is a
// per-instance suffix, but ONLY the Kind can say so, because the same shape on a
// cloud resource or a controller is the name. Ingestion still keeps the raw pod name
// (normalization is a comparison-time concern); this is where the Kind is consulted.
func TestWorkloadIdentityFoldsReplicaOrdinalForPodsOnly(t *testing.T) {
	cases := []struct {
		name string
		w    Workload
		want string
	}{
		{"a StatefulSet pod folds to its StatefulSet",
			Workload{Kind: "Pod", Namespace: "observability", Name: "vmagent-vmagent-2"}, "vmagent-vmagent"},
		{"a Deployment pod still folds to its Deployment",
			Workload{Kind: "Pod", Namespace: "tooling", Name: "harbor-registry-59598dbd57-ltkzw"}, "harbor-registry"},
		{"a kind-less name keeps its numeric tail: two databases, not two replicas",
			Workload{Namespace: "observability", Name: "aurora-serverless-postgres-old-1"}, "aurora-serverless-postgres-old-1"},
		{"a controller whose own name ends in a digit is its own name",
			Workload{Kind: "StatefulSet", Namespace: "apps", Name: "shard-1"}, "shard-1"},
		{"an ARN-spelled cloud resource is unaffected",
			Workload{Name: "arn:aws:rds:us-east-1:111111111111:db:datagrok-1"}, "datagrok-1"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.w.IdentityName(); got != c.want {
				t.Errorf("IdentityName() = %q, want %q", got, c.want)
			}
			if got := c.w.ResourceID().Name; got != c.want {
				t.Errorf("ResourceID().Name = %q, want %q", got, c.want)
			}
		})
	}
}
