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
	asked    providers.Workload // what the last call was actually given
}

func (c *countingInspector) ResourceStatus(_ context.Context, w providers.Workload) (providers.ResourceStatus, error) {
	c.calls++
	c.asked = w
	return providers.ResourceStatus{Workload: w, NotFound: c.notFound, Ready: "True"}, nil
}

func (c *countingInspector) DependencyTree(_ context.Context, w providers.Workload) (providers.DepNode, error) {
	c.calls++
	c.asked = w
	return providers.DepNode{Workload: w, NotFound: c.notFound, Ready: "True"}, nil
}

// TestGitOpsKindIsCanonicalisedBeforeTheLookup closes the gap between a
// case-INSENSITIVE guard and a case-SENSITIVE resolver.
//
// gitopsKindSupported matched with EqualFold while Call forwarded in.Kind verbatim and
// flux.kindToGVR is an exact-match map, so kind:"helmrelease" passed the guard and then
// missed the map. gitops_resource_status surfaced that as an error, but gitops_tree
// swallows a resolution failure (flux depNode: `if err != nil { return node }`) and
// rendered
//
//	helmrelease apps/api (Ready=unknown)
//
// — a node for an object nobody looked up, asserting it exists. A fabricated node in a
// dependency tree is the same class of false evidence as #503's false negative.
func TestGitOpsKindIsCanonicalisedBeforeTheLookup(t *testing.T) {
	for _, tc := range []struct {
		tool string
		call func(*countingInspector, string) (string, error)
	}{
		{"gitops_resource_status", func(i *countingInspector, kind string) (string, error) {
			return GitOpsStatusTool{Inspector: i, Engine: "flux"}.Call(context.Background(),
				`{"kind":"`+kind+`","name":"api","namespace":"apps"}`)
		}},
		{"gitops_tree", func(i *countingInspector, kind string) (string, error) {
			return GitOpsTreeTool{Inspector: i, Engine: "flux"}.Call(context.Background(),
				`{"kind":"`+kind+`","name":"api","namespace":"apps"}`)
		}},
	} {
		for _, written := range []string{"helmrelease", "HELMRELEASE", "HelmRelease"} {
			insp := &countingInspector{}
			if _, err := tc.call(insp, written); err != nil {
				t.Fatalf("%s/%s: %v", tc.tool, written, err)
			}
			if insp.asked.Kind != "HelmRelease" {
				t.Errorf("%s: kind %q reached the inspector as %q — the resolver matches exactly, "+
					"so anything but the canonical spelling silently fails to resolve",
					tc.tool, written, insp.asked.Kind)
			}
		}
	}
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
	// Case-insensitive in BOTH directions, which the earlier version only half-asserted:
	// the model writes the kind as prose, so "helmrelease" is accepted on flux (and
	// canonicalised — see TestGitOpsKindIsCanonicalisedBeforeTheLookup), and refused on
	// argocd for exactly the same reason "HelmRelease" is. A guard that folds case when
	// accepting but not when refusing is a hole, not a convenience.
	if got, ok := canonicalGitOpsKind("flux", "helmrelease"); !ok || got != "HelmRelease" {
		t.Errorf(`canonicalGitOpsKind("flux","helmrelease") = %q,%v; want "HelmRelease",true`, got, ok)
	}
	if gitopsKindSupported("argocd", "helmrelease") {
		t.Error("lowercase helmrelease is accepted on argocd, where the engine cannot own it")
	}
	if gitopsKindSupported("flux", "APPLICATION") {
		t.Error("uppercase APPLICATION is accepted on flux, where the engine cannot own it")
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

// TestGitOpsNegativeReportsTheLookupNotAVerdict covers the three states in which the
// softened NOT FOUND wording was still false. All of them are on the SUPPORTED-kind
// path, which engine scoping does not touch, so #503's mechanism survived there:
//
//   - an unserved CRD 404s exactly like an absent object;
//   - a Forbidden on the all-namespaces List was swallowed, so the answer claimed a
//     cluster-wide search that never ran;
//   - flux-system was named unconditionally, including on Argo CD, which never
//     searches it — and the earlier test asserted that very string with Engine:"argocd".
//
// The old wording's own guard is why this checks the SHAPE of the claim rather than a
// blocklist: the forbidden-word list contained "does not exist", and "and no such object
// exists" walked straight past it.
func TestGitOpsNegativeReportsTheLookupNotAVerdict(t *testing.T) {
	for _, tc := range []struct {
		name    string
		engine  string
		lookup  providers.Lookup
		want    []string
		wantNot []string
	}{
		{
			name:   "absent on argocd names only the scopes that ran",
			engine: "argocd",
			lookup: providers.Lookup{Reason: providers.LookupAbsent,
				Scopes: []string{"apps", providers.AllNamespaces}},
			want: []string{"NOT FOUND", "apps", providers.AllNamespaces, "do not assume it is the root cause"},
			// The whole point: Argo CD never searches flux-system, so the answer may not
			// say it did.
			wantNot: []string{"flux-system"},
		},
		{
			name:   "unserved CRD is reported as a missing type, not a missing object",
			engine: "flux",
			lookup: providers.Lookup{Reason: providers.LookupKindNotServed, Scopes: []string{"apps", "flux-system"}},
			want: []string{"serves no such resource type",
				"NOT evidence that the object is absent"},
			wantNot: []string{"NOT FOUND"},
		},
		{
			name:   "denied cluster-wide search is reported as denied",
			engine: "flux",
			lookup: providers.Lookup{Reason: providers.LookupDenied, Scopes: []string{"apps", "flux-system"}},
			want: []string{"DENIED by RBAC", "never ran", "NOT evidence that the object is absent",
				"may exist in a namespace this agent cannot list"},
			// The search it did not do must not be listed as one it did.
			wantNot: []string{providers.AllNamespaces},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			insp := fakeInspector{status: providers.ResourceStatus{NotFound: true, Lookup: tc.lookup}}
			kind := "HelmRelease"
			if tc.engine == "argocd" {
				kind = "Application"
			}
			out, err := GitOpsStatusTool{Inspector: insp, Engine: tc.engine}.Call(context.Background(),
				`{"kind":"`+kind+`","name":"api","namespace":"apps"}`)
			if err != nil {
				t.Fatal(err)
			}
			for _, w := range tc.want {
				if !strings.Contains(out, w) {
					t.Errorf("answer is missing %q:\n%s", w, out)
				}
			}
			for _, w := range tc.wantNot {
				if strings.Contains(out, w) {
					t.Errorf("answer claims %q, which this lookup did not establish:\n%s", w, out)
				}
			}
			// No wording, in any state, may assert the object is absent from the cluster.
			for _, claim := range []string{"does not exist", "no such object exists", "genuinely", "cascade root"} {
				if strings.Contains(strings.ToLower(out), claim) {
					t.Errorf("answer asserts %q from one name lookup:\n%s", claim, out)
				}
			}
		})
	}
}

// TestGitOpsTreeReportsTheLookupPerNode is gitops_tree's half of the same property. It
// used to render a node the API never returned as "NOT FOUND  ← root" — which this
// branch's own comment calls nominating the absence as the cause outright — and a node
// whose read merely FAILED as "(Ready=unknown)", asserting an object nobody read.
func TestGitOpsTreeReportsTheLookupPerNode(t *testing.T) {
	tree := providers.DepNode{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"},
		Ready:    "False", Reason: "DependencyNotReady",
		Children: []providers.DepNode{
			{Workload: providers.Workload{Kind: "GitRepository", Name: "gone", Namespace: "flux-system"},
				NotFound: true, Lookup: providers.Lookup{Reason: providers.LookupAbsent}},
			{Workload: providers.Workload{Kind: "HelmRelease", Name: "denied", Namespace: "apps"},
				Lookup: providers.Lookup{Reason: providers.LookupDenied}},
			{Workload: providers.Workload{Kind: "ArtifactGenerator", Name: "split", Namespace: "flux-system"},
				Lookup: providers.Lookup{Reason: providers.LookupUnresolvable}},
			{Workload: providers.Workload{Kind: "Bucket", Name: "flaky", Namespace: "flux-system"},
				Lookup: providers.Lookup{Reason: providers.LookupFailed}},
		},
	}
	out, err := GitOpsTreeTool{Inspector: fakeInspector{tree: tree}, Engine: "flux"}.Call(context.Background(),
		`{"kind":"Kustomization","name":"apps","namespace":"flux-system"}`)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"gone: not found by name",
		"denied: search DENIED by RBAC (not evidence of absence)",
		"split: not a kind these tools resolve — not looked up (not evidence of absence)",
		"flaky: read FAILED (not evidence of absence)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("tree is missing %q:\n%s", want, out)
		}
	}
	// A node nobody could read must never render as an existing one.
	if strings.Contains(out, "Ready=unknown") {
		t.Errorf("a node the API did not return is rendered with a Ready state:\n%s", out)
	}
	if strings.Contains(out, "← root") {
		t.Errorf("the tree still nominates a root cause from one name lookup:\n%s", out)
	}
}

