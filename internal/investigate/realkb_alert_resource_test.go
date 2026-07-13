// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// Replayed against the REAL catalog, not a fixture.
//
// testdata/realkb holds the two Harbor entries copied verbatim off a running RunLore's
// catalog PVC — real frontmatter, real bodies, written by RunLore itself.
//
// Production, 2026-07-13: alert HelmRelease/harbor InstallFailed (workload
// tooling/harbor). The catalog held the entry that explained it —
// harbor-registry-down-due-to-iam-access-key-quota-limit.md — but filed under
// `resource: tooling/harbor-registry`, where the fault IS. The structural gate compares
// against the resource the ALERT carries, so the entry was not even a candidate
// (no_resource_match) and a full paid investigation ran beside the answer.
func TestRealKBAlertResourceMakesTheEntryACandidate(t *testing.T) {
	const (
		dirReal = "testdata/realkb"
		iamFile = "harbor-registry-down-due-to-iam-access-key-quota-limit.md"
		capFile = "harbor-helmrelease-terminal-failed.md"
	)
	alert := providers.Workload{Namespace: "tooling", Name: "harbor"}

	src, err := os.ReadFile(filepath.Join(dirReal, iamFile))
	if err != nil {
		t.Fatal(err)
	}

	// BEFORE: the real entry, exactly as RunLore wrote it. Unreachable.
	before, _, err := catalog.Load(dirReal)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range before {
		if !strings.Contains(e.Path, "iam-access-key-quota") {
			continue
		}
		if e.AlertResource != "" {
			t.Fatalf("the real entry should carry no alert_resource yet, got %q", e.AlertResource)
		}
		if got := entryAgrees(alert, e, false); got != matchNone {
			t.Fatalf("the real entry must be structurally unreachable from tooling/harbor without alert_resource "+
				"(that is the bug); got %v", got)
		}
	}

	// AFTER: with the alert-side index the curator now writes.
	patched := strings.Replace(string(src),
		"resource: tooling/harbor-registry",
		"resource: tooling/harbor-registry\nalert_resource: tooling/harbor", 1)
	if patched == string(src) {
		t.Fatal("failed to patch the real entry's frontmatter")
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, iamFile), []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}
	capBody, err := os.ReadFile(filepath.Join(dirReal, capFile))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, capFile), capBody, 0o600); err != nil {
		t.Fatal(err)
	}

	entries, skipped, err := catalog.Load(dir)
	if err != nil {
		t.Fatalf("catalog.Load: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("loader skipped an entry: %v — alert_resource must not break parsing of a real entry", skipped)
	}
	var iam catalog.Entry
	for _, e := range entries {
		if strings.Contains(e.Path, "iam-access-key-quota") {
			iam = e
		}
	}
	if iam.AlertResource != "tooling/harbor" {
		t.Fatalf("alert_resource did not parse off the real entry: %q", iam.AlertResource)
	}
	if got := entryAgrees(alert, iam, false); got == matchNone {
		t.Fatal("with alert_resource recorded, the real IAM entry must become a recall CANDIDATE for the real alert — " +
			"this is what stops the next firing paying for a full investigation beside the answer")
	}
}
