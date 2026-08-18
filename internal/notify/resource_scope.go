// SPDX-License-Identifier: Apache-2.0

package notify

import (
	"strings"

	"github.com/Smana/runlore/internal/providers"
)

// clusterScopedKinds are Kubernetes kinds that HAVE no namespace, so a namespace
// qualifier on one is never a fact about the object — it is whatever namespace
// happened to be in scope when the workload was assembled (see resourceRef).
//
// Keys are lowercased; lookups go through kindKey.
//
// Well-known built-ins first, then the third-party CRDs whose objects show up in
// alert-driven investigations on a typical cluster. The list is deliberately allowed
// to be incomplete: an unlisted kind falls through to "namespaced", which is what
// the renderer already did, so a missing entry costs nothing that is not already the
// status quo — while a WRONG entry would strip a real namespace off a real object.
// That asymmetry is why nothing is guessed here.
var clusterScopedKinds = map[string]struct{}{
	// core
	"node":             {},
	"namespace":        {},
	"persistentvolume": {},
	"componentstatus":  {},
	// rbac.authorization.k8s.io
	"clusterrole":        {},
	"clusterrolebinding": {},
	// storage.k8s.io
	"storageclass":     {},
	"volumeattachment": {},
	"csidriver":        {},
	"csinode":          {},
	// apiextensions / apiregistration
	"customresourcedefinition": {},
	"apiservice":               {},
	// admissionregistration.k8s.io
	"mutatingwebhookconfiguration":     {},
	"validatingwebhookconfiguration":   {},
	"validatingadmissionpolicy":        {},
	"validatingadmissionpolicybinding": {},
	// scheduling / node / networking / certificates / flowcontrol
	"priorityclass":              {},
	"runtimeclass":               {},
	"ingressclass":               {},
	"certificatesigningrequest":  {},
	"flowschema":                 {},
	"prioritylevelconfiguration": {},
	// third-party, cluster-scoped by design
	"gatewayclass":  {}, // gateway.networking.k8s.io
	"clusterissuer": {}, // cert-manager.io
	"clusterpolicy": {}, // kyverno.io
	"nodepool":      {}, // karpenter.sh — NOT hypershift.openshift.io's, which is namespaced
	"nodeclaim":     {}, // karpenter.sh
	"ec2nodeclass":  {}, // karpenter.k8s.aws
}

// nonKubernetesKinds are cloud resources RunLore reasons about that are not
// Kubernetes objects at all — an RDS instance has no namespace to be in, so
// "DBInstance observability/datagrok-aqemia-shared" names a namespace that does not
// exist anywhere in the world the reader can go and look at.
//
// The bar for an entry is that the name is unambiguous ON THE PLATFORM RunLore runs
// against. "Queue", "Function" and "Volume" are pointedly ABSENT even though AWS has
// all three: each is also a namespaced CRD kind in a widely-deployed operator
// (RabbitMQ, OpenFaaS, Longhorn), and stripping a real namespace off a real object is
// the failure this whole file exists to avoid.
//
// That bar is NOT the stronger "no namespaced homonym exists anywhere", and the gap
// is the first thing whoever edits this list next needs to know. AWS Controllers for
// Kubernetes (ACK) ships NAMESPACED CRDs spelled exactly like most of these —
// DBInstance, DBCluster, DBSubnetGroup, DBParameterGroup, CacheCluster, Nodegroup,
// LoadBalancer, TargetGroup, LaunchTemplate — and Crossplane v2 made its managed
// resources namespaced too. Workload carries only Kind/Name/Namespace, no group and
// no apiVersion, so nothing here can tell an ACK DBInstance from the RDS instance the
// delivered card misnamed. On a cluster running ACK or Crossplane v2 these entries
// WOULD strip a true namespace; revisit the list before pointing RunLore at one.
//
// Kinds spelled as a cloud identifier rather than a Kubernetes kind — CloudTrail's
// "AWS::RDS::DBInstance", an ARN — need no entry here: notKubernetesShaped rejects
// them structurally.
var nonKubernetesKinds = map[string]struct{}{
	// RDS — the family the delivered "DBInstance observability/…" card came from.
	"dbinstance":        {},
	"dbcluster":         {},
	"dbsnapshot":        {},
	"dbclustersnapshot": {},
	"dbparametergroup":  {},
	"dbsubnetgroup":     {},
	"cachecluster":      {}, // ElastiCache
	// The AWS objects RunLore's own cloud readers describe by name
	// (internal/providers/cloud/aws): EKS node groups, ASGs and their load balancing.
	"nodegroup":        {},
	"autoscalinggroup": {},
	"launchtemplate":   {},
	"loadbalancer":     {}, // ELB/ALB — the Kubernetes spelling is Service or Ingress
	"targetgroup":      {},
}

// kindKey normalizes a kind for lookup: the field is free text (submit_findings lets
// the model write it), so "node", "Node" and " Node " are the same kind.
func kindKey(kind string) string {
	return strings.ToLower(strings.TrimSpace(kind))
}

// notKubernetesShaped reports whether kind holds a colon — a character no Kubernetes
// kind can contain — so it names something outside Kubernetes entirely: a CloudTrail
// resource type ("AWS::RDS::DBInstance"), an ARN ("arn:aws:rds:eu-west-1:…:db/x").
// Enumerating every cloud type would be endless, so the shape answers for them.
//
// ':' ALONE, deliberately, and narrower than "is this a well-formed Kubernetes kind".
// The field is model-written free text — submit_findings declares it as a bare
// {"kind":{"type":"string"}} with no enum — and every OTHER malformed spelling a
// model reaches for is a real, NAMESPACED object wearing a bad name:
//
//   - dotted or hyphenated: "helmreleases.helm.toolkit.fluxcd.io", "helm-release"
//   - slash-qualified:      "apps/Deployment", "v1/Pod", "Deployment/checkout-api"
//   - spaced:               "Stateful Set", "Persistent Volume Claim", "Cron Job"
//
// Reading '/' or whitespace as "not Kubernetes" would strip a true namespace off
// every one of those — the one new failure this file must not introduce — and would
// buy nothing, because both motivating examples above carry a ':' anyway.
func notKubernetesShaped(kind string) bool {
	return strings.Contains(kind, ":")
}

