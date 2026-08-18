// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// formatResourceLine returns what Format printed after "Resource: ", or "" when it
// printed no resource line at all. Asserting on the rendered line (rather than on
// the helper that builds it) is what makes these tests about the card a human reads.
func formatResourceLine(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if after, ok := strings.CutPrefix(l, "Resource: "); ok {
			return after
		}
	}
	return ""
}

// formatScopeLine returns the Format metadata line carrying the cluster/tenant
// facts, or "" when none was printed.
func formatScopeLine(t *testing.T, out string) string {
	t.Helper()
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "cluster ") || strings.Contains(l, "tenant ") {
			return l
		}
	}
	return ""
}

// TestCardResourceLineOmitsNamespaceForUnnamespacedKinds proves the defect observed
// in delivered cards on 2026-08-17/18: the ALERTING RULE's namespace was rendered as
// though it were the resource's own, producing "Node observability/ip-10-11-132-8.
// ec2.internal" for a cluster-scoped Node and "DBInstance observability/datagrok-
// aqemia-shared" for an RDS instance that has no Kubernetes namespace at all. Both
// actively mislead — an operator goes looking in a namespace the object was never in.
//
// Namespaced kinds must be untouched: this is a suppression of a qualifier that
// cannot be true, not a reshuffle of the resource line.
func TestCardResourceLineOmitsNamespaceForUnnamespacedKinds(t *testing.T) {
	tests := []struct {
		name string
		res  providers.Workload
		want string // the whole rendered "Resource: " value
	}{
		{
			name: "Node is cluster-scoped: the alert rule's namespace is dropped",
			res:  providers.Workload{Kind: "Node", Namespace: "observability", Name: "ip-10-11-132-8.ec2.internal"},
			want: "Node ip-10-11-132-8.ec2.internal",
		},
		{
			name: "PersistentVolume is cluster-scoped",
			res:  providers.Workload{Kind: "PersistentVolume", Namespace: "observability", Name: "pvc-8a1f"},
			want: "PersistentVolume pvc-8a1f",
		},
		{
			name: "Namespace is cluster-scoped, and is not inside itself",
			res:  providers.Workload{Kind: "Namespace", Namespace: "observability", Name: "coder-engineering"},
			want: "Namespace coder-engineering",
		},
		{
			name: "ClusterRole is cluster-scoped",
			res:  providers.Workload{Kind: "ClusterRole", Namespace: "kube-system", Name: "view"},
			want: "ClusterRole view",
		},
		{
			name: "StorageClass is cluster-scoped",
			res:  providers.Workload{Kind: "StorageClass", Namespace: "kube-system", Name: "gp3"},
			want: "StorageClass gp3",
		},
		{
			name: "CustomResourceDefinition is cluster-scoped",
			res:  providers.Workload{Kind: "CustomResourceDefinition", Namespace: "flux-system", Name: "helmreleases.helm.toolkit.fluxcd.io"},
			want: "CustomResourceDefinition helmreleases.helm.toolkit.fluxcd.io",
		},
		{
			name: "DBInstance is not a Kubernetes object at all",
			res:  providers.Workload{Kind: "DBInstance", Namespace: "observability", Name: "datagrok-aqemia-shared"},
			want: "DBInstance datagrok-aqemia-shared",
		},
		{
			name: "a CloudTrail resource type is not a Kubernetes kind",
			res:  providers.Workload{Kind: "AWS::RDS::DBInstance", Namespace: "observability", Name: "datagrok-aqemia-shared"},
			want: "AWS::RDS::DBInstance datagrok-aqemia-shared",
		},
		{
			name: "a cluster-scoped kind with no name renders the kind alone, never the namespace",
			res:  providers.Workload{Kind: "Node", Namespace: "observability"},
			want: "Node",
		},
		{
			name: "Pod stays namespaced",
			res:  providers.Workload{Kind: "Pod", Namespace: "observability", Name: "vector-7f9c"},
			want: "Pod observability/vector-7f9c",
		},
		{
			name: "DaemonSet stays namespaced",
			res:  providers.Workload{Kind: "DaemonSet", Namespace: "observability", Name: "vector"},
			want: "DaemonSet observability/vector",
		},
		{
			name: "an unknown kind is assumed namespaced — only a known-wrong qualifier is dropped",
			res:  providers.Workload{Kind: "VerticalPodAutoscaler", Namespace: "observability", Name: "vector"},
			want: "VerticalPodAutoscaler observability/vector",
		},
		{
			name: "a CRD spelled as its group-qualified plural keeps its namespace: the object is real and namespaced",
			res:  providers.Workload{Kind: "helmreleases.helm.toolkit.fluxcd.io", Namespace: "flux-system", Name: "harbor"},
			want: "helmreleases.helm.toolkit.fluxcd.io flux-system/harbor",
		},
		{
			name: "a lowercased, hyphenated kind spelling keeps its namespace too",
			res:  providers.Workload{Kind: "helm-release", Namespace: "flux-system", Name: "harbor"},
			want: "helm-release flux-system/harbor",
		},
		{
			name: "an empty kind renders exactly as before",
			res:  providers.Workload{Namespace: "payments", Name: "api"},
			want: "payments/api",
		},
		{
			name: "an empty kind with no name renders the namespace, as before",
			res:  providers.Workload{Namespace: "payments"},
			want: "payments",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv := providers.Investigation{
				Title:     "something is wrong",
				AlertName: "KubeNodeNotReady",
				Resource:  tc.res,
			}

			if got := formatResourceLine(t, Format(inv)); got != tc.want {
				t.Errorf("Format resource line = %q, want %q", got, tc.want)
			}

			card := blocksText(t, summaryBlocks(inv))
			if !strings.Contains(card, tc.want) {
				t.Errorf("Slack card does not render %q:\n%s", tc.want, card)
			}
			// The header appends the same ref next to the tenant/cluster scope, so a
			// forged namespace there is the same lie in a more prominent place.
			if bad := tc.res.Namespace + "/" + tc.res.Name; !strings.Contains(tc.want, bad) && tc.res.Name != "" {
				if strings.Contains(card, bad) {
					t.Errorf("Slack card still stamps the alert rule's namespace on the resource (%q):\n%s", bad, card)
				}
			}
		})
	}
}

