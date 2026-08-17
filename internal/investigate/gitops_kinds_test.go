// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/providers/gitops/argocd"
	"github.com/Smana/runlore/internal/providers/gitops/flux"
)

// TestGitOpsKindsTrackTheProviders is the drift guard the first version of this file
// lacked. The advertised set was typed out by hand next to a provider that resolves its
// own, and they diverged on the very first edit: flux.kindToGVR resolves
// ExternalArtifact (and the chart grants RBAC for it, and this repo's own eval renders
// one as a source node), yet the tools refused it as "not a GitOps object these tools
// can resolve" — the same defect as #503 with the sign flipped, since every clause of
// that refusal was false.
//
// Both directions matter. A kind the provider resolves but the tools hide is an
// unreachable capability plus a false refusal; a kind the tools offer but the provider
// cannot resolve reaches an inspector that errors, or worse (see the canonicalisation
// test) renders a fabricated node.
func TestGitOpsKindsTrackTheProviders(t *testing.T) {
	for engine, resolvable := range map[string][]string{
		"flux":   flux.GitOpsKinds(),
		"argocd": argocd.GitOpsKinds(),
	} {
		advertised := GitOpsKinds(engine)
		if !slices.Equal(slices.Sorted(slices.Values(advertised)), slices.Sorted(slices.Values(resolvable))) {
			t.Errorf("%s: advertised kinds %v != the kinds the provider resolves %v",
				engine, advertised, resolvable)
		}
		for _, k := range resolvable {
			if !gitopsKindSupported(engine, k) {
				t.Errorf("%s: the provider resolves %s but the tools refuse it", engine, k)
			}
		}
	}
	// Named explicitly: this is the kind that was actually dropped, so a future edit
	// that re-hand-rolls the list fails here with the incident's own name on it.
	if !gitopsKindSupported("flux", "ExternalArtifact") {
		t.Error("ExternalArtifact is refused on flux, yet the provider resolves it and the chart grants RBAC for it")
	}
}

// countingInspector records what it was asked, so a test can assert the tool refused an
// out-of-scope kind BEFORE any lookup rather than after one.
type countingInspector struct {
	calls    int
	notFound bool
}

func (c *countingInspector) ResourceStatus(_ context.Context, w providers.Workload) (providers.ResourceStatus, error) {
	c.calls++
	return providers.ResourceStatus{Workload: w, NotFound: c.notFound, Ready: "True"}, nil
}

func (c *countingInspector) DependencyTree(_ context.Context, w providers.Workload) (providers.DepNode, error) {
	c.calls++
	return providers.DepNode{Workload: w, NotFound: c.notFound, Ready: "True"}, nil
}

// TestGitOpsKindsAreEngineScoped pins the capability boundary. Flux objects cannot exist on
// an Argo CD deployment, so advertising them invites a question whose only possible answer
// is a misleading negative — the live failure this comes from, where three
// HelmRelease/Kustomization "NOT FOUND" replies were cited as evidence on a cluster with no
// Flux CRDs at all.
func TestGitOpsKindsAreEngineScoped(t *testing.T) {
	argo := GitOpsKinds("argocd")
	if len(argo) != 1 || argo[0] != "Application" {
		t.Fatalf("argocd kinds = %v, want exactly [Application]", argo)
	}
	for _, k := range []string{"HelmRelease", "Kustomization", "GitRepository", "Bucket"} {
		if gitopsKindSupported("argocd", k) {
			t.Errorf("%s is advertised on argocd, where it can never exist", k)
		}
	}
	if !gitopsKindSupported("flux", "HelmRelease") {
		t.Error("HelmRelease must be supported on flux")
	}
	if gitopsKindSupported("flux", "Application") {
		t.Error("Argo CD's Application is advertised on flux, where it can never exist")
	}
	// The default mirrors app.GitopsEngine's own ("" ⇒ flux) so the default lives in one
	// place. A caller that forgets to set Engine therefore behaves as before, not worse.
	// Compared element-wise: the earlier length-only check passed a seven-element list
	// that had dropped Bucket and gained Application.
	if got, want := GitOpsKinds(""), GitOpsKinds("flux"); !slices.Equal(got, want) {
		t.Errorf("empty engine = %v, want the flux set %v", got, want)
	}
	// Case-insensitive: the model writes the kind as prose, and "helmrelease" must be
	// refused for the same reason as "HelmRelease" rather than falling through to a lookup.
	if !gitopsKindSupported("flux", "helmrelease") {
		t.Error("kind matching must be case-insensitive")
	}
}

// TestGitOpsSchemaConstrainsKind pins the enum, which is the part that actually prevents
// the failure: a description can be disregarded by the model, an out-of-enum value cannot
// be sent.
//
// The enum is checked against what the PROVIDER resolves, not against
// gitopsKindSupported: both of those derive from GitOpsKinds, so comparing them proves
// nothing, and the earlier version of this test was that tautology.
func TestGitOpsSchemaConstrainsKind(t *testing.T) {
	resolvable := map[string][]string{"argocd": argocd.GitOpsKinds(), "flux": flux.GitOpsKinds()}
	for _, engine := range []string{"argocd", "flux"} {
		for name, schema := range map[string]string{
			"gitops_resource_status": GitOpsStatusTool{Engine: engine}.Schema(),
			"gitops_tree":            GitOpsTreeTool{Engine: engine}.Schema(),
		} {
			var got struct {
				Properties struct {
					Kind struct {
						Enum []string `json:"enum"`
					} `json:"kind"`
				} `json:"properties"`
			}
			if err := json.Unmarshal([]byte(schema), &got); err != nil {
				t.Fatalf("%s/%s: schema is not valid JSON: %v", name, engine, err)
			}
			if len(got.Properties.Kind.Enum) == 0 {
				t.Fatalf("%s/%s: kind has no enum, so any kind can be asked: %s", name, engine, schema)
			}
			want := slices.Sorted(slices.Values(resolvable[engine]))
			if got := slices.Sorted(slices.Values(got.Properties.Kind.Enum)); !slices.Equal(got, want) {
				t.Errorf("%s/%s: enum offers %v, but the %s provider resolves %v", name, engine, got, engine, want)
			}
		}
	}
}

