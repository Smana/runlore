// SPDX-License-Identifier: Apache-2.0

package github

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
)

// TestAlertResourceRoundTrips is the wire test: the two halves of this fix live in
// different packages (curator writes, recall reads) and are unit-tested apart. If the
// frontmatter key does not survive renderEntry → disk → catalog.Load, both halves pass
// their own tests while the feature silently does nothing in production.
func TestAlertResourceRoundTrips(t *testing.T) {
	out := renderEntry(providers.KBEntry{
		Type:          "Incident",
		Title:         "Harbor Registry Down due to IAM Access Key Quota Limit",
		Description:   "AccessKey/xplane-harbor hit AccessKeysPerUser: 2.",
		Resource:      "tooling/harbor-registry",
		AlertResource: "tooling/harbor",
		Body:          "## Cause\nQuota.\n",
	})
	if !strings.Contains(out, "alert_resource: tooling/harbor") {
		t.Fatalf("renderEntry must emit the alert_resource frontmatter key:\n%s", out)
	}

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "iam.md"), []byte(out), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, skipped, err := catalog.Load(dir)
	if err != nil {
		t.Fatalf("catalog.Load: %v", err)
	}
	if len(skipped) != 0 {
		t.Fatalf("entry was skipped by the loader: %v", skipped)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 entry, got %d", len(entries))
	}
	e := entries[0]
	if e.Resource != "tooling/harbor-registry" {
		t.Fatalf("resource lost in round-trip: %q", e.Resource)
	}
	if e.AlertResource != "tooling/harbor" {
		t.Fatalf("alert_resource lost in round-trip: %q — the curator writes it and recall reads it, "+
			"but if it does not survive the file both halves pass their own tests while the fix does nothing", e.AlertResource)
	}
}

// TestAlertResourceOmittedFromFrontmatterWhenEmpty proves the key is not written as a
// dangling empty value on the overwhelming majority of entries (where the alert and the
// fault are the same resource), which would churn every existing file for no reason.
func TestAlertResourceOmittedFromFrontmatterWhenEmpty(t *testing.T) {
	out := renderEntry(providers.KBEntry{
		Type:     "Incident",
		Title:    "Something",
		Resource: "tooling/harbor",
		Body:     "## Cause\nx\n",
	})
	if strings.Contains(out, "alert_resource") {
		t.Fatalf("alert_resource must be omitted when empty (omitempty):\n%s", out)
	}
}
