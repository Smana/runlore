// SPDX-License-Identifier: Apache-2.0

package app

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRestConfigForContextUnknownErrors(t *testing.T) {
	kubeconfig := filepath.Join(t.TempDir(), "kubeconfig")
	const raw = `apiVersion: v1
kind: Config
current-context: dev
clusters:
- name: dev-cluster
  cluster:
    server: https://127.0.0.1:6443
contexts:
- name: dev
  context:
    cluster: dev-cluster
    user: dev-user
users:
- name: dev-user
  user:
    token: dummy
`
	if err := os.WriteFile(kubeconfig, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	cfg, err := RestConfigForContext("dev")
	if err != nil {
		t.Fatalf("RestConfigForContext(dev): %v", err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Errorf("Host = %q, want https://127.0.0.1:6443", cfg.Host)
	}

	_, err = RestConfigForContext("nonexistent-context")
	if err == nil {
		t.Fatal("expected error for nonexistent kube context, got nil")
	}
	if !strings.Contains(err.Error(), "nonexistent-context") {
		t.Errorf("error = %q, want context name in message", err.Error())
	}
}
