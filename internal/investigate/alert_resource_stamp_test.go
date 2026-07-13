// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

// TestLoopStampsAlertResourceFromRequest closes the wire between the Request and the
// curated entry.
//
// The curator can only write alert_resource if the loop stamps it, and the curator's own
// tests construct the Investigation by hand with the field already set — so nothing
// exercised that hop. A mutation deleting `inv.AlertResource = req.Workload` from
// stampRequestFacts passed the ENTIRE suite: both halves of the feature green, the wire
// between them cut, and the fix silently doing nothing in production.
func TestLoopStampsAlertResourceFromRequest(t *testing.T) {
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
			Args: `{"confidence":0.8,"root_causes":[{"summary":"x","confidence":0.8}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	req := Request{
		Title:       "HelmRelease/harbor InstallFailed",
		Fingerprint: "fp-stamp",
		Workload:    providers.Workload{Namespace: "tooling", Name: "harbor"},
		Labels:      map[string]string{"alertname": "HelmReleaseInstallFailed"},
	}
	if err := li.Investigate(context.Background(), req); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("OnComplete not called")
	}
	if ref := got.AlertResource.Ref(); ref != "tooling/harbor" {
		t.Fatalf("the loop must stamp AlertResource verbatim from the REQUEST (a trigger-time fact, "+
			"like AlertName/Severity); got %q", ref)
	}
}

// TestAlertResourceSurvivesResourceRefinement is the whole point of the field.
//
// preferDiscoveredResource lets a resource the investigation DISCOVERS win over the
// alert's — that is right for the entry's human-facing "affected resource". But the
// alert side must survive that overwrite untouched, or the entry is filed only under the
// fault locus and becomes unreachable from the alert that produced it.
func TestAlertResourceSurvivesResourceRefinement(t *testing.T) {
	// The model names a DEEPER resource than the alert did: the pod, not the HelmRelease.
	model := &scriptModel{responses: []providers.CompletionResponse{
		{ToolCalls: []providers.ToolCall{{ID: "1", Name: submitFindingsName,
			Args: `{"confidence":0.9,"resource":{"kind":"Pod","namespace":"tooling","name":"harbor-registry"},"root_causes":[{"summary":"IAM AccessKey quota","confidence":0.9}]}`}}},
	}}
	var got *providers.Investigation
	li := &LoopInvestigator{
		Model:      model,
		Log:        slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnComplete: func(inv providers.Investigation) { got = &inv },
	}
	req := Request{
		Title:       "HelmRelease/harbor InstallFailed",
		Fingerprint: "fp-refine",
		Workload:    providers.Workload{Namespace: "tooling", Name: "harbor"},
		Labels:      map[string]string{"alertname": "HelmReleaseInstallFailed"},
	}
	if err := li.Investigate(context.Background(), req); err != nil {
		t.Fatalf("Investigate: %v", err)
	}
	if got == nil {
		t.Fatal("OnComplete not called")
	}
	// The alert side must be preserved regardless of how the investigation refined the
	// affected resource — that is the index recall will arrive on next time.
	if ref := got.AlertResource.Ref(); ref != "tooling/harbor" {
		t.Fatalf("AlertResource must survive resource refinement (it is the index the NEXT alert arrives on); got %q", ref)
	}
}
