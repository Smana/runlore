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

// ReadyFunc gates readiness on leadership AND a warm catalog. When a catalog is
// configured, the leader must NOT advertise ready until its knowledge base is
// loaded and warm — otherwise it would serve incident traffic blind. This
// distinguishes the two ways BuildCatalog returns a nil catalog:
//
//   - configured but the load failed (configured=true, cat=nil): stay 503. A
//     static catalog has no syncer to recover, so the pod stays not-ready and the
//     misconfiguration surfaces loudly instead of silently serving with no KB.
//   - not configured at all (configured=false, cat=nil): no catalog gate;
//     readiness is pure leadership.
//
// A configured-but-not-yet-warm catalog (git-sync NewEmpty, cat!=nil &&
// !Ready()) is also held at 503 until its first successful sync.
func ReadyFunc(leader func() bool, cat *catalog.Catalog, configured bool) func() bool {
	return func() bool {
		if configured && (cat == nil || !cat.Ready()) {
			return false
		}
		if cat != nil && !cat.Ready() {
			return false
		}
		return leader()
	}
}