// unnamespacedKind reports whether a namespace qualifier on this kind is not a fact
// about the object: a cluster-scoped Kubernetes kind, or something that is not a
// Kubernetes object at all.
//
// Fail-safe by construction — it answers false for an empty kind and for every kind
// it does not recognize, so the renderer keeps doing exactly what it does today
// unless the namespace is known to be wrong.
//
// "Known" is as strong as this layer can be, not a guarantee: Workload carries no
// group or apiVersion, so the nonKubernetesKinds caveat above (ACK and Crossplane v2
// ship NAMESPACED CRDs under several of those names) is a limit on this function too.
// Read it before adding an entry.
func unnamespacedKind(kind string) bool {
	k := kindKey(kind)
	if k == "" {
		return false
	}
	if _, ok := clusterScopedKinds[k]; ok {
		return true
	}
	if _, ok := nonKubernetesKinds[k]; ok {
		return true
	}
	return notKubernetesShaped(k)
}

// resourceRef renders the affected resource's identity for a card: "namespace/name"
// for a namespaced object, and the bare name for one that has no namespace.
//
// It exists because Workload.Ref() is namespace-first unconditionally, and the
// namespace on an alert-driven investigation is NOT necessarily the object's own.
// The alert's `namespace` label is the namespace of whatever exported the series —
// kube-state-metrics, the exporter, the alerting rule — and the investigation loop
// defaults a nameless-namespace discovery to it (investigate.preferDiscoveredResource).
// For a cluster-scoped or cloud object that produced delivered cards reading
// "Node observability/ip-10-11-132-8.ec2.internal" and "DBInstance
// observability/datagrok-aqemia-shared": `observability` is where the metrics come
// from, not where the node or the database is, and an operator who believes the card
// goes looking in the wrong place.
//
// This is the RENDERING half of the fix only. The conflation upstream is real — the
// same wrong namespace still reaches recall matching, the curated entry's
// `resource:` frontmatter and the outcome ledger — and belongs where the workload is
// assembled, not here.
func resourceRef(w providers.Workload) string {
	if !unnamespacedKind(w.Kind) {
		return w.Ref()
	}
	if name := strings.TrimSpace(w.Name); name != "" {
		return name
	}
	// A Namespace object is the one cluster-scoped kind whose OWN identity can arrive
	// in the namespace field: "the coder-engineering namespace" is a resource a model
	// names with no workload inside it, and preferDiscoveredResource deliberately keeps
	// that shape (a namespace, no name). Here alone the qualifier is not foreign, so
	// reading it as one and dropping it would discard the object's actual name.
	if kindKey(w.Kind) == "namespace" {
		return strings.TrimSpace(w.Namespace)
	}
	return ""
}

// resourceLine renders the whole "Kind namespace/name" value every surface prints
// for the affected resource — "Node ip-10-11-132-8.ec2.internal" for a cluster-scoped
// object, "Pod observability/vector-7f9c" for a namespaced one — or "" when the
// investigation named no resource, so the caller omits the line entirely.
//
// One function for both the Slack card and Format, because a resource line that says
// two different things about one investigation on two surfaces is the drift Format's
// own doc comment is written against.
func resourceLine(w providers.Workload) string {
	// The kind is trimmed BEFORE it is joined, not after: the field is model-written
	// free text, and trimming the joined string leaves a padded " Node " rendering as
	// "Node␣␣ip-10-11-132-8.ec2.internal" — the ends are clean, the seam is not.
	kind, ref := strings.TrimSpace(w.Kind), resourceRef(w)
	switch {
	case ref == "" && strings.TrimSpace(w.Namespace) == "":
		// Nothing to name and nothing to scope it to: no fact beyond the kind, which
		// is what the renderer already omitted.
		return ""
	case ref == "":
		// A cluster-scoped kind whose name is unknown but whose namespace was stamped:
		// the kind alone, because printing that namespace as the object's own is the
		// one thing that must never happen.
		return kind
	case kind == "":
		return ref
	}
	return kind + " " + ref
}

// scopeIdentity returns the cluster and tenant a card should render, blanking the
// tenant when it only repeats the cluster's own name.
//
// "Cluster: shared · shared" is what a single-tenant cluster produced: both labels
// carry the same value and the card says it twice, which reads like a rendering
// fault and costs a line of the fold budget. Where they genuinely differ
// ("tmem175 · tmem175-0" — the tenant and the cluster it lives on) the pair is the
// point, so only the equal case collapses; the tenant is never simply dropped.
//
// Compared trimmed AND returned trimmed, in both branches: when the two agree modulo
// whitespace they name the same thing, so the canonical spelling is the one to print
// — and when they differ, padding is no more worth rendering than it was in the case
// that collapses. Trimming only the collapse branch left "  shared · shared-0" on the
// card, which is the same rendering fault this function exists to remove.
func scopeIdentity(cluster, tenant string) (outCluster, outTenant string) {
	cluster, tenant = strings.TrimSpace(cluster), strings.TrimSpace(tenant)
	if cluster == tenant {
		tenant = ""
	}
	return cluster, tenant
}
