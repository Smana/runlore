// SPDX-License-Identifier: Apache-2.0

package app

import (
	"log/slog"

	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/providers/cluster"
)

// KubeClientset builds a read-only clientset for pod-log access, or nil when no
// cluster is reachable (e.g. local runs without a kubeconfig).
func KubeClientset(log *slog.Logger) *kubernetes.Clientset {
	restCfg, err := RestConfig()
	if err != nil {
		return nil
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		log.Warn("clientset unavailable; controller_logs disabled", "err", err)
		return nil
	}
	return cs
}

// RestConfig builds a Kubernetes REST config from in-cluster config, falling back
// to the local kubeconfig (respecting $KUBECONFIG, then ~/.kube/config).
func RestConfig() (*rest.Config, error) {
	if cfg, err := rest.InClusterConfig(); err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}

// BuildResourceSpecReader builds the read-only arbitrary-object spec reader, or nil when
// the cluster is unreachable or discovery is unavailable.
//
// It needs BOTH a dynamic client and discovery: the typed clientset cannot read a CRD at
// all, and without discovery a kind can only be resolved from a hardcoded map — which is
// exactly the limitation that makes the Flux/ArgoCD inspectors blind to every kind they
// were not compiled with.
//
// Returning nil rather than a half-working reader is deliberate. A tool that cannot answer
// should not be registered, for the same reason controller_logs is gated on the engine: an
// unanswerable question gets answered with a misleading negative, and the model reasons
// from it.
//
// Discovery is memoised and lazy: the round trip happens on first use rather than at
// startup, so an unreachable API server delays a tool call instead of blocking boot.
func BuildResourceSpecReader(log *slog.Logger) providers.ResourceSpecReader {
	restCfg, err := RestConfig()
	if err != nil {
		return nil
	}
	dc, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		log.Warn("dynamic client unavailable; resource_spec disabled", "err", err)
		return nil
	}
	disco, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		log.Warn("discovery client unavailable; resource_spec disabled", "err", err)
		return nil
	}
	// Memoised discovery: resolving a Kind costs one round trip the first time and nothing
	// after. memcache caches FAILURES as permanently as successes, so the reader drops the
	// cache and retries once on a miss — otherwise a CRD installed after RunLore started
	// would be "this cluster serves no such kind" for the pod's whole lifetime, and one
	// aggregated-APIService blip would poison a group forever. That retry is behind a type
	// assertion on Invalidate(); TestResourceSpecDiscoveryIsInvalidatable pins that the
	// client wired here satisfies it.
	return cluster.NewSpecReader(dc, memory.NewMemCacheClient(disco))
}

// kindScoperFor exposes the discovery-backed scope resolver a spec reader also
// provides, or nil when it provides none.
//
// The assertion is what lets Workload.Scope be populated from the API server's own
// discovery instead of guessed downstream from a kind's NAME — and it is silent when it
// stops holding: nothing fails to compile, delivered resources simply carry
// ScopeUnknown again and every consumer falls back to its old behaviour. That is safe,
// which is exactly why it takes a test to notice — see kind_scope_wiring_test.go.
//
// Nil in, nil out — a nil reader must not become a non-nil interface holding nothing.
func kindScoperFor(sr providers.ResourceSpecReader) providers.KindScoper {
	if sr == nil {
		return nil
	}
	ks, ok := sr.(providers.KindScoper)
	if !ok {
		return nil
	}
	return ks
}

// kindScoperFromTools takes the scope resolver from the resource_spec tool already
// registered, or nil when none was (no cluster, no discovery — an eval replay, the
// demo, a local run without a kubeconfig).
//
// Reading it back off the built tool rather than calling BuildResourceSpecReader again
// keeps ONE memoised discovery client in the process. A second one would mean a second
// full discovery fan-out and two caches going stale independently — the same
// build-it-twice defect Deps was introduced to fix for the catalog.
func kindScoperFromTools(tools []investigate.Tool) providers.KindScoper {
	for _, t := range tools {
		if rst, ok := t.(investigate.ResourceSpecTool); ok {
			return kindScoperFor(rst.Reader)
		}
	}
	return nil
}