// TestGitOpsTreeRecordsOnlyNodesItActuallyRead pins the observed-set side of the same
// change. recordObservedTree gated on NotFound alone, so a node whose read was DENIED or
// errored — never returned by the API — was recorded as server-confirmed and became a
// legitimate action target (guardUnobservedTargets). Only a node the provider actually
// read may count.
func TestGitOpsTreeRecordsOnlyNodesItActuallyRead(t *testing.T) {
	tree := providers.DepNode{
		Workload: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}, Ready: "True",
		Children: []providers.DepNode{
			{Workload: providers.Workload{Kind: "HelmRelease", Name: "denied", Namespace: "apps"},
				Lookup: providers.Lookup{Reason: providers.LookupDenied}},
			{Workload: providers.Workload{Kind: "Bucket", Name: "flaky", Namespace: "flux-system"},
				Lookup: providers.Lookup{Reason: providers.LookupFailed}},
		},
	}
	ctx := WithObservedResources(context.Background())
	if _, err := (GitOpsTreeTool{Inspector: fakeInspector{tree: tree}, Engine: "flux"}).Call(ctx,
		`{"kind":"Kustomization","name":"apps","namespace":"flux-system"}`); err != nil {
		t.Fatal(err)
	}
	obs := observedFrom(ctx)
	if !obs.matches(providers.Workload{Name: "apps", Namespace: "flux-system"}) {
		t.Error("a node that WAS read is not recorded as observed")
	}
	for _, ghost := range []providers.Workload{
		{Name: "denied", Namespace: "apps"},
		{Name: "flaky", Namespace: "flux-system"},
	} {
		if obs.matches(ghost) {
			t.Errorf("%s/%s was recorded as observed, but the API never returned it",
				ghost.Namespace, ghost.Name)
		}
	}
}

