// SPDX-License-Identifier: Apache-2.0

package config

import (
	"os"
	"path/filepath"
	"testing"
)

// repoRoot walks up from the test's working directory until it finds go.mod, so
// shipped-file paths resolve regardless of where `go test` is invoked from.
func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("repo root (go.mod) not found")
		}
		dir = parent
	}
}

// TestShippedExampleConfigsLoad guards the config files RunLore ships to users
// against silent drift from the config structs. hack/demo.config.yaml is the
// zero-cluster quickstart the README points newcomers at — the literal first
// thing a user runs — so a field removed by a migration (as happened when
// enablement moved from triggers.*.enabled to sources.<name>) must fail loudly
// in CI here, not at the user's terminal. Add any new shipped example config to
// the list.
func TestShippedExampleConfigsLoad(t *testing.T) {
	root := repoRoot(t)
	shipped := []string{
		filepath.Join(root, "hack", "demo.config.yaml"),
	}
	for _, path := range shipped {
		t.Run(filepath.Base(path), func(t *testing.T) {
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("shipped config missing: %v", err)
			}
			c, err := Load(path)
			if err != nil {
				t.Fatalf("shipped config must load under the strict loader: %v", err)
			}
			if _, ok := c.Sources["alertmanager"]; !ok {
				t.Errorf("expected sources.alertmanager to be wired (the demo fires alertmanager webhooks)")
			}
		})
	}
}
