// SPDX-License-Identifier: Apache-2.0

package app

import (
	"testing"

	"k8s.io/client-go/discovery/cached/memory"
)

// TestResourceSpecDiscoveryIsInvalidatable pins the one thing about this wiring that can
// break silently.
//
// The spec reader drops its memoised discovery and retries once when a Kind does not
// resolve — the difference between "a CRD installed after startup is readable" and "this
// cluster serves no such kind, for the pod's whole lifetime". It reaches Invalidate()
// through a TYPE ASSERTION, because the reader's discovery interface is deliberately
// narrow, and a type assertion that stops matching does not fail to compile: it just stops
// invalidating, and every stale answer is reported to the model as a fact about the
// cluster.
func TestResourceSpecDiscoveryIsInvalidatable(t *testing.T) {
	var client any = memory.NewMemCacheClient(nil)
	if _, ok := client.(interface{ Invalidate() }); !ok {
		t.Fatal("the memoised discovery client wired into resource_spec no longer implements " +
			"Invalidate(): a stale or failed discovery result is now permanent for the process")
	}
}