// TestGitOpsUnsupportedKindIsNotEvidenceOfAbsence is the core regression. Asked about a
// kind it never supported, the tool used to answer "the object genuinely does not exist
// (likely the cascade root)" — a claim about the world in reply to a question about the
// tool's scope. The model quoted it as evidence, built a wrong mechanism on it, and advised
// recovering from Git history an object that was in the cluster the whole time.
func TestGitOpsUnsupportedKindIsNotEvidenceOfAbsence(t *testing.T) {
	for _, tc := range []struct {
		tool string
		call func(*countingInspector) (string, error)
	}{
		{"gitops_resource_status", func(i *countingInspector) (string, error) {
			return GitOpsStatusTool{Inspector: i, Engine: "argocd"}.Call(context.Background(),
				`{"kind":"VMServiceScrape","name":"datagrok-rabbitmq","namespace":"observability"}`)
		}},
		{"gitops_tree", func(i *countingInspector) (string, error) {
			return GitOpsTreeTool{Inspector: i, Engine: "argocd"}.Call(context.Background(),
				`{"kind":"VMServiceScrape","name":"datagrok-rabbitmq","namespace":"observability"}`)
		}},
	} {
		insp := &countingInspector{notFound: true}
		out, err := tc.call(insp)
		if err != nil {
			t.Fatalf("%s: %v", tc.tool, err)
		}
		// The words that made the old answer dangerous. "not found" is included because the
		// tree renderer emits "NOT FOUND  ← root", which hands back a root cause outright.
		for _, forbidden := range []string{"does not exist", "genuinely", "cascade root", "← root"} {
			if strings.Contains(strings.ToLower(out), strings.ToLower(forbidden)) {
				t.Errorf("%s: out-of-scope kind answered with %q — that is a claim about the cluster:\n%s",
					tc.tool, forbidden, out)
			}
		}
		// The disclaimer is load-bearing: without it the model cannot tell that the reply
		// is silent about existence rather than asserting absence.
		if !strings.Contains(out, "NOTHING about whether the object exists") {
			t.Errorf("%s: reply does not disclaim evidence of absence:\n%s", tc.tool, out)
		}
		// Refused BEFORE the lookup — a tool that cannot answer should not spend a call.
		if insp.calls != 0 {
			t.Errorf("%s: inspector was called %d times for an unsupported kind", tc.tool, insp.calls)
		}
	}
}

// TestGitOpsNotFoundDoesNotNominateARootCause covers the SUPPORTED-kind path. Absence is a
// fact worth stating; that the absence caused the incident is for the loop to establish.
// The old wording asserted both from a single name lookup.
func TestGitOpsNotFoundDoesNotNominateARootCause(t *testing.T) {
	insp := &countingInspector{notFound: true}
	out, err := GitOpsStatusTool{Inspector: insp, Engine: "argocd"}.Call(context.Background(),
		`{"kind":"Application","name":"runlore","namespace":"argocd"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "NOT FOUND") {
		t.Fatalf("a genuinely absent supported object must still report absence:\n%s", out)
	}
	for _, forbidden := range []string{"genuinely does not exist", "likely the cascade root"} {
		if strings.Contains(out, forbidden) {
			t.Errorf("NOT FOUND still over-claims with %q:\n%s", forbidden, out)
		}
	}
	if !strings.Contains(out, "do not assume it is the root cause") {
		t.Errorf("NOT FOUND does not warn against reading absence as causation:\n%s", out)
	}
}

// TestGitOpsDescriptionsMatchTheEngine stops the prose and the enum drifting apart: a
// description naming kinds the schema refuses would send the model to a dead end, which is
// the same class of confusion in the opposite direction.
func TestGitOpsDescriptionsMatchTheEngine(t *testing.T) {
	for _, d := range []string{
		GitOpsStatusTool{Engine: "argocd"}.Description(),
		GitOpsTreeTool{Engine: "argocd"}.Description(),
	} {
		for _, flux := range []string{"HelmRelease", "Kustomization", "OCIRepository"} {
			if strings.Contains(d, flux) {
				t.Errorf("argocd description advertises the Flux kind %s:\n%s", flux, d)
			}
		}
		if !strings.Contains(d, "Application") {
			t.Errorf("argocd description does not name its one supported kind:\n%s", d)
		}
	}
	for _, d := range []string{
		GitOpsStatusTool{Engine: "flux"}.Description(),
		GitOpsTreeTool{Engine: "flux"}.Description(),
	} {
		if !strings.Contains(d, "HelmRelease") {
			t.Errorf("flux description does not name HelmRelease:\n%s", d)
		}
	}
}
