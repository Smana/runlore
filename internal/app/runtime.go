// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"strings"

	"github.com/Smana/runlore/internal/catalog"
)

// PodName returns this pod's identity for leader election.
func PodName() string {
	if n := os.Getenv("POD_NAME"); n != "" {
		return n
	}
	h, _ := os.Hostname()
	return h
}

// PodIP returns this pod's IP from the downward API (POD_IP, set by the chart).
// Empty outside Kubernetes or on a chart that predates #264 — the lease
// identity then degrades to the name-only format and leader forwarding is
// unavailable (followers answer 503 + Retry-After instead of proxying).
func PodIP() string {
	return os.Getenv("POD_IP")
}

// PodNamespace resolves the namespace from the downward API or the service-account mount.
func PodNamespace() string {
	if ns := os.Getenv("POD_NAMESPACE"); ns != "" {
		return ns
	}
	if b, err := os.ReadFile("/var/run/secrets/kubernetes.io/serviceaccount/namespace"); err == nil {
		if ns := strings.TrimSpace(string(b)); ns != "" {
			return ns
		}
	}
	return "default"
}

// ReadyFunc gates readiness on process + catalog health — and deliberately NOT
// on leadership (#264). Readiness used to also require holding the leader
// Lease, which kept every standby replica permanently NotReady: with
// replicaCount>1, `helm upgrade --wait` / kstatus (Flux helm-controller) could
// never observe the release Ready and timed out on every upgrade. Every warm
// replica now reports Ready; "only the leader processes work" is preserved by
// the forwarding layer instead (server.Forward proxies work-bearing requests
// from followers to the leader learned from the Lease).
//
// The catalog gate stays: a replica must NOT advertise ready until its
// knowledge base is loaded and warm — otherwise it would serve incident
// traffic blind (and, as a follower, could be promoted to leader cold). This
// distinguishes the two ways BuildCatalog returns a nil catalog:
//
//   - configured but the load failed (configured=true, cat=nil): stay 503. A
//     static catalog has no syncer to recover, so the pod stays not-ready and the
//     misconfiguration surfaces loudly instead of silently serving with no KB.
//   - not configured at all (configured=false, cat=nil): no catalog gate;
//     the replica is ready as soon as it serves.
//
// A configured-but-not-yet-warm catalog (git-sync NewEmpty, cat!=nil &&
// !Ready()) is also held at 503 until its first successful sync.
func ReadyFunc(cat *catalog.Catalog, configured bool) func() bool {
	return func() bool {
		if configured && (cat == nil || !cat.Ready()) {
			return false
		}
		if cat != nil && !cat.Ready() {
			return false
		}
		return true
	}
}
