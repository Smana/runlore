// SPDX-License-Identifier: Apache-2.0

package app

import (
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	dynamicfake "k8s.io/client-go/dynamic/fake"

	"github.com/Smana/runlore/internal/config"
	argoexec "github.com/Smana/runlore/internal/executor/argocd"
	fluxexec "github.com/Smana/runlore/internal/executor/flux"
)

// TestBuildExecutorFollowsGitopsEngine pins the M3 wiring: the action executor
// must track gitops.engine exactly as BuildGitOps does, or approve-rung actions
// fail with "unsupported target kind" on one of the engines.
func TestBuildExecutorFollowsGitopsEngine(t *testing.T) {
	dc := dynamicfake.NewSimpleDynamicClient(runtime.NewScheme())

	cfg := &config.Config{}
	if _, ok := BuildExecutor(cfg, dc).(*fluxexec.Executor); !ok {
		t.Fatalf("default engine: got %T, want *flux.Executor", BuildExecutor(cfg, dc))
	}

	cfg.GitOps.Engine = "argocd"
	if _, ok := BuildExecutor(cfg, dc).(*argoexec.Executor); !ok {
		t.Fatalf("argocd engine: got %T, want *argocd.Executor", BuildExecutor(cfg, dc))
	}
}
