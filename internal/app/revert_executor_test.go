// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

type recordingExecutor struct {
	ops []string
}

func (r *recordingExecutor) Execute(_ context.Context, a providers.Action) error {
	r.ops = append(r.ops, a.Op)
	return nil
}

func TestCompositeExecutorDispatch(t *testing.T) {
	clusterExec := &recordingExecutor{}
	revertExec := &recordingExecutor{}
	exec := CombineExecutors(clusterExec, revertExec)

	if err := exec.Execute(context.Background(), providers.Action{Op: "suspend"}); err != nil {
		t.Fatalf("suspend error: %v", err)
	}
	if err := exec.Execute(context.Background(), providers.Action{Op: "revert_pr"}); err != nil {
		t.Fatalf("revert_pr error: %v", err)
	}

	if len(clusterExec.ops) != 1 || clusterExec.ops[0] != "suspend" {
		t.Fatalf("clusterExec ops = %v, want [suspend]", clusterExec.ops)
	}
	if len(revertExec.ops) != 1 || revertExec.ops[0] != "revert_pr" {
		t.Fatalf("revertExec ops = %v, want [revert_pr]", revertExec.ops)
	}
}

func TestCompositeExecutorFailsClosedWhenBackendMissing(t *testing.T) {
	exec := CompositeExecutor{Cluster: &recordingExecutor{}, Revert: nil}
	if err := exec.Execute(context.Background(), providers.Action{Op: "revert_pr"}); err == nil {
		t.Fatal("expected error when Revert executor is nil")
	}

	exec2 := CompositeExecutor{Cluster: nil, Revert: &recordingExecutor{}}
	if err := exec2.Execute(context.Background(), providers.Action{Op: "suspend"}); err == nil {
		t.Fatal("expected error when Cluster executor is nil")
	}
}

func TestBuildRevertForgeCredentialSelection(t *testing.T) {
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))

	t.Setenv("TEST_REVERT_GL_TOK", "glpat-dedicated")
	t.Setenv("TEST_FORGE_GL_TOK", "glpat-shared")

	// Disabled => nil.
	if got := BuildRevertForge(&config.Config{}, discard); got != nil {
		t.Fatalf("disabled revert_pr must return nil forge, got %T", got)
	}

	// Dedicated GitLab credentials.
	cfgDedicated := &config.Config{
		Actions: config.ActionPolicy{
			Mode: config.ActionApprove,
			RevertPR: config.RevertPRPolicy{
				Enabled:      true,
				AllowedRepos: []string{"group/gitops"},
				Forge: &config.RevertPRForge{
					Provider: "gitlab",
					GitLab:   config.GitLab{TokenEnv: "TEST_REVERT_GL_TOK"},
				},
			},
		},
	}
	if got := BuildRevertForge(cfgDedicated, discard); got == nil {
		t.Fatal("expected non-nil RevertForge for dedicated gitlab credentials")
	}

	// Explicit reuse_forge_credentials: true.
	cfgReuse := &config.Config{
		Forge: config.Forge{
			Provider: "gitlab",
			GitLab:   config.GitLab{TokenEnv: "TEST_FORGE_GL_TOK"},
		},
		Actions: config.ActionPolicy{
			Mode: config.ActionApprove,
			RevertPR: config.RevertPRPolicy{
				Enabled:               true,
				AllowedRepos:          []string{"group/gitops"},
				ReuseForgeCredentials: true,
			},
		},
	}
	if got := BuildRevertForge(cfgReuse, discard); got == nil {
		t.Fatal("expected non-nil RevertForge when reuse_forge_credentials is true")
	}

	// reuse_forge_credentials: false with no dedicated forge => nil (fail closed).
	cfgNoOptIn := &config.Config{
		Forge: config.Forge{
			Provider: "gitlab",
			GitLab:   config.GitLab{TokenEnv: "TEST_FORGE_GL_TOK"},
		},
		Actions: config.ActionPolicy{
			Mode: config.ActionApprove,
			RevertPR: config.RevertPRPolicy{
				Enabled:               true,
				AllowedRepos:          []string{"group/gitops"},
				ReuseForgeCredentials: false,
			},
		},
	}
	if got := BuildRevertForge(cfgNoOptIn, discard); got != nil {
		t.Fatalf("expected nil RevertForge when reuse_forge_credentials is false and no dedicated forge, got %T", got)
	}
}
