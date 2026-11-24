// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/Smana/runlore/internal/provenance"
	"github.com/Smana/runlore/internal/providers"
)

func TestResolveRevertTargets(t *testing.T) {
	const (
		gitopsSHA = "abcdef1234567890abcdef1234567890abcdef12"
		prevSHA   = "1111111111111111111111111111111111111111"
		sourceSHA = "9999999999999999999999999999999999999999"
	)
	discard := slog.New(slog.NewTextHandler(io.Discard, nil))
	styles := provenance.DefaultHostStyles("", "", "")

	ctx := WithObservedChanges(context.Background())
	recordObservedGitOpsChange(ctx, "https://github.com/org/gitops-infra.git", prevSHA, gitopsSHA, providers.Diff{})
	recordObservedChange(ctx, "https://github.com/org/app-service.git", "v1.0.0", "v1.0.1", providers.Diff{}, sourceSHA)
	obs := observedChangesFrom(ctx)

	t.Run("single observed gitops commit stamps RevertTarget server-side", func(t *testing.T) {
		inv := providers.Investigation{
			Title: "payments CrashLoopBackOff",
			RootCauses: []providers.Hypothesis{{
				Summary:   "bad image bump",
				ChangeRef: gitopsSHA[:7],
				Evidence:  []string{"what_changed showed image bump"},
			}},
			Actions: []providers.Action{{
				Description: "Revert commit " + gitopsSHA[:7],
				Op:          "revert_pr",
				ChangeRef:   gitopsSHA[:7],
				// Forged model RevertTarget must be ignored and overwritten.
				Revert: &providers.RevertTarget{Repo: "evil/repo", CommitSHA: "0000000000000000000000000000000000000000"},
			}},
		}
		resolveRevertTargets(&inv, obs, styles, discard)
		got := inv.Actions[0]
		if got.Op != "revert_pr" {
			t.Fatalf("Op = %q, want revert_pr", got.Op)
		}
		if got.Revert == nil {
			t.Fatal("expected RevertTarget to be populated")
		}
		if got.Revert.Repo != "org/gitops-infra" || got.Revert.CommitSHA != gitopsSHA {
			t.Fatalf("Revert = %+v, want org/gitops-infra@%s", got.Revert, gitopsSHA)
		}
		if got.Revert.RepoURL != "https://github.com/org/gitops-infra" {
			t.Fatalf("RepoURL = %q", got.Revert.RepoURL)
		}
		if got.Revert.ProvenanceURL != "https://github.com/org/gitops-infra/commit/"+gitopsSHA {
			t.Fatalf("ProvenanceURL = %q", got.Revert.ProvenanceURL)
		}
	})

	t.Run("unobserved SHA strips op to suggestion", func(t *testing.T) {
		inv := providers.Investigation{
			Actions: []providers.Action{{
				Description: "Revert hallucinated commit",
				Op:          "revert_pr",
				ChangeRef:   "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
			}},
		}
		resolveRevertTargets(&inv, obs, styles, discard)
		if inv.Actions[0].Op != "" || inv.Actions[0].Revert != nil {
			t.Fatalf("unobserved SHA must strip Op and Revert, got %+v", inv.Actions[0])
		}
	})

	t.Run("source_diff commit strips op to suggestion", func(t *testing.T) {
		inv := providers.Investigation{
			Actions: []providers.Action{{
				Description: "Revert app source commit",
				Op:          "revert_pr",
				ChangeRef:   sourceSHA,
			}},
		}
		resolveRevertTargets(&inv, obs, styles, discard)
		if inv.Actions[0].Op != "" || inv.Actions[0].Revert != nil {
			t.Fatalf("source_diff commit must strip Op and Revert, got %+v", inv.Actions[0])
		}
	})

	t.Run("multi-commit range strips op to suggestion", func(t *testing.T) {
		inv := providers.Investigation{
			Actions: []providers.Action{{
				Description: "Revert range",
				Op:          "revert_pr",
				ChangeRef:   prevSHA[:7] + ".." + gitopsSHA[:7],
			}},
		}
		resolveRevertTargets(&inv, obs, styles, discard)
		if inv.Actions[0].Op != "" || inv.Actions[0].Revert != nil {
			t.Fatalf("multi-commit range must strip Op and Revert, got %+v", inv.Actions[0])
		}
	})
}
