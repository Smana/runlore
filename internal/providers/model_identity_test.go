// SPDX-License-Identifier: Apache-2.0

package providers

import "testing"

// TestReconcileWorkloadIdentityNeverDowngradesAnIngestedScope pins the MODEL edge of
// the same canonicalisation ResolveWorkloadIdentity applies at ingestion.
//
// A resource identity enters RunLore from two places, and until now only one of them
// canonicalised: an alert (ResolveWorkloadIdentity) and the model itself, which names
// the affected resource in submit_findings. A model that echoes the full ARN back out
// of the seed prompt produced an identity that keyed and matched differently from the
// ingested one — the spelling split the alert edge closes, still open here.
//
// The model has no alert labels, so the fallback scope is the one the ORIGINATING
// workload already carries. That is what makes this a reconciliation rather than a
// second normalizer: the account ingestion established must survive a model that
// spells the resource without it.
func TestReconcileWorkloadIdentityNeverDowngradesAnIngestedScope(t *testing.T) {
	// The alert workload as ingestion left it: bare identifier, scope on its own fields.
	cloudOrigin := Workload{Namespace: "observability", Name: "datagrok",
		Region: "us-east-1", Account: "111111111111"}
	k8sOrigin := Workload{Kind: "HelmRelease", Namespace: "tooling", Name: "harbor"}

	cases := []struct {
		name   string
		in     Workload
		origin Workload
		want   Workload
	}{
		{"a model-written short name keeps the account ingestion established",
			Workload{Namespace: "observability", Name: "datagrok"}, cloudOrigin,
			Workload{Namespace: "observability", Name: "datagrok", Region: "us-east-1", Account: "111111111111"}},
		{"a model-written ARN is reduced exactly as an ingested one",
			Workload{Namespace: "observability", Name: "arn:aws:rds:us-east-1:111111111111:db:datagrok"}, cloudOrigin,
			Workload{Namespace: "observability", Name: "datagrok", Region: "us-east-1", Account: "111111111111"}},
		{"the ARN wins a disagreement — it names the resource, the origin names the alert that led here",
			Workload{Namespace: "observability", Name: "arn:aws:rds:eu-west-1:333333333333:db:datagrok"}, cloudOrigin,
			Workload{Namespace: "observability", Name: "datagrok", Region: "eu-west-1", Account: "333333333333"}},
		{"a deeper cloud resource found in the same investigation inherits its scope",
			Workload{Namespace: "observability", Name: "datagrok-replica"}, cloudOrigin,
			Workload{Namespace: "observability", Name: "datagrok-replica", Region: "us-east-1", Account: "111111111111"}},
		{"an S3 ARN carries neither qualifier, so the origin supplies both",
			Workload{Namespace: "observability", Name: "arn:aws:s3:::prod-logs/exports"}, cloudOrigin,
			Workload{Namespace: "observability", Name: "prod-logs/exports", Region: "us-east-1", Account: "111111111111"}},
		{"a scope the model named itself is authoritative",
			Workload{Name: "datagrok", Account: "555555555555", Region: "sa-east-1"}, cloudOrigin,
			Workload{Name: "datagrok", Account: "555555555555", Region: "sa-east-1"}},
		{"a Kubernetes object never acquires a cloud scope, whatever the alert carried",
			Workload{Kind: "Pod", Namespace: "apps", Name: "payment-api-59598dbd57-ltkzw"}, cloudOrigin,
			Workload{Kind: "Pod", Namespace: "apps", Name: "payment-api-59598dbd57-ltkzw"}},
		{"an ARN overrides the Kubernetes check — whatever kind the model claimed, it is a cloud resource",
			Workload{Kind: "Deployment", Name: "arn:aws:rds:eu-west-1:333333333333:db:datagrok"}, k8sOrigin,
			Workload{Kind: "Deployment", Name: "datagrok", Region: "eu-west-1", Account: "333333333333"}},
		{"a name that merely contains arn is not one",
			Workload{Kind: "Deployment", Namespace: "apps", Name: "arnica-exporter"}, cloudOrigin,
			Workload{Kind: "Deployment", Namespace: "apps", Name: "arnica-exporter"}},
		{"nothing to qualify: no name",
			Workload{Namespace: "observability"}, cloudOrigin,
			Workload{Namespace: "observability"}},
		{"an unqualified origin adds nothing",
			Workload{Namespace: "tooling", Name: "harbor-registry"}, k8sOrigin,
			Workload{Namespace: "tooling", Name: "harbor-registry"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ReconcileWorkloadIdentity(c.in, c.origin)
			if got != c.want {
				t.Errorf("ReconcileWorkloadIdentity(%+v, %+v) = %+v, want %+v", c.in, c.origin, got, c.want)
			}
			if again := ReconcileWorkloadIdentity(got, c.origin); again != got {
				t.Errorf("not idempotent: %+v became %+v", got, again)
			}
		})
	}
}

// TestBothIdentityEdgesCanonicaliseIdentically is the property the review asked for:
// ONE canonicalisation covering both the alert edge and the model edge, not two hooks
// that can drift. Whatever the alert says about a resource, a model naming the SAME
// resource must land on the same identity — otherwise the recurrence key of a finding
// disagrees with the key of the alert that produced it.
func TestBothIdentityEdgesCanonicaliseIdentically(t *testing.T) {
	labels := map[string]string{"account_id": "111111111111", "region": "us-east-1"}
	for _, spelling := range []string{
		"datagrok",
		"arn:aws:rds:us-east-1:111111111111:db:datagrok",
	} {
		ingested := ResolveWorkloadIdentity(Workload{Namespace: "observability", Name: spelling}, labels)
		modelled := ReconcileWorkloadIdentity(Workload{Namespace: "observability", Name: spelling}, ingested)
		if ingested != modelled {
			t.Errorf("the two identity edges disagree on %q:\n ingested: %+v\n model:    %+v", spelling, ingested, modelled)
		}
	}
}
