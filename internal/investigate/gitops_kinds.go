// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"encoding/json"
	"slices"
	"strings"

	"github.com/Smana/runlore/internal/providers"
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

// gitopsLookupAnswer renders a read that returned no object, as what the lookup
// ESTABLISHED rather than as a verdict on the cluster.
//
// The wording this replaces — "searched the given namespace, flux-system, and all
// namespaces by name, and no such object exists" — was wrong in three separate states,
// all of which reach it on the SUPPORTED-kind path that engine scoping does not touch:
//
//   - a CRD the API does not serve 404s exactly like an absent object, so #503's own
//     mechanism survived here untouched;
//   - a Forbidden on the all-namespaces List was swallowed, so the answer claimed a
//     cluster-wide search that RBAC had refused;
//   - on Argo CD flux-system is never searched, yet the sentence named it — and the
//     branch's own test asserted that string with Engine:"argocd".
//
// So the scopes come from the provider (only searches that actually completed are
// listed) and each reason gets its own answer. Nothing here says "does not exist": the
// earlier forbidden-word list contained that exact phrase and "no such object exists"
// slipped past it, which is why the wording is checked against the SHAPE of the claim
// below rather than a blocklist.
func gitopsLookupAnswer(id string, lk providers.Lookup) string {
	const notEvidence = " This is NOT evidence that the object is absent and NOT a root " +
		"cause — do not build a mechanism on it."
	switch lk.Reason {
	case providers.LookupKindNotServed:
		return id + ": the API server serves no such resource type on this cluster, so no " +
			"object of that kind could be returned by any query." + notEvidence +
			" It says the CRD is missing or not installed, which is a fact about the cluster's " +
			"API surface, not about this object."
	case providers.LookupDenied:
		return id + ": " + searchedClause(lk.Scopes) + " returned no object, and the " +
			"cluster-wide search by name was DENIED by RBAC, so it never ran." + notEvidence +
			" The object may exist in a namespace this agent cannot list."
	default:
		return id + ": NOT FOUND — " + searchedClause(lk.Scopes) + " returned no object of " +
			"that name. That is the result of a name lookup, not a diagnosis: whether an " +
			"absence explains the incident is for you to establish from other evidence, so do " +
			"not assume it is the root cause. If you expected it to exist, re-check the " +
			"kind/name and namespace before concluding it was deleted."
	}
}

// searchedClause names the scopes a provider actually completed. With none recorded it
// stays silent about scope rather than inventing one — the failure being fixed here is a
// message that named searches that never happened.
func searchedClause(scopes []string) string {
	if len(scopes) == 0 {
		return "the lookup"
	}
	return "a search of " + strings.Join(scopes, ", ")
}

// gitopsLookupNote is the one-line form for a dependency-tree node.
func gitopsLookupNote(lk providers.Lookup) string {
	switch lk.Reason {
	case providers.LookupKindNotServed:
		return "kind not served by this cluster's API (not evidence of absence)"
	case providers.LookupDenied:
		return "search DENIED by RBAC (not evidence of absence)"
	case providers.LookupUnresolvable:
		return "not a kind these tools resolve — not looked up (not evidence of absence)"
	case providers.LookupFailed:
		return "read FAILED (not evidence of absence)"
	default:
		return "not found by name"
	}
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

// gitopsKindProse renders the supported kinds for a tool description, naming the engine
// so the model is told why the list is short rather than left to infer a missing
// capability.
//
// What it deliberately does NOT say is what used to follow: "this deployment runs Argo
// CD; Flux kinds cannot exist here". That is a claim about the CLUSTER, made in the tool
// list on every turn, derived from a config string nothing validates — app.GitopsEngine
// maps "argo", "ArgoCD" and every other misspelling silently to flux — and false outright
// on a cluster running both engines mid-migration. Before this branch that case produced
// an honest tool ERROR, which the loop already reads as missing data; a fluent unhedged
// claim in the prompt is worse, because the model has nothing to notice.
//
// Which kinds this tool resolves is a fact about the tool, and that is all this says.
func gitopsKindProse(engine string) string {
	return "kind must be one of: " + strings.Join(GitOpsKinds(engine), ", ") +
		" — the GitOps kinds this deployment's " + engineDisplayName(engine) + " provider " +
		"resolves. Any other kind is outside this tool's scope, which is not evidence about " +
		"whether such an object exists; use pod_status, kube_events or query_metrics for those."
}

// engineFromTools reports the GitOps engine the REGISTERED tools are scoped to, empty
// when no GitOps introspection tool is wired.
//
// The system prompt is built from this rather than from a field on the investigator, so
// the prose and the schemas cannot disagree at any of the six LoopInvestigator
// construction sites. A prompt that tells the model to follow a Kustomization's sourceRef
// and read kustomize-controller's logs, next to a schema that refuses Flux kinds and a
// tool list with no controller_logs in it, is prompt/schema mismatch — which is how #503
// reached the model in the first place.
func engineFromTools(tools []Tool) string {
	for _, t := range tools {
		switch g := t.(type) {
		case GitOpsStatusTool:
			return g.Engine
		case GitOpsTreeTool:
			return g.Engine
		}
	}
	return ""
}

// engineDisplayName renders an engine for prose.
func engineDisplayName(engine string) string {
	if engine == "argocd" {
		return "Argo CD"
	}
	return "Flux"
}
