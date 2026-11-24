// SPDX-License-Identifier: Apache-2.0

package revertpr

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/providers"
)

type fakeRevertForge struct {
	defaultBranch string
	headSHA       string
	detail        providers.CommitDetail
	laterFiles    []string
	blobs         map[string][]byte // key: ref + ":" + path
	existingURL   string
	existingNum   int
	existingFound bool

	createCalls int
	lastReq     providers.RevertPRRequest
	createURL   string
	createNum   int
	createErr   error
}

func (f *fakeRevertForge) DefaultBranch(_ context.Context, _ string) (string, string, error) {
	return f.defaultBranch, f.headSHA, nil
}

func (f *fakeRevertForge) CommitDetail(_ context.Context, _, _ string) (providers.CommitDetail, error) {
	return f.detail, nil
}

func (f *fakeRevertForge) CompareChangedFiles(_ context.Context, _, _, _ string) ([]string, error) {
	return f.laterFiles, nil
}

func (f *fakeRevertForge) FileBlob(_ context.Context, _, ref, path string) ([]byte, bool, error) {
	b, ok := f.blobs[ref+":"+path]
	return b, ok, nil
}

func (f *fakeRevertForge) FindOpenPRByHead(_ context.Context, _, _ string) (string, int, bool, error) {
	return f.existingURL, f.existingNum, f.existingFound, nil
}

func (f *fakeRevertForge) CreateRevertDraftPR(_ context.Context, req providers.RevertPRRequest) (string, int, error) {
	f.createCalls++
	f.lastReq = req
	if f.createErr != nil {
		return "", 0, f.createErr
	}
	return f.createURL, f.createNum, nil
}

