// SPDX-License-Identifier: Apache-2.0

package notify

import "testing"

// TestGCPResourceKindsKeepTheirIdentityOnTheCard pins the fix for a silent drop.
//
// notKubernetesShaped recognised a cloud resource by its ':' — true for
// "AWS::EC2::Instance" and false for GCP's monitored-resource types, which are
// lowercase with underscores. A GCP resource therefore fell through to Workload.Ref(),
// which returns "" when Namespace is empty, so the card rendered no resource at all
// where the AWS equivalent renders its name. Nothing errored; the identity was just
// absent.
func TestGCPResourceKindsKeepTheirIdentityOnTheCard(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		want bool
	}{
		{"a GKE node pool is a cloud resource, not a Kubernetes kind", "gke_nodepool", true},
		{"a Compute Engine instance is a cloud resource", "gce_instance", true},
		{"an AWS ARN-shaped kind still reads as a cloud resource", "AWS::EC2::Instance", true},
		{"a plain Kubernetes kind is unaffected", "Deployment", false},
		{"a dotted CRD kind is unaffected", "helmreleases.helm.toolkit.fluxcd.io", false},
		{"a hyphenated kind is unaffected, since no Kubernetes kind uses an underscore", "helm-release", false},
		{"a slash-qualified kind is unaffected", "apps/Deployment", false},
		{"a spaced kind is unaffected", "Stateful Set", false},
		{"an empty kind stays fail-safe", "", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := notKubernetesShaped(tc.kind); got != tc.want {
				t.Errorf("notKubernetesShaped(%q) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}
