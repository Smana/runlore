// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"encoding/json"
	"slices"
	"strings"
)

// The kinds each GitOps engine can own. gitops_resource_status and gitops_tree resolve
// GitOps objects only — they are not a generic Kubernetes resource reader.
var (
	argoCDGitOpsKinds = []string{"Application"}
	fluxGitOpsKinds   = []string{
		"Kustomization", "HelmRelease", "GitRepository",
		"OCIRepository", "HelmRepository", "HelmChart", "Bucket",
	}
)

// GitOpsKinds returns the resource kinds the GitOps introspection tools can resolve on
// the configured engine.
//
// It is engine-scoped for the same reason clusterTools registers controller_logs ONLY
// under Flux: a kind the configured engine cannot own is not merely unanswerable, it is
// answerable only with a MISLEADING negative. Flux objects can never exist on an Argo CD
// deployment, so asking about a HelmRelease there always reports "not found" — and a model
// reasonably reads that as "the workload is not deployed" rather than "you asked a question
// with no possible answer".
//
// That is not hypothetical. A live investigation cited three such calls as evidence:
//
//	gitops_resource_status(HelmRelease, coder, coder)         => NOT FOUND
//	gitops_resource_status(HelmRelease, coder, observability) => NOT FOUND
//	gitops_resource_status(Kustomization, coder, flux-system) => NOT FOUND
//
// Three authoritative negatives, zero information: the cluster runs engine "argocd" and has
// no Flux CRDs at all. Advertising only what the engine can own is what stops the question
// being asked, which is stronger than describing the trap in the answer.
//
// An empty engine yields the Flux set, mirroring app.GitopsEngine's own default so the
// default lives in one place rather than being restated here.
func GitOpsKinds(engine string) []string {
	if engine == "argocd" {
		return slices.Clone(argoCDGitOpsKinds)
	}
	return slices.Clone(fluxGitOpsKinds)
}

// gitopsKindSupported reports whether the tools can resolve kind on this engine. Matching
// is case-insensitive: the model writes the kind as prose and "helmrelease" should be
// refused for the same reason as "HelmRelease", not fall through to a lookup.
func gitopsKindSupported(engine, kind string) bool {
	return slices.ContainsFunc(GitOpsKinds(engine), func(k string) bool {
		return strings.EqualFold(k, kind)
	})
}

// gitopsUnsupportedKind is the answer for a kind this tool cannot resolve.
//
// It exists because the alternative — reusing the NOT FOUND wording — states a fact about
// the world in answer to a question about the tool's scope. Observed live: asked about a
// VMServiceScrape (never a supported kind), the tool replied "the object genuinely does not
// exist (likely the cascade root)"; the model quoted that as evidence, built its mechanism
// on it, and advised recovering the object from Git history while the object sat in the
// cluster the whole time.
//
// So this says what the tool is, and says explicitly what the answer is NOT. The negative
// claim is the load-bearing half: without it the model has no way to know the reply is
// silent about existence rather than asserting absence.
func gitopsUnsupportedKind(engine, kind string) string {
	return kind + " is not a GitOps object these tools can resolve (supported on this " +
		"deployment: " + strings.Join(GitOpsKinds(engine), ", ") + "). This says NOTHING " +
		"about whether the object exists — it is a statement about this tool's scope, not " +
		"about the cluster. Do NOT treat it as evidence of absence or as a root cause. To " +
		"check a non-GitOps object, use pod_status, kube_events or query_metrics."
}

// gitopsKindSchema builds the argument schema with kind constrained to what the engine can
// own. The enum is the part that actually prevents the failure: a description can be
// disregarded, whereas an out-of-enum value is refused before the call is made.
func gitopsKindSchema(engine string) string {
	enum, err := json.Marshal(GitOpsKinds(engine))
	if err != nil { // unreachable: a []string always marshals
		enum = []byte(`[]`)
	}
	return `{"type":"object","properties":{"kind":{"type":"string","enum":` + string(enum) +
		`},"name":{"type":"string"},"namespace":{"type":"string"}},` +
		`"required":["kind","name","namespace"]}`
}

// gitopsKindProse renders the supported kinds for a tool description, naming the engine so
// the model is told why the list is short rather than left to infer a missing capability.
func gitopsKindProse(engine string) string {
	if engine == "argocd" {
		return "kind must be one of: " + strings.Join(GitOpsKinds(engine), ", ") +
			" (this deployment runs Argo CD; Flux kinds cannot exist here)."
	}
	return "kind must be one of: " + strings.Join(GitOpsKinds(engine), ", ") +
		" (this deployment runs Flux; Argo CD kinds cannot exist here)."
}
