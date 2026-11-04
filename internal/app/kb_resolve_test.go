// SPDX-License-Identifier: Apache-2.0

package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Smana/runlore/internal/kbresolve"
	"github.com/Smana/runlore/internal/providers"
)

const resolveEntry = "---\ntype: Incident\ntitle: Alert too sensitive\n---\n\n## Cause\n\nc\n"

// resolveStub is one open PR's entry, in memory.
type resolveStub struct{ raw string }

func (s *resolveStub) PREntry(context.Context, int) (providers.PREntry, error) {
	return providers.PREntry{Path: "incidents/a.md", State: "open"}, nil
}
func (s *resolveStub) EditEntryOnPR(_ context.Context, _ int, _ string, edit func([]byte) ([]byte, bool, error)) (bool, error) {
	out, changed, err := edit([]byte(s.raw))
	if changed {
		s.raw = string(out)
	}
	return changed, err
}
func (s *resolveStub) ListPRsByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return nil, nil
}
func (s *resolveStub) OpenResolvePR(context.Context, string, func([]byte) ([]byte, bool, error), string) (providers.Ref, error) {
	return providers.Ref{}, nil
}

func swapResolveForge(t *testing.T, f kbresolve.Forge) {
	t.Helper()
	old := newResolveForge
	newResolveForge = func(string) (kbresolve.Forge, error) { return f, nil }
	t.Cleanup(func() { newResolveForge = old })
}

func TestKBResolve(t *testing.T) {
	stub := &resolveStub{raw: resolveEntry}
	swapResolveForge(t, stub)
	evidence := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(evidence, []byte("the alert cleared\nafter 30m token=ghp_abcdefghijklmnopqrstuvwxyz0123456789"), 0o600); err != nil {
		t.Fatal(err)
	}
	args := []string{"--pr", "157", "--fix", "https://github.com/Smana/cloud-native-ref/pull/2638", "--merge-sha", "3f5a133c", "--verdict", "resolved", "--evidence", evidence}
	var out strings.Builder
	if err := runKBResolve(args, &out); err != nil {
		t.Fatal(err)
	}
	if out.String() != "PR #157: resolution appended\n" {
		t.Fatalf("%q", out.String())
	}
	if !strings.Contains(stub.raw, "the alert cleared after 30m") || strings.Contains(stub.raw, "ghp_") {
		t.Fatal("the evidence is written, redacted: ", stub.raw)
	}
	out.Reset()
	if err := runKBResolve(args, &out); err != nil || out.String() != "PR #157: resolution already recorded\n" {
		t.Fatalf("%q %v", out.String(), err)
	}
}

func TestKBResolveUsage(t *testing.T) {
	swapResolveForge(t, &resolveStub{raw: resolveEntry})
	big := filepath.Join(t.TempDir(), "big.txt")
	if err := os.WriteFile(big, []byte(strings.Repeat("x", 16*1024+1)), 0o600); err != nil {
		t.Fatal(err)
	}
	const fix = "https://github.com/Smana/cloud-native-ref/pull/2638"
	for name, args := range map[string][]string{
		"no pr":            {"--fix", fix, "--verdict", "resolved"},
		"verdict fixed":    {"--pr", "1", "--fix", fix, "--verdict", "fixed"},
		"no fix":           {"--pr", "1", "--verdict", "resolved"},
		"evidence too big": {"--pr", "1", "--fix", fix, "--verdict", "resolved", "--evidence", big},
		"unknown flag":     {"--pr", "1", "--nope"},
	} {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			if err := runKBResolve(args, &out); !errors.Is(err, ErrUsage) {
				t.Fatalf("want ErrUsage, got %v", err)
			}
		})
	}
}

func TestKBResolveReportsClosedUnmerged(t *testing.T) {
	swapResolveForge(t, &closedStub{})
	var out strings.Builder
	err := runKBResolve([]string{"--pr", "1", "--fix", "https://github.com/o/r/pull/2", "--verdict", "persisting"}, &out)
	if !errors.Is(err, kbresolve.ErrClosedUnmerged) || errors.Is(err, ErrUsage) {
		t.Fatal(err)
	}
}

type closedStub struct{ resolveStub }

func (*closedStub) PREntry(context.Context, int) (providers.PREntry, error) {
	return providers.PREntry{State: "closed"}, nil
}

func TestKBResolvePersistingKeepsEvidence(t *testing.T) {
	stub := &resolveStub{raw: resolveEntry}
	swapResolveForge(t, stub)
	evidence := filepath.Join(t.TempDir(), "f.txt")
	if err := os.WriteFile(evidence, []byte("the alert fired again after 20m"), 0o600); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	err := runKBResolve([]string{"--pr", "157", "--fix", "https://github.com/Smana/cloud-native-ref/pull/2638", "--verdict", "persisting", "--evidence", evidence}, &out)
	if err != nil || !strings.Contains(stub.raw, "did not hold: the alert fired again after 20m.") {
		t.Fatal(err, stub.raw)
	}
}
