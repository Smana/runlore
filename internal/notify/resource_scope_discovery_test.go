// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestCardResourceLineTrustsTheDiscoveredScope proves the renderer prefers the
// namespaced-ness the API server itself reported over the kind lists in
// resource_scope.go.
//
// The lists were a stopgap for a fact the process already obtains: discovery
// (ServerPreferredResources) answers correctly for EVERY kind the cluster serves,
// including CRDs the lists cannot enumerate. Their stated invariant — "only names
// with no namespaced Kubernetes homonym are listed" — is false: AWS Controllers for
// Kubernetes and Crossplane v2 ship NAMESPACED CRDs spelled DBInstance, DBCluster,
// CacheCluster, Nodegroup, TargetGroup, LaunchTemplate, LoadBalancer, and HyperShift's
// NodePool is namespaced where Karpenter's is not. On such a cluster the lists strip a
// namespace that was a fact about the object, which is the exact failure this whole
// area exists to prevent — and only discovery can tell the two apart.
func TestCardResourceLineTrustsTheDiscoveredScope(t *testing.T) {
	tests := []struct {
		name string
		res  providers.Workload
		want string
	}{
		{
			name: "an ACK DBInstance discovered as namespaced keeps its namespace, list or no list",
			res: providers.Workload{
				Kind: "DBInstance", Namespace: "ack-system", Name: "datagrok-aqemia-shared",
				Scope: providers.ScopeNamespaced,
			},
			want: "DBInstance ack-system/datagrok-aqemia-shared",
		},
		{
			name: "HyperShift's NodePool is namespaced where Karpenter's is not",
			res: providers.Workload{
				Kind: "NodePool", Namespace: "clusters", Name: "dev-workers",
				Scope: providers.ScopeNamespaced,
			},
			want: "NodePool clusters/dev-workers",
		},
		{
			name: "a CRD no list could enumerate, discovered as cluster-scoped, loses the alert's namespace",
			res: providers.Workload{
				Kind: "ClusterCompliancePolicy", Namespace: "observability", Name: "nsa",
				Scope: providers.ScopeClusterScoped,
			},
			want: "ClusterCompliancePolicy nsa",
		},
		{
			name: "a discovered cluster-scoped Node renders exactly as the list already made it",
			res: providers.Workload{
				Kind: "Node", Namespace: "observability", Name: "ip-10-11-132-8.ec2.internal",
				Scope: providers.ScopeClusterScoped,
			},
			want: "Node ip-10-11-132-8.ec2.internal",
		},
		{
			// The one cluster-scoped kind whose own identity arrives in the namespace
			// field. Discovery agreeing with the list must not change that carve-out.
			name: "a discovered cluster-scoped Namespace with no name still renders its own name",
			res: providers.Workload{
				Kind: "Namespace", Namespace: "coder-engineering",
				Scope: providers.ScopeClusterScoped,
			},
			want: "Namespace coder-engineering",
		},
		{
			// Unknown is NOT cluster-scoped: no discovery answer means the lists still
			// decide, which is what keeps an RDS alert's DBInstance working.
			name: "with no discovered scope the cloud-kind list still strips a namespace no RDS instance has",
			res: providers.Workload{
				Kind: "DBInstance", Namespace: "observability", Name: "datagrok-aqemia-shared",
			},
			want: "DBInstance datagrok-aqemia-shared",
		},
		{
			name: "with no discovered scope an unlisted kind keeps its namespace, exactly as before",
			res: providers.Workload{
				Kind: "VerticalPodAutoscaler", Namespace: "observability", Name: "vector",
			},
			want: "VerticalPodAutoscaler observability/vector",
		},
		{
			// A namespaced answer for a kind the cloud list names is the whole point:
			// on an ACK cluster the qualifier is real, and the list must not win.
			name: "a discovered namespaced LoadBalancer beats the cloud list",
			res: providers.Workload{
				Kind: "LoadBalancer", Namespace: "ack-system", Name: "public",
				Scope: providers.ScopeNamespaced,
			},
			want: "LoadBalancer ack-system/public",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv := providers.Investigation{
				Title:     "something is wrong",
				AlertName: "KubeNodeNotReady",
				Resource:  tc.res,
			}
			if got := formatResourceLine(Format(inv)); got != tc.want {
				t.Errorf("Format resource line = %q, want %q", got, tc.want)
			}
			card := blocksText(t, summaryBlocks(inv))
			if !strings.Contains(card, "*Resource:*\\n"+tc.want) {
				t.Errorf("Slack card Resource field is not %q:\n%s", tc.want, card)
			}
		})
	}
}