const (
	testCommitSHA = "1234567890abcdef1234567890abcdef12345678"
	testParentSHA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testHeadSHA   = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func baseAction() providers.Action {
	return providers.Action{
		Name:        "revert-commit",
		Description: "Revert broken deployment",
		Op:          "revert_pr",
		ApprovalID:  "apr-123",
		Revert: &providers.RevertTarget{
			Repo:            "org/gitops-infra",
			RepoURL:         "https://github.com/org/gitops-infra",
			CommitSHA:       testCommitSHA,
			IncidentTitle:   "CrashLoopBackOff in payments (password=supersecret)",
			IncidentSummary: "Image bump broke startup",
			Evidence:        []string{"pod logs: token=ghp_0123456789abcdefghijklmnopqrstuvwxyzAB"},
			ProvenanceURL:   "https://github.com/org/gitops-infra/commit/" + testCommitSHA,
		},
	}
}

func TestExecuteCleanRevertAllFileStatuses(t *testing.T) {
	forge := &fakeRevertForge{
		defaultBranch: "main",
		headSHA:       testCommitSHA,
		detail: providers.CommitDetail{
			SHA:     testCommitSHA,
			Parents: []string{testParentSHA},
			Message: "bump payments to v2.0.0\n\nSigned-off-by: dev",
			Files: []providers.CommitFile{
				{Path: "apps/payments/added.yaml", Status: "added"},
				{Path: "apps/payments/deploy.yaml", Status: "modified"},
				{Path: "apps/payments/old.yaml", Status: "removed"},
				{Path: "apps/payments/new-name.yaml", Status: "renamed", PreviousPath: "apps/payments/prev-name.yaml"},
			},
		},
		blobs: map[string][]byte{
			testParentSHA + ":apps/payments/deploy.yaml":    []byte("image: payments:v1.9.9\n"),
			testParentSHA + ":apps/payments/old.yaml":       []byte("kind: ConfigMap\n"),
			testParentSHA + ":apps/payments/prev-name.yaml": []byte("kind: Service\n"),
		},
		createURL: "https://github.com/org/gitops-infra/pull/42",
		createNum: 42,
	}

	var openedURL string
	var openedReused bool
	exec := New(forge, []string{"org/gitops-infra"}, nil, WithOnOpened(func(_ context.Context, _ providers.Action, url string, reused bool) {
		openedURL = url
		openedReused = reused
	}))

	act := baseAction()
	if err := exec.Execute(context.Background(), act); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if forge.createCalls != 1 {
		t.Fatalf("createCalls = %d, want 1", forge.createCalls)
	}
	req := forge.lastReq
	if req.HeadBranch != "runlore/revert-1234567890ab" {
		t.Fatalf("HeadBranch = %q, want runlore/revert-1234567890ab", req.HeadBranch)
	}
	if req.BaseBranch != "main" || req.BaseSHA != testCommitSHA {
		t.Fatalf("Base = %s@%s, want main@%s", req.BaseBranch, req.BaseSHA, testCommitSHA)
	}
	if !strings.Contains(req.CommitMessage, `Revert "bump payments to v2.0.0"`) || !strings.Contains(req.CommitMessage, testCommitSHA) {
		t.Fatalf("CommitMessage = %q", req.CommitMessage)
	}
	// Verify secrets are redacted from the PR body.
	if strings.Contains(req.Body, "supersecret") || strings.Contains(req.Body, "ghp_0123456789") {
		t.Fatalf("secret leaked into PR body: %q", req.Body)
	}
	// Expect 5 file actions: delete added.yaml, restore deploy.yaml, restore old.yaml, delete new-name.yaml, restore prev-name.yaml.
	byPath := map[string]providers.RevertFileAction{}
	for _, fa := range req.Files {
		byPath[fa.Path] = fa
	}
	if len(byPath) != 5 {
		t.Fatalf("got %d file actions, want 5: %+v", len(byPath), req.Files)
	}
	if !byPath["apps/payments/added.yaml"].Delete {
		t.Error("added.yaml must be deleted")
	}
	if !byPath["apps/payments/new-name.yaml"].Delete {
		t.Error("new-name.yaml (rename destination) must be deleted")
	}
	if got := byPath["apps/payments/deploy.yaml"].Content; !bytes.Equal(got, []byte("image: payments:v1.9.9\n")) {
		t.Errorf("deploy.yaml content = %q", got)
	}
	if got := byPath["apps/payments/old.yaml"].Content; !bytes.Equal(got, []byte("kind: ConfigMap\n")) {
		t.Errorf("old.yaml content = %q", got)
	}
	if got := byPath["apps/payments/prev-name.yaml"].Content; !bytes.Equal(got, []byte("kind: Service\n")) {
		t.Errorf("prev-name.yaml content = %q", got)
	}
	if act.Revert.ResultURL != forge.createURL {
		t.Fatalf("ResultURL = %q, want %q", act.Revert.ResultURL, forge.createURL)
	}
	if openedURL != forge.createURL || openedReused {
		t.Fatalf("onOpened = (%q, reused=%v), want (%q, false)", openedURL, openedReused, forge.createURL)
	}
}

func TestExecuteRefusals(t *testing.T) {
	manyFiles := make([]providers.CommitFile, 51)
	for i := range manyFiles {
		manyFiles[i] = providers.CommitFile{Path: fmt.Sprintf("apps/svc/f%d.yaml", i), Status: "modified"}
	}

	tests := []struct {
		name           string
		protectedPaths []string
		forge          *fakeRevertForge
		wantErr        string
	}{
		{
			name: "merge commit refused",
			forge: &fakeRevertForge{
				defaultBranch: "main",
				headSHA:       testCommitSHA,
				detail: providers.CommitDetail{
					SHA:     testCommitSHA,
					Parents: []string{testParentSHA, "cccccccccccccccccccccccccccccccccccccccc"},
					Files:   []providers.CommitFile{{Path: "apps/web/deploy.yaml", Status: "modified"}},
				},
			},
			wantErr: "merge commit",
		},
		{
			name: "root commit refused",
			forge: &fakeRevertForge{
				defaultBranch: "main",
				headSHA:       testCommitSHA,
				detail: providers.CommitDetail{
					SHA:     testCommitSHA,
					Parents: nil,
					Files:   []providers.CommitFile{{Path: "apps/web/deploy.yaml", Status: "added"}},
				},
			},
			wantErr: "parent",
		},
		{
			name: "more than 50 files refused",
			forge: &fakeRevertForge{
				defaultBranch: "main",
				headSHA:       testCommitSHA,
				detail: providers.CommitDetail{
					SHA:     testCommitSHA,
					Parents: []string{testParentSHA},
					Files:   manyFiles,
				},
			},
			wantErr: "50",
		},
		{
			name: "built-in protected path flux-system refuses entire commit",
			forge: &fakeRevertForge{
				defaultBranch: "main",
				headSHA:       testCommitSHA,
				detail: providers.CommitDetail{
					SHA:     testCommitSHA,
					Parents: []string{testParentSHA},
					Files: []providers.CommitFile{
						{Path: "apps/payments/deploy.yaml", Status: "modified"},
						{Path: "clusters/prod/flux-system/gotk-sync.yaml", Status: "modified"},
					},
				},
			},
			wantErr: "protected path",
		},
		{
			name: "built-in protected path kube-system refused",
			forge: &fakeRevertForge{
				defaultBranch: "main",
				headSHA:       testCommitSHA,
				detail: providers.CommitDetail{
					SHA:     testCommitSHA,
					Parents: []string{testParentSHA},
					Files:   []providers.CommitFile{{Path: "manifests/kube-system/coredns.yaml", Status: "modified"}},
				},
			},
			wantErr: "protected path",
		},
		{
			name: "built-in protected path .github workflow refused",
			forge: &fakeRevertForge{
				defaultBranch: "main",
				headSHA:       testCommitSHA,
				detail: providers.CommitDetail{
					SHA:     testCommitSHA,
					Parents: []string{testParentSHA},
					Files:   []providers.CommitFile{{Path: ".github/workflows/deploy.yaml", Status: "modified"}},
				},
			},
			wantErr: "protected path",
		},
		{
			name: "built-in protected path .gitlab-ci.yml refused",
			forge: &fakeRevertForge{
				defaultBranch: "main",
				headSHA:       testCommitSHA,
				detail: providers.CommitDetail{
					SHA:     testCommitSHA,
					Parents: []string{testParentSHA},
					Files:   []providers.CommitFile{{Path: ".gitlab-ci.yml", Status: "modified"}},
				},
			},
			wantErr: "protected path",
		},
		{
			name:           "operator protected_paths glob refused",
			protectedPaths: []string{"infra/iam/**"},
			forge: &fakeRevertForge{
				defaultBranch: "main",
				headSHA:       testCommitSHA,
				detail: providers.CommitDetail{
					SHA:     testCommitSHA,
					Parents: []string{testParentSHA},
					Files:   []providers.CommitFile{{Path: "infra/iam/prod/role.yaml", Status: "modified"}},
				},
			},
			wantErr: "protected path",
		},
		{
			name: "overlapping later edit between C and HEAD refused",
			forge: &fakeRevertForge{
				defaultBranch: "main",
				headSHA:       testHeadSHA,
				detail: providers.CommitDetail{
					SHA:     testCommitSHA,
					Parents: []string{testParentSHA},
					Files:   []providers.CommitFile{{Path: "apps/payments/deploy.yaml", Status: "modified"}},
				},
				laterFiles: []string{"apps/other/svc.yaml", "apps/payments/deploy.yaml"},
			},
			wantErr: "apps/payments/deploy.yaml",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			exec := New(tc.forge, []string{"org/gitops-infra"}, tc.protectedPaths)
			err := exec.Execute(context.Background(), baseAction())
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error = %q, want substring %q", err.Error(), tc.wantErr)
			}
			if tc.forge.createCalls != 0 {
				t.Fatalf("CreateRevertDraftPR must not be called on refusal, got %d calls", tc.forge.createCalls)
			}
		})
	}
}

