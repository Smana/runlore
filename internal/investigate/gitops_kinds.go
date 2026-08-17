// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/Smana/runlore/internal/providers/gitops/argocd"
	"github.com/Smana/runlore/internal/providers/gitops/flux"
)

// GitOpsKinds returns the resource kinds the GitOps introspection tools can resolve on
// the configured engine. gitops_resource_status and gitops_tree resolve GitOps objects
// only — they are not a generic Kubernetes resource reader.
//
// The lists are DERIVED from the providers that do the resolving (flux.GitOpsKinds,
// argocd.GitOpsKinds) rather than restated here. The first version of this file kept
// its own copy and it drifted immediately: it omitted ExternalArtifact, which the Flux
// provider resolves and the chart grants RBAC for, so a model following a sourceRef to
// one — exactly what this repo's gitops-broken-kustomization eval renders — was told
// the kind is not a GitOps object and sent to pod_status. That is #503's defect with
// the sign flipped, and a restated list is what made it possible.
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
		return argocd.GitOpsKinds()
	}
	return flux.GitOpsKinds()
}

// canonicalGitOpsKind resolves kind to the exact spelling the engine's provider uses,
// reporting false when this engine cannot own it.
//
// Matching is case-insensitive because the model writes the kind as prose, so
// "helmrelease" must be ACCEPTED rather than refused as out of scope — but the canonical
// spelling is what the caller has to forward. The resolvers are exact-match maps
// (flux.kindToGVR, argocd.kindToGVR), so a lowercase kind that cleared a case-insensitive
// guard and was then passed on verbatim missed the map: gitops_tree swallows that
// resolution failure and rendered "helmrelease apps/api (Ready=unknown)" — a node for an
// object nobody ever looked up. Returning the canonical form is what keeps the guard and
// the resolver talking about the same object.
func canonicalGitOpsKind(engine, kind string) (string, bool) {
	kinds := GitOpsKinds(engine)
	if i := slices.IndexFunc(kinds, func(k string) bool {
		return strings.EqualFold(k, kind)
	}); i >= 0 {
		return kinds[i], true
	}
	return "", false
}

// gitopsKindSupported reports whether the tools can resolve kind on this engine.
func gitopsKindSupported(engine, kind string) bool {
	_, ok := canonicalGitOpsKind(engine, kind)
	return ok
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
