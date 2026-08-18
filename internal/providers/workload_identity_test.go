// SPDX-License-Identifier: Apache-2.0

package providers

import "testing"

// TestResolveWorkloadIdentityReconcilesTheCloudScope pins the reconciliation
// contract the ingestion adapters depend on. The behaviour itself is driven by the
// source-level tests, where it is observable end to end; this table exists so the
// individual rules — which side wins, what is left alone — cannot drift silently.
func TestResolveWorkloadIdentityReconcilesTheCloudScope(t *testing.T) {
	cwLabels := map[string]string{"account_id": "111111111111", "region": "us-east-1"}
	cases := []struct {
		name   string
		in     Workload
		labels map[string]string
		want   Workload
	}{
		{"an ARN names its own scope, with no labels at all",
			Workload{Name: "arn:aws:rds:eu-west-1:333333333333:db:compute-stages"}, nil,
			Workload{Name: "compute-stages", Region: "eu-west-1", Account: "333333333333"}},
		{"a short name takes the scope from the labels",
			Workload{Name: "datagrok"}, cwLabels,
			Workload{Name: "datagrok", Region: "us-east-1", Account: "111111111111"}},
		{"agreeing sources are a no-op",
			Workload{Name: "arn:aws:rds:us-east-1:111111111111:db:datagrok"}, cwLabels,
			Workload{Name: "datagrok", Region: "us-east-1", Account: "111111111111"}},
		{"the ARN wins a disagreement — it names the resource, the label names the series that saw it",
			Workload{Name: "arn:aws:rds:eu-west-1:333333333333:db:datagrok"}, cwLabels,
			Workload{Name: "datagrok", Region: "eu-west-1", Account: "333333333333"}},
		{"an S3 ARN carries neither qualifier, so the labels supply both",
			Workload{Name: "arn:aws:s3:::prod-logs/exports"}, cwLabels,
			Workload{Name: "prod-logs/exports", Region: "us-east-1", Account: "111111111111"}},
		{"the cloudwatch_exporter label spellings are read too",
			Workload{Name: "datagrok"}, map[string]string{"aws_account_id": "444444444444", "aws_region": "ap-south-1"},
			Workload{Name: "datagrok", Region: "ap-south-1", Account: "444444444444"}},
		{"a bare `account` label is NOT an AWS account — it is a plausible application label",
			Workload{Name: "datagrok"}, map[string]string{"account": "acme-corp"},
			Workload{Name: "datagrok"}},
		{"a Kubernetes object is never qualified, whatever the stack stamps globally",
			Workload{Kind: "Deployment", Namespace: "apps", Name: "payment-api"}, cwLabels,
			Workload{Kind: "Deployment", Namespace: "apps", Name: "payment-api"}},
		{"a pod keeps its template hash — normalization is a comparison-time concern",
			Workload{Kind: "Pod", Namespace: "tooling", Name: "harbor-registry-59598dbd57-ltkzw"}, cwLabels,
			Workload{Kind: "Pod", Namespace: "tooling", Name: "harbor-registry-59598dbd57-ltkzw"}},
		{"an ARN overrides the Kubernetes check — whatever label carried it, it is a cloud resource",
			Workload{Kind: "Deployment", Name: "arn:aws:rds:eu-west-1:333333333333:db:datagrok"}, nil,
			Workload{Kind: "Deployment", Name: "datagrok", Region: "eu-west-1", Account: "333333333333"}},
		{"a name that merely contains arn is not one",
			Workload{Kind: "Deployment", Name: "arnica-exporter"}, cwLabels,
			Workload{Kind: "Deployment", Name: "arnica-exporter"}},
		{"nothing to qualify: no name, no scope",
			Workload{Namespace: "observability"}, cwLabels,
			Workload{Namespace: "observability"}},
		{"a scope already set is authoritative",
			Workload{Name: "datagrok", Account: "555555555555", Region: "sa-east-1"}, cwLabels,
			Workload{Name: "datagrok", Account: "555555555555", Region: "sa-east-1"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ResolveWorkloadIdentity(c.in, c.labels)
			if got != c.want {
				t.Errorf("ResolveWorkloadIdentity(%+v) = %+v, want %+v", c.in, got, c.want)
			}
			if again := ResolveWorkloadIdentity(got, c.labels); again != got {
				t.Errorf("not idempotent: %+v became %+v", got, again)
			}
		})
	}
}

// TestWorkloadResourceIDUnionsTheTwoSourcesOfScope pins the asymmetry between the
// comparison path and the key path: comparison reads the scope from the name AND the
// fields, because an absent qualifier is compatible rather than different, so extra
// evidence can only split resources that really are distinct. A key cannot do that —
// see curator.IncidentKey, which reads the field alone.
func TestWorkloadResourceIDUnionsTheTwoSourcesOfScope(t *testing.T) {
	cases := []struct {
		name string
		in   Workload
		want ResourceID
	}{
		{"an ARN still in the name is resolved — that is how legacy catalog entries stay comparable",
			Workload{Namespace: "observability", Name: "arn:aws:rds:us-east-1:195275669196:db:compute-stages"},
			ResourceID{Name: "compute-stages", Region: "us-east-1", Account: "195275669196"}},
		{"a bare name is qualified by the scope ingestion stamped on it",
			Workload{Namespace: "observability", Name: "compute-stages", Account: "195275669196", Region: "us-east-1"},
			ResourceID{Name: "compute-stages", Region: "us-east-1", Account: "195275669196"}},
		{"a Kubernetes workload resolves to a bare, unqualified identity",
			Workload{Namespace: "tooling", Name: "harbor-registry-59598dbd57-ltkzw"},
			ResourceID{Name: "harbor-registry"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := c.in.ResourceID(); got != c.want {
				t.Errorf("%+v.ResourceID() = %+v, want %+v", c.in, got, c.want)
			}
		})
	}
}
