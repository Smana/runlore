// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestCloudResourceKindsKeepTheirIdentityOnTheCard pins the fix for a silent drop, and
// pins WHERE that fix lives.
//
// The defect: notKubernetesShaped recognised a cloud resource by its ':' — true for
// "AWS::EC2::Instance" and false for GCP's monitored-resource types, which are lowercase
// with underscores. A GCP resource therefore fell through to Workload.Ref(), which
// returns "" when Namespace is empty, so the card rendered no resource at all where the
// AWS equivalent renders its name. Nothing errored; the identity was just absent.
//
// The fix attempted FIRST was to add '_' to this function, and the table below is why
// that was wrong: this function's input is model-written free text, so '_' also matches
// a real namespaced object a model spelled "stateful_set", stripping a true namespace
// off it — the one failure resource_scope.go says it must not introduce. The fix instead
// belongs in the provider, which is the only layer that knows whether a type name came
// from a cloud API. providers.CloudKind stamps the "<engine>::" prefix there, so every
// cloud resource arrives here already carrying the one character no Kubernetes kind can
// contain, and this function never has to learn a provider name.
func TestCloudResourceKindsKeepTheirIdentityOnTheCard(t *testing.T) {
	for _, tc := range []struct {
		name string
		kind string
		want bool
	}{
		// What the providers actually emit, via CloudKind.
		{"a qualified GKE node pool is a cloud resource", providers.CloudKind(providers.EngineGCP, "gke_nodepool"), true},
		{"a qualified Compute Engine instance is a cloud resource", providers.CloudKind(providers.EngineGCP, "gce_instance"), true},
		{"an AWS resource type passes through already qualified", providers.CloudKind(providers.EngineAWS, "AWS::EC2::Instance"), true},
		{"an AWS event source, which has no colon of its own, is qualified too",
			providers.CloudKind(providers.EngineAWS, "ec2.amazonaws.com"), true},
		{"a bare ARN-shaped kind still reads as a cloud resource", "AWS::EC2::Instance", true},

		// Kubernetes kinds, including the malformed spellings a model reaches for.
		{"a plain Kubernetes kind is unaffected", "Deployment", false},
		{"a dotted CRD kind is unaffected", "helmreleases.helm.toolkit.fluxcd.io", false},
		{"a hyphenated kind is unaffected", "helm-release", false},
		{"a slash-qualified kind is unaffected", "apps/Deployment", false},
		{"a spaced kind is unaffected", "Stateful Set", false},
		{"an empty kind stays fail-safe", "", false},

		// The regression the '_' rule would have introduced. snake_case is one of the
		// most common ways a model normalises a kind, and every one of these is a REAL
		// namespaced object whose namespace must survive.
		{"a snake_case StatefulSet keeps its namespace", "stateful_set", false},
		{"a snake_case ConfigMap keeps its namespace", "config_map", false},
		{"a snake_case CronJob keeps its namespace", "cron_job", false},
		{"a snake_case PersistentVolumeClaim keeps its namespace", "persistent_volume_claim", false},

		// A third provider must need no change here at all.
		{"an Azure resource type is a cloud resource once qualified",
			providers.CloudKind("azure", "Microsoft.Compute/virtualMachines"), true},
		{"an unqualified Azure-shaped type is dotted and slashed, both Kubernetes shapes",
			"Microsoft.Compute/virtualMachines", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := notKubernetesShaped(tc.kind); got != tc.want {
				t.Errorf("notKubernetesShaped(%q) = %v, want %v", tc.kind, got, tc.want)
			}
		})
	}
}
