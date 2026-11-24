// SPDX-License-Identifier: Apache-2.0

package action

import (
	"context"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

func validRevertAction(repo string) providers.Action {
	return providers.Action{
		Name:        "revert-commit",
		Description: "Revert broken commit abc1234",
		Op:          "revert_pr",
		Revert: &providers.RevertTarget{
			Repo:      repo,
			RepoURL:   "https://github.com/" + repo,
			CommitSHA: "abcdef1234567890abcdef1234567890abcdef12",
		},
	}
}

func TestAutoHardDeniesRevertPR(t *testing.T) {
	exec := &fakeExec{}
	pol := New(config.ActionPolicy{
		Mode: config.ActionAuto,
		Allow: config.ActionAllow{
			ReversibleOnly: true,
			Namespaces:     []string{"apps"},
			MaxBlastRadius: 10,
		},
		RevertPR: config.RevertPRPolicy{
			Enabled:      true,
			AllowedRepos: []string{"org/gitops-infra"},
		},
	})
	a := NewAuto(exec, config.AutoPolicy{MinConfidence: 0.5, MaxPerWindow: 10}, pol, audit.Nop{}, discardLog())
	a.Resume()

	act := validRevertAction("org/gitops-infra")
	act.Reversible = true // even if already set
	out := a.Run(context.Background(), autoInv(0.99, act))

	if len(exec.ran) != 0 {
		t.Fatalf("auto must NEVER execute revert_pr, got %d executions", len(exec.ran))
	}
	if len(out) != 1 || !strings.Contains(out[0].Description, "denied") {
		t.Fatalf("expected denied annotation for revert_pr under auto, got %+v", out)
	}
}

func TestPolicyReviewRevertPR(t *testing.T) {
	tests := []struct {
		name       string
		cfg        config.ActionPolicy
		act        providers.Action
		wantKept   bool
		wantReason string
	}{
		{
			name: "allowed when enabled and repo allowlisted",
			cfg: config.ActionPolicy{
				Mode: config.ActionApprove,
				Allow: config.ActionAllow{
					ReversibleOnly: true,
					MaxBlastRadius: 2,
					Kinds:          []string{"Kustomization"},
				},
				RevertPR: config.RevertPRPolicy{
					Enabled:      true,
					AllowedRepos: []string{"org/gitops-infra"},
				},
			},
			act:      validRevertAction("org/gitops-infra"),
			wantKept: true,
		},
		{
			name: "denied when revert_pr disabled",
			cfg: config.ActionPolicy{
				Mode:     config.ActionApprove,
				RevertPR: config.RevertPRPolicy{Enabled: false, AllowedRepos: []string{"org/gitops-infra"}},
			},
			act:        validRevertAction("org/gitops-infra"),
			wantReason: "not enabled",
		},
		{
			name: "denied when Revert is nil",
			cfg: config.ActionPolicy{
				Mode:     config.ActionApprove,
				RevertPR: config.RevertPRPolicy{Enabled: true, AllowedRepos: []string{"org/gitops-infra"}},
			},
			act: providers.Action{
				Name:        "revert-commit",
				Description: "Revert broken commit",
				Op:          "revert_pr",
			},
			wantReason: "revert target",
		},
		{
			name: "denied when repo not in allowed_repos",
			cfg: config.ActionPolicy{
				Mode:     config.ActionApprove,
				RevertPR: config.RevertPRPolicy{Enabled: true, AllowedRepos: []string{"org/gitops-infra"}},
			},
			act:        validRevertAction("org/other-repo"),
			wantReason: "allowed_repos",
		},
		{
			name: "denied when allowed_repos is empty",
			cfg: config.ActionPolicy{
				Mode:     config.ActionApprove,
				RevertPR: config.RevertPRPolicy{Enabled: true},
			},
			act:        validRevertAction("org/gitops-infra"),
			wantReason: "allowed_repos",
		},
		{
			name: "denied under auto mode at policy review too",
			cfg: config.ActionPolicy{
				Mode:     config.ActionAuto,
				RevertPR: config.RevertPRPolicy{Enabled: true, AllowedRepos: []string{"org/gitops-infra"}},
			},
			act:        validRevertAction("org/gitops-infra"),
			wantReason: "approve-only",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			p := New(tc.cfg)
			kept, withheld := p.Review([]providers.Action{tc.act})
			if tc.wantKept {
				if len(kept) != 1 || len(withheld) != 0 {
					t.Fatalf("kept=%d withheld=%v; want 1 kept", len(kept), withheld)
				}
				if !kept[0].Reversible || kept[0].BlastRadius != 2 {
					t.Fatalf("derived (reversible=%v, blast=%d); want (true, 2)", kept[0].Reversible, kept[0].BlastRadius)
				}
				return
			}
			if len(kept) != 0 || len(withheld) != 1 {
				t.Fatalf("kept=%d withheld=%v; want 0 kept, 1 withheld", len(kept), withheld)
			}
			if !strings.Contains(withheld[0], tc.wantReason) {
				t.Fatalf("withheld = %q, want substring %q", withheld[0], tc.wantReason)
			}
		})
	}
}

func TestApprovalsRevalidatesRevertPRAtExecBoundary(t *testing.T) {
	exec := &fakeExec{}
	cfg := config.ActionPolicy{
		Mode: config.ActionApprove,
		RevertPR: config.RevertPRPolicy{
			Enabled:      true,
			AllowedRepos: []string{"org/gitops-infra"},
		},
	}
	pol := New(cfg)
	ap := NewApprovals(exec, pol, audit.Nop{}, discardLog())

	// Register an action whose repo is NOT allowlisted (simulating TOCTOU / direct Register).
	id := ap.Register(validRevertAction("org/unlisted-repo"))
	if _, err := ap.Approve(context.Background(), id, "approve:slack:U1"); err == nil {
		t.Fatal("expected Approve to fail closed on unlisted repo")
	}
	if len(exec.ran) != 0 {
		t.Fatalf("executor must not run on denied revert_pr, got %d", len(exec.ran))
	}

	// Register a valid action and approve it.
	okID := ap.Register(validRevertAction("org/gitops-infra"))
	got, err := ap.Approve(context.Background(), okID, "approve:slack:U1")
	if err != nil {
		t.Fatalf("unexpected Approve error: %v", err)
	}
	if len(exec.ran) != 1 || got.Op != "revert_pr" {
		t.Fatalf("expected 1 revert_pr execution, got %d (op=%q)", len(exec.ran), got.Op)
	}
}