// TestCardCollapsesDuplicateClusterIdentity proves the second delivered-card defect:
// "Cluster: shared · shared", where the tenant merely repeats the cluster name. The
// pair is genuinely informative when the two differ ("tmem175 · tmem175-0"), so only
// the equal case collapses — the tenant is never simply dropped.
func TestCardCollapsesDuplicateClusterIdentity(t *testing.T) {
	tests := []struct {
		name        string
		cluster     string
		tenant      string
		wantCard    string // the Slack "Cluster" field value
		wantFormat  string // the Format metadata line
		absentInAll string // text neither surface may contain ("" = no such check)
	}{
		{
			name:    "tenant equals cluster: say it once",
			cluster: "shared", tenant: "shared",
			wantCard: "shared", wantFormat: "cluster shared", absentInAll: "shared · shared",
		},
		{
			name:    "tenant differs from cluster: both are useful",
			cluster: "tmem175-0", tenant: "tmem175",
			wantCard: "tmem175 · tmem175-0", wantFormat: "cluster tmem175-0 · tenant tmem175",
		},
		{
			name:    "equality is compared trimmed",
			cluster: "shared", tenant: "  shared ",
			wantCard: "shared", wantFormat: "cluster shared",
		},
		{
			name:     "cluster alone is unchanged",
			cluster:  "eu-west-1",
			wantCard: "eu-west-1", wantFormat: "cluster eu-west-1",
		},
		{
			name:     "tenant alone is unchanged",
			tenant:   "platform",
			wantCard: "platform", wantFormat: "tenant platform",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			inv := providers.Investigation{
				Title:     "something is wrong",
				AlertName: "KubeNodeNotReady",
				Cluster:   tc.cluster,
				Tenant:    tc.tenant,
			}

			if got := formatScopeLine(t, Format(inv)); !strings.HasSuffix(got, tc.wantFormat) {
				t.Errorf("Format scope line = %q, want it to end with %q", got, tc.wantFormat)
			}

			card := blocksText(t, summaryBlocks(inv))
			if !strings.Contains(card, "*Cluster:*\\n"+tc.wantCard) {
				t.Errorf("Slack Cluster field is not %q:\n%s", tc.wantCard, card)
			}
			if tc.absentInAll != "" {
				if strings.Contains(card, tc.absentInAll) {
					t.Errorf("Slack card repeats the same name twice (%q):\n%s", tc.absentInAll, card)
				}
				if out := Format(inv); strings.Contains(out, tc.absentInAll) {
					t.Errorf("Format repeats the same name twice (%q):\n%s", tc.absentInAll, out)
				}
			}
		})
	}
}
