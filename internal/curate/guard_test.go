// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"errors"
	"testing"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/providers"
)

// Guard must satisfy every pass-facing forge surface, so one wrapper covers all passes.
var (
	_ Forge          = Guard{}
	_ RetireForge    = Guard{}
	_ ClosedPRLister = Guard{}
	_ ContestedForge = Guard{}
)

// fakeGuarded extends the shared fakeForge with the wider GuardedForge surface.
type fakeGuarded struct {
	fakeForge
	retired  []string
	closeErr error
}

func (f *fakeGuarded) ListClosedUnmergedPRsByLabel(context.Context, string) ([]providers.CuratedIssue, error) {
	return nil, nil
}
func (f *fakeGuarded) ListIssueCommentBodies(context.Context, int) ([]string, error) { return nil, nil }
func (f *fakeGuarded) IsPROpen(context.Context, int) (bool, error)                   { return true, nil }
func (f *fakeGuarded) OpenRetirePR(_ context.Context, entryPath, _ string) (providers.Ref, error) {
	f.retired = append(f.retired, entryPath)
	return providers.Ref{URL: "https://forge/pr/1"}, nil
}
func (f *fakeGuarded) Close(ctx context.Context, n int) error {
	if f.closeErr != nil {
		return f.closeErr
	}
	return f.fakeForge.Close(ctx, n)
}

// recAudit records audit entries in memory.
type recAudit struct{ recs []audit.Record }

func (r *recAudit) Log(rec audit.Record) error { r.recs = append(r.recs, rec); return nil }

func TestGuardDryRunSkipsWritesButAudits(t *testing.T) {
	inner, aud := &fakeGuarded{}, &recAudit{}
	g := Guard{Inner: inner, DryRun: true, Audit: aud, Log: discardLog()}
	if err := g.Close(context.Background(), 7); err != nil {
		t.Fatal(err)
	}
	if _, err := g.OpenRetirePR(context.Background(), "entries/a.md", "body"); err != nil {
		t.Fatal(err)
	}
	if len(inner.closed) != 0 || len(inner.retired) != 0 {
		t.Fatalf("dry-run must not reach the forge: closed=%v retired=%v", inner.closed, inner.retired)
	}
	if len(aud.recs) != 2 || aud.recs[0].Decision != audit.DecisionDryRun || aud.recs[0].Actor != "curate" {
		t.Fatalf("want 2 dry-run audit records with actor=curate, got %+v", aud.recs)
	}
	if aud.recs[0].Op != "kb.close" || aud.recs[0].Target != "pr/7" {
		t.Fatalf("record[0] = %+v, want op kb.close target pr/7", aud.recs[0])
	}
}

func TestGuardApplyExecutesAndAudits(t *testing.T) {
	inner, aud := &fakeGuarded{}, &recAudit{}
	g := Guard{Inner: inner, DryRun: false, Audit: aud, Log: discardLog()}
	if err := g.Comment(context.Background(), 9, "back-ref\ndetails"); err != nil {
		t.Fatal(err)
	}
	if len(inner.commented) != 1 || inner.commented[0] != 9 {
		t.Fatalf("apply must reach the forge, got %v", inner.commented)
	}
	if len(aud.recs) != 1 || aud.recs[0].Decision != audit.DecisionExecuted || aud.recs[0].Reason != "back-ref" {
		t.Fatalf("want 1 executed record with first-line reason, got %+v", aud.recs)
	}
}

func TestGuardFailureIsAuditedAndPropagated(t *testing.T) {
	boom := errors.New("forge 502")
	inner, aud := &fakeGuarded{closeErr: boom}, &recAudit{}
	g := Guard{Inner: inner, DryRun: false, Audit: aud, Log: discardLog()}
	if err := g.Close(context.Background(), 3); !errors.Is(err, boom) {
		t.Fatalf("error must propagate, got %v", err)
	}
	if len(aud.recs) != 1 || aud.recs[0].Decision != audit.DecisionFailed {
		t.Fatalf("want a failed audit record, got %+v", aud.recs)
	}
}

func TestGuardReadsPassThrough(t *testing.T) {
	inner := &fakeGuarded{fakeForge: fakeForge{prs: []providers.CuratedIssue{{Number: 1}}}}
	g := Guard{Inner: inner, DryRun: true, Log: discardLog()} // nil Audit must be safe
	prs, err := g.ListPRsByLabel(context.Background(), "runlore")
	if err != nil || len(prs) != 1 {
		t.Fatalf("reads must pass through even in dry-run: %v %v", prs, err)
	}
}