// TestGitOpsDescriptionsMatchTheEngine stops the prose and the enum drifting apart: a
// description naming kinds the schema refuses would send the model to a dead end, which is
// the same class of confusion in the opposite direction.
//
// The flux side is checked too, and it was not before — which is how the descriptions kept
// naming "an Argo CD Application" on a Flux deployment whose enum refuses that kind.
func TestGitOpsDescriptionsMatchTheEngine(t *testing.T) {
	for engine, foreign := range map[string][]string{
		"argocd": {"HelmRelease", "Kustomization", "OCIRepository", "Flux"},
		"flux":   {"Application", "Argo CD"},
	} {
		for _, d := range []string{
			GitOpsStatusTool{Engine: engine}.Description(),
			GitOpsTreeTool{Engine: engine}.Description(),
		} {
			for _, other := range foreign {
				if strings.Contains(d, other) {
					t.Errorf("%s description mentions %q, which this deployment's enum refuses:\n%s",
						engine, other, d)
				}
			}
			for _, own := range GitOpsKinds(engine) {
				if !strings.Contains(d, own) {
					t.Errorf("%s description omits the supported kind %s:\n%s", engine, own, d)
				}
			}
		}
	}
}

// TestGitOpsProseMakesNoClaimAboutTheCluster is F4. The description read:
//
//	(this deployment runs Argo CD; Flux kinds cannot exist here)
//
// A cluster fact, stated in the tool list on every single turn, derived from an
// UNVALIDATED config string: app.GitopsEngine maps "argo", "ArgoCD" and every other
// misspelling silently to flux (pinned as intended by its own test), and a cluster
// mid-migration runs both engines at once. Before this branch that case produced an
// honest tool ERROR, which the loop already reads as missing data; the branch turned it
// into a fluent unhedged claim. The lie moved out of the answer and into the prompt.
//
// The list of kinds is a fact about this tool. What cannot exist on the cluster is not.
func TestGitOpsProseMakesNoClaimAboutTheCluster(t *testing.T) {
	for _, engine := range []string{"flux", "argocd", ""} {
		prose := gitopsKindProse(engine)
		for _, claim := range []string{"cannot exist", "do not exist", "does not exist", "no Flux", "no Argo"} {
			if strings.Contains(strings.ToLower(prose), strings.ToLower(claim)) {
				t.Errorf("engine %q: the kind list asserts %q about the cluster:\n%s", engine, claim, prose)
			}
		}
		if !strings.Contains(prose, "not evidence") {
			t.Errorf("engine %q: the kind list does not say an out-of-scope kind is not evidence "+
				"of absence:\n%s", engine, prose)
		}
	}
}
