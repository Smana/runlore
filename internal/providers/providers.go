// SPDX-License-Identifier: Apache-2.0

// Package providers defines the pluggable backend contracts RunLore is built on.
//
// Every backend the agent touches is an interface, so the investigation loop and
// the knowledge entries are written against engine-agnostic types (notably Change),
// never against Flux/ArgoCD/VictoriaMetrics/Prometheus directly.
//
// Core providers are built-in (direct clients) so the binary is self-contained;
// MCP is the extension layer for additional, optional tools.
//
// This file is the architecture contract. Method bodies live in sub-packages.
package providers

// ---- engine-agnostic "what changed" model ------------------------------------

// Engine identifies a GitOps engine.
type Engine string

// Supported GitOps engines. EngineAWS and EngineGCP mark a non-GitOps change from a
// cloud control plane (CloudTrail / Cloud Audit Logs), so cloud events join the same
// "what changed" model.
const (
	EngineFlux   Engine = "flux"
	EngineArgoCD Engine = "argocd"
	EngineAWS    Engine = "aws"
	EngineGCP    Engine = "gcp"
)

// ChangeType classifies a detected change.
type ChangeType string

// Change types detected on the cluster.
const (
	ChangeSync      ChangeType = "sync"       // a reconcile/sync applied a new revision
	ChangeChartBump ChangeType = "chart-bump" // a Helm chart version changed
	ChangeImageBump ChangeType = "image-bump" // a container image tag changed
	ChangeDrift     ChangeType = "drift"      // observed state diverged from desired
	ChangeCloudAPI  ChangeType = "cloud-api"  // a mutating cloud control-plane call (CloudTrail / Cloud Audit Logs)
)

// ResourceScope records whether a resource's KIND is namespaced, as a fact CARRIED
// with the resource rather than guessed from the kind's name.
//
// It is three-valued, and the third value is the load-bearing one. "Namespaced" and
// "cluster-scoped" are different answers, but "nobody established which" is different
// again — and a bool conflates it with one of them, silently. A Workload assembled
// from a cached alert, from a non-Kubernetes cloud event, or on a run with no cluster
// access has no discovery answer, and reading its zero value as "cluster-scoped" would
// strip a namespace that was a fact about the object. So the zero value is Unknown,
// every consumer must fall back to whatever it did before, and no existing
// construction site changes meaning by not setting the field.
type ResourceScope uint8

// The scopes a resource can be known to have.
const (
	// ScopeUnknown means nothing established this kind's scope: no cluster access, no
	// discovery answer, or not a Kubernetes object at all. Consumers must degrade to
	// their prior behaviour rather than assume either answer.
	ScopeUnknown ResourceScope = iota
	// ScopeNamespaced means the API server reports this kind as namespaced, so a
	// namespace qualifier on it is a fact about the object.
	ScopeNamespaced
	// ScopeClusterScoped means the API server reports this kind as cluster-scoped, so
	// the object HAS no namespace and any qualifier on it came from somewhere else.
	ScopeClusterScoped
)