func TestExecuteSucceedsWhenHeadMovedOnUnrelatedFiles(t *testing.T) {
	forge := &fakeRevertForge{
		defaultBranch: "main",
		headSHA:       testHeadSHA,
		detail: providers.CommitDetail{
			SHA:     testCommitSHA,
			Parents: []string{testParentSHA},
			Message: "bump payments",
			Files:   []providers.CommitFile{{Path: "apps/payments/deploy.yaml", Status: "modified"}},
		},
		laterFiles: []string{"apps/unrelated/cm.yaml"},
		blobs: map[string][]byte{
			testParentSHA + ":apps/payments/deploy.yaml": []byte("v1\n"),
		},
		createURL: "https://github.com/org/gitops-infra/pull/99",
		createNum: 99,
	}
	exec := New(forge, []string{"org/gitops-infra"}, nil)
	act := baseAction()
	if err := exec.Execute(context.Background(), act); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if forge.lastReq.BaseSHA != testHeadSHA {
		t.Fatalf("BaseSHA = %q, want current HEAD %q", forge.lastReq.BaseSHA, testHeadSHA)
	}
	if act.Revert.ResultURL != forge.createURL {
		t.Fatalf("ResultURL = %q, want %q", act.Revert.ResultURL, forge.createURL)
	}
}

func TestExecuteIdempotentExistingOpenPR(t *testing.T) {
	forge := &fakeRevertForge{
		defaultBranch: "main",
		headSHA:       testHeadSHA,
		existingFound: true,
		existingURL:   "https://github.com/org/gitops-infra/pull/77",
		existingNum:   77,
	}
	var reusedFlag bool
	exec := New(forge, []string{"org/gitops-infra"}, nil, WithOnOpened(func(_ context.Context, _ providers.Action, _ string, reused bool) {
		reusedFlag = reused
	}))
	act := baseAction()
	if err := exec.Execute(context.Background(), act); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if forge.createCalls != 0 {
		t.Fatalf("CreateRevertDraftPR must not be called when open PR exists, got %d", forge.createCalls)
	}
	if act.Revert.ResultURL != forge.existingURL {
		t.Fatalf("ResultURL = %q, want %q", act.Revert.ResultURL, forge.existingURL)
	}
	if !reusedFlag {
		t.Fatal("expected reused=true in onOpened callback")
	}
}

func TestExecuteForgeErrorPropagates(t *testing.T) {
	forge := &fakeRevertForge{
		defaultBranch: "main",
		headSHA:       testCommitSHA,
		detail: providers.CommitDetail{
			SHA:     testCommitSHA,
			Parents: []string{testParentSHA},
			Message: "bump payments",
			Files:   []providers.CommitFile{{Path: "apps/payments/deploy.yaml", Status: "modified"}},
		},
		blobs: map[string][]byte{
			testParentSHA + ":apps/payments/deploy.yaml": []byte("v1\n"),
		},
		createErr: errors.New("boom"),
	}
	exec := New(forge, []string{"org/gitops-infra"}, nil)
	if err := exec.Execute(context.Background(), baseAction()); err == nil {
		t.Fatal("expected error from CreateRevertDraftPR")
	}
}
