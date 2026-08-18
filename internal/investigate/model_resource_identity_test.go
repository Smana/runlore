// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/providers"
)

// cloudAlertWorkload is the RDS instance as INGESTION left it: the bare identifier
// (an ARN has already been reduced) with the cloud scope on its own fields, which is
// what providers.ResolveWorkloadIdentity produces for either spelling of the alert.
func cloudAlertWorkload() providers.Workload {
	return providers.Workload{Namespace: "observability", Name: "datagrok",
		Region: "us-east-1", Account: "111111111111"}
}

// TestModelWrittenResourceIsCanonicalisedLikeAnIngestedOne is the model edge of the
// identity split. `submit_findings.affected_resource` is a second place a resource
// identity enters RunLore, and the model routinely writes the full ARN — it reads it
// off the request labels in the seed prompt. Delivered verbatim, that finding is keyed
// and deduped under a spelling no ingested alert ever produces, so one recurring fault
// files a second knowledge-base entry and its recurrence count restarts.
func TestModelWrittenResourceIsCanonicalisedLikeAnIngestedOne(t *testing.T) {
	got := investigateWithSubmittedResource(t,
		`{"kind":"","namespace":"observability","name":"arn:aws:rds:us-east-1:111111111111:db:datagrok"}`,
		cloudAlertWorkload())
	if got.Resource.Name != "datagrok" {
		t.Errorf("a model-written ARN must be reduced to the identifier ingestion stores, got %q", got.Resource.Name)
	}
	if got.Resource.Account != "111111111111" || got.Resource.Region != "us-east-1" {
		t.Errorf("the scope the ARN names must be lifted onto the workload, got account=%q region=%q",
			got.Resource.Account, got.Resource.Region)
	}
}

// TestModelWrittenResourceCannotDowngradeTheIngestedIdentity is the regression this
// seam is most likely to cause, and the one that matters most: the model names the
// resource WITHOUT the account (it has no labels to hand, and nothing obliges it to
// spell one). If the model's value simply replaced the alert's, the account ingestion
// established would be silently dropped and two AWS accounts' findings would collapse
// onto one dedup fingerprint — the cross-account collision reopened from the model
// edge.
func TestModelWrittenResourceCannotDowngradeTheIngestedIdentity(t *testing.T) {
	origin := cloudAlertWorkload()
	got := investigateWithSubmittedResource(t,
		`{"kind":"","namespace":"observability","name":"datagrok"}`, origin)
	if got.Resource.Account != origin.Account || got.Resource.Region != origin.Region {
		t.Fatalf("a model-written name must not blank the scope ingestion established: got account=%q region=%q, want %q/%q",
			got.Resource.Account, got.Resource.Region, origin.Account, origin.Region)
	}
	// The consequence the field exists for: two accounts stay two findings.
	other := cloudAlertWorkload()
	other.Account = "222222222222"
	got2 := investigateWithSubmittedResource(t,
		`{"kind":"","namespace":"observability","name":"datagrok"}`, other)
	if curator.DupFingerprint(got) == curator.DupFingerprint(got2) {
		t.Fatal("two AWS accounts' findings share one dup fingerprint once the model names the resource — " +
			"the model edge dropped the account and re-opened the collision")
	}
}

// TestModelWrittenKubernetesResourceIsUntouched pins that nothing about Kubernetes
// moves. namespace/name is the whole identity of a Kubernetes object, so a discovered
// object must not acquire a cloud qualifier even when the alert that led there was a
// cloud resource in a real AWS account.
func TestModelWrittenKubernetesResourceIsUntouched(t *testing.T) {
	got := investigateWithSubmittedResource(t,
		`{"kind":"Pod","namespace":"tooling","name":"harbor-registry-59598dbd57-ltkzw"}`,
		cloudAlertWorkload())
	want := providers.Workload{Kind: "Pod", Namespace: "tooling", Name: "harbor-registry-59598dbd57-ltkzw"}
	if got.Resource != want {
		t.Fatalf("a discovered Kubernetes object must be delivered exactly as the model named it:\n got  %+v\n want %+v",
			got.Resource, want)
	}
}

// TestModelWrittenARNInAnotherAccountKeepsItsOwnScope pins the direction of the
// reconciliation: the ARN names the resource, the origin names the alert that led
// here, so an explicitly spelled account wins over the inherited one rather than
// being ignored.
func TestModelWrittenARNInAnotherAccountKeepsItsOwnScope(t *testing.T) {
	got := investigateWithSubmittedResource(t,
		`{"namespace":"observability","name":"arn:aws:rds:eu-west-1:333333333333:db:datagrok"}`,
		cloudAlertWorkload())
	if got.Resource.Account != "333333333333" || got.Resource.Region != "eu-west-1" {
		t.Fatalf("an ARN the model spelled must name its own scope, got account=%q region=%q",
			got.Resource.Account, got.Resource.Region)
	}
}

// investigateWithSubmittedResource runs the real loop with a scripted model that
// submits findings naming the given affected_resource, and returns the delivered
// investigation. It goes through Investigate rather than calling the reconciliation
// directly so the wire between submit_findings and the delivered identity is
// exercised — a helper that skipped the loop would pass with the seam unhooked.
func investigateWithSubmittedResource(t *testing.T, resourceJSON string, alert providers.Workload) providers.Investigation {
	t.Helper()
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
			Args: `{"confidence":0.9,"verdict":"action_suggested","affected_resource":` + resourceJSON +
				`,"root_causes":[{"summary":"connection pool exhausted by the nightly backfill","confidence":0.9}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	req := Request{
		Title:    "RDSHighCPU",
		Workload: alert,
		Labels:   map[string]string{"alertname": "RDSHighCPU", "account_id": alert.Account, "region": alert.Region},
	}
	if err := li.Investigate(context.Background(), req); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("OnComplete not called")
	}
	return *got
}
