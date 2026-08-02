// SPDX-License-Identifier: Apache-2.0

package catalog

import (
	"strings"
	"testing"
)

// commonsDir is the shipped generic OKF bundle (examples/kb-commons) — the
// day-one catalog an adopter can point `catalog.dir` at before writing any
// entries of their own.
const commonsDir = "../../examples/kb-commons"

// minCommonsEntries is a floor, not the exact count: the bundle is meant to grow,
// and pinning the precise number would turn "someone added a playbook" into a
// failing test. It only guards the failure that matters — the loader silently
// indexing nothing (wrong path, reserved-name rule widened, frontmatter split
// broken) and kb_search returning empty on a fresh install.
const minCommonsEntries = 15

// TestKBCommonsBundleIndexes pins the two properties the commons bundle is built
// on, because both are invisible until an incident:
//
//  1. every entry is a resource-less Playbook. That is deliberate — these describe
//     a failure CLASS, not one cluster's workload — and it is exactly why they
//     cannot fire instant recall (recall's structural filter refuses a
//     resource-less entry against any workload-carrying alert; see
//     internal/investigate.resourceAgrees). A `resource:` sneaking into one of
//     these would silently claim a scope the entry does not have.
//  2. the loader actually indexes them. index.md is reserved and must stay out.
func TestKBCommonsBundleIndexes(t *testing.T) {
	c, err := New(commonsDir)
	if err != nil {
		t.Fatalf("New(%q): %v", commonsDir, err)
	}
	entries := c.Entries()
	if len(entries) < minCommonsEntries {
		t.Fatalf("indexed %d entries, want at least %d — the bundle is not being loaded", len(entries), minCommonsEntries)
	}

	for _, e := range entries {
		if strings.EqualFold(e.Path, "index.md") {
			t.Errorf("%s: reserved bundle file was indexed as an entry", e.Path)
		}
		if e.Type != "Playbook" {
			t.Errorf("%s: type = %q, want Playbook (commons entries are platform-wide procedures)", e.Path, e.Type)
		}
		if e.Resource != "" {
			t.Errorf("%s: resource = %q, want empty — a scoped resource here claims a cluster the commons bundle does not know", e.Path, e.Resource)
		}
		if len(e.Tags) == 0 {
			t.Errorf("%s: tags are empty; alert names live there and are a primary recall signal", e.Path)
		}
		if _, ok := ParseEntryDate(e.LastValidated); !ok {
			t.Errorf("%s: last_validated = %q is not a parseable OKF date, so age down-weighting ignores it", e.Path, e.LastValidated)
		}
	}
}

// TestKBCommonsSearchSurfacesTheRightEntry is the kb_search half: an
// investigation reaches these entries by query text only (no structural filter),
// so a query built from an alert name plus the words an on-call would type must
// land the matching playbook first. Each case names an alert whose text appears
// ONLY in the expected entry's corpus, which is what makes the assertion about
// retrieval rather than about wording luck.
func TestKBCommonsSearchSurfacesTheRightEntry(t *testing.T) {
	c, err := New(commonsDir)
	if err != nil {
		t.Fatalf("New(%q): %v", commonsDir, err)
	}

	cases := []struct {
		query    string
		wantPath string
	}{
		{"KubePodCrashLooping container CrashLoopBackOff restarts after deploy", "pod-crashloopbackoff-after-deploy"},
		{"KubeContainerOOMKilled container killed exit code 137 memory limit", "container-oomkilled"},
		{"KubePersistentVolumeFillingUp volume running out of space", "persistentvolume-filling-up"},
		{"FluxReconciliationFailure HelmRelease upgrade failed Ready False", "flux-helmrelease-upgrade-failed"},
		{"CertManagerCertNotReady acme order challenge pending", "certmanager-challenge-stuck"},
		{"CoreDNSErrorsHigh dns resolution failure no such host", "dns-resolution-failure"},
		{"KubeNodeNotReady node unreachable kubelet", "node-notready"},
		{"FailedScheduling pod pending insufficient cpu untolerated taint", "pod-unschedulable"},
	}
	for _, tc := range cases {
		t.Run(tc.wantPath, func(t *testing.T) {
			hits, err := c.Search(tc.query, 3)
			if err != nil {
				t.Fatalf("Search(%q): %v", tc.query, err)
			}
			if len(hits) == 0 {
				t.Fatalf("Search(%q) returned nothing", tc.query)
			}
			if !strings.Contains(hits[0].Path, tc.wantPath) {
				got := make([]string, len(hits))
				for i, h := range hits {
					got[i] = h.Path
				}
				t.Errorf("Search(%q) top hit = %s, want a path containing %q (all: %v)", tc.query, hits[0].Path, tc.wantPath, got)
			}
		})
	}
}
