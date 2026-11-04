// SPDX-License-Identifier: Apache-2.0

package curate

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/forge/github"
	"github.com/Smana/runlore/internal/providers"
)

// Guard must satisfy every pass-facing forge surface, so one wrapper covers all passes.
var (
	_ Forge           = Guard{}
	_ RetireForge     = Guard{}
	_ RevalidateForge = Guard{}
	_ ClosedPRLister  = Guard{}
	_ ContestedForge  = Guard{}
	_ QueueIssueForge = Guard{}
	_ RefreshForge    = Guard{}
)

// fakeGuarded extends the shared fakeForge with the wider GuardedForge surface.
type fakeGuarded struct {
	fakeForge
	retired       []string
	revalidated   []string
	closeErr      error
	revalidateErr error
	edited        int
	resolvePRs    int
	resolveErr    error
	issuesCreated int
	issuesEdited  int
}

func (f *fakeGuarded) PREntry(context.Context, int) (providers.PREntry, error) {
	return providers.PREntry{Path: "incidents/a.md", State: "open"}, nil
}
func (f *fakeGuarded) EditEntryOnPR(context.Context, int, string, func([]byte) ([]byte, bool, error)) (bool, error) {
	f.edited++
	return true, nil
}
func (f *fakeGuarded) OpenResolvePR(context.Context, string, func([]byte) ([]byte, bool, error), string) (providers.Ref, error) {
	if f.resolveErr != nil {
		return providers.Ref{}, f.resolveErr
	}
	f.resolvePRs++
	return providers.Ref{URL: "https://forge/pr/3"}, nil
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
func (f *fakeGuarded) OpenRevalidatePR(_ context.Context, entryPath string, _ time.Time, _ time.Duration, _ string) (providers.Ref, error) {
	if f.revalidateErr != nil {
		return providers.Ref{}, f.revalidateErr
	}
	f.revalidated = append(f.revalidated, entryPath)
	return providers.Ref{URL: "https://forge/pr/2"}, nil
}
func (f *fakeGuarded) Close(ctx context.Context, n int) error {
	if f.closeErr != nil {
		return f.closeErr
	}
	return f.fakeForge.Close(ctx, n)
}

func (f *fakeGuarded) ListMergedPRsByLabel(context.Context, string, time.Time) ([]providers.CuratedIssue, error) {
	return nil, nil
}
func (f *fakeGuarded) CreateIssue(context.Context, string, string, []string) (int, providers.Ref, error) {
	f.issuesCreated++
	return 7, providers.Ref{URL: "https://forge/issues/7"}, nil
}
func (f *fakeGuarded) EditIssue(context.Context, int, string) error {
	f.issuesEdited++
	return nil
}
func (f *fakeGuarded) EntryPRState(context.Context, int) (providers.EntryPRState, error) {
	return providers.EntryPRState{}, nil
}
func (f *fakeGuarded) RebuildEntryPR(context.Context, int, string) error { return nil }

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
	validated := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	if _, err := g.OpenRevalidatePR(context.Background(), "entries/b.md", validated, time.Hour, "body"); err != nil {
		t.Fatal(err)
	}
	if len(inner.closed) != 0 || len(inner.retired) != 0 || len(inner.revalidated) != 0 {
		t.Fatalf("dry-run must not reach the forge: closed=%v retired=%v revalidated=%v",
			inner.closed, inner.retired, inner.revalidated)
	}
	if len(aud.recs) != 3 || aud.recs[0].Decision != audit.DecisionDryRun || aud.recs[0].Actor != "curate" {
		t.Fatalf("want 3 dry-run audit records with actor=curate, got %+v", aud.recs)
	}
	if aud.recs[0].Op != "kb.close" || aud.recs[0].Target != "pr/7" {
		t.Fatalf("record[0] = %+v, want op kb.close target pr/7", aud.recs[0])
	}
	// The revalidation record names the entry AND the date under proposal.
	if r := aud.recs[2]; r.Op != "kb.revalidate-pr" || r.Target != "entries/b.md" || r.Reason != "2026-08-03" {
		t.Fatalf("record[2] = %+v, want op kb.revalidate-pr target entries/b.md reason 2026-08-03", r)
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

// TestGuardAuditsDoneSkipsAsSkippedNotFailed pins the disposition of the entry-edit
// sentinels. They mean the forge looked and no edit was warranted — nothing was
// attempted, so nothing failed. It matters beyond tidiness: for revalidation the
// done-skip is the STEADY state (every healthy entry is a candidate every sweep,
// and nearly all are already fresh), so mislabelling it would fill an append-only
// chain with "failed" records describing a pass working exactly as designed.
func TestGuardAuditsDoneSkipsAsSkippedNotFailed(t *testing.T) {
	skips := []error{github.ErrRecentlyValidated, github.ErrEntryInactive, github.ErrAlreadyRetired}
	for _, sentinel := range skips {
		t.Run(sentinel.Error(), func(t *testing.T) {
			inner, aud := &fakeGuarded{revalidateErr: sentinel}, &recAudit{}
			g := Guard{Inner: inner, Audit: aud, Log: discardLog()}
			_, err := g.OpenRevalidatePR(context.Background(), "entries/a.md", time.Now(), time.Hour, "body")
			if !errors.Is(err, sentinel) {
				t.Fatalf("the sentinel must still reach the pass, got %v", err)
			}
			if len(aud.recs) != 1 || aud.recs[0].Decision != audit.DecisionSkipped {
				t.Fatalf("want one skipped record, got %+v", aud.recs)
			}
		})
	}
	// A genuine forge error is still a failure.
	inner, aud := &fakeGuarded{revalidateErr: errors.New("forge 502")}, &recAudit{}
	g := Guard{Inner: inner, Audit: aud, Log: discardLog()}
	if _, err := g.OpenRevalidatePR(context.Background(), "entries/a.md", time.Now(), time.Hour, "body"); err == nil {
		t.Fatal("want the forge error to propagate")
	}
	if len(aud.recs) != 1 || aud.recs[0].Decision != audit.DecisionFailed {
		t.Fatalf("a real forge error must stay a failure, got %+v", aud.recs)
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

var _ ResolveForge = Guard{}

func TestGuardResolveWrites(t *testing.T) {
	ctx := context.Background()
	edit := func(raw []byte) ([]byte, bool, error) { return raw, true, nil }

	t.Run("dry-run skips the inner forge and audits", func(t *testing.T) {
		inner, aud := &fakeGuarded{}, &recAudit{}
		g := Guard{Inner: inner, DryRun: true, Audit: aud, Log: discardLog()}
		if changed, err := g.EditEntryOnPR(ctx, 7, "m", edit); err != nil || !changed {
			t.Fatal("dry-run reports the push that would happen: ", changed, err)
		}
		if _, err := g.OpenResolvePR(ctx, "incidents/a.md", edit, "b"); err != nil {
			t.Fatal(err)
		}
		if inner.edited != 0 || inner.resolvePRs != 0 {
			t.Fatal("dry-run must not reach the forge")
		}
		if len(aud.recs) != 2 || aud.recs[0].Op != "kb.resolve" || aud.recs[1].Op != "kb.resolve" || aud.recs[0].Decision != audit.DecisionDryRun || aud.recs[1].Reason != "b" {
			t.Fatalf("%+v", aud.recs)
		}
	})

	t.Run("apply calls through, and PREntry passes through", func(t *testing.T) {
		inner, aud := &fakeGuarded{}, &recAudit{}
		g := Guard{Inner: inner, Audit: aud, Log: discardLog()}
		if changed, err := g.EditEntryOnPR(ctx, 7, "m", edit); err != nil || !changed || inner.edited != 1 {
			t.Fatal(changed, err)
		}
		if pe, err := g.PREntry(ctx, 7); err != nil || pe.State != "open" || len(aud.recs) != 1 {
			t.Fatal("a read is not audited: ", pe, err, aud.recs)
		}
	})

	t.Run("ErrAlreadyResolved is a skip", func(t *testing.T) {
		inner, aud := &fakeGuarded{resolveErr: providers.ErrAlreadyResolved}, &recAudit{}
		g := Guard{Inner: inner, Audit: aud, Log: discardLog()}
		if _, err := g.OpenResolvePR(ctx, "incidents/a.md", edit, "b"); !errors.Is(err, providers.ErrAlreadyResolved) {
			t.Fatal(err)
		}
		if len(aud.recs) != 1 || aud.recs[0].Decision != audit.DecisionSkipped {
			t.Fatalf("%+v", aud.recs)
		}
	})
}

func TestResolveLabelIsAnEntryEdit(t *testing.T) {
	if !isEntryEditProposal([]string{"runlore", "runlore-resolve"}) {
		t.Fatal("a resolve PR is never an entry (KP17)")
	}
}

func TestGuardAuditsTheQueueIssueWrites(t *testing.T) {
	for _, dry := range []bool{true, false} {
		inner, aud := &fakeGuarded{}, &recAudit{}
		g := Guard{Inner: inner, DryRun: dry, Audit: aud, Log: discardLog()}
		n, ref, err := g.CreateIssue(context.Background(), "Knowledge queue", "body", []string{"runlore-queue"})
		if err != nil {
			t.Fatal(err)
		}
		if err := g.EditIssue(context.Background(), 7, "body"); err != nil {
			t.Fatal(err)
		}
		want, wantWrites := audit.DecisionExecuted, 1
		if dry {
			want, wantWrites = audit.DecisionDryRun, 0
		}
		if inner.issuesCreated != wantWrites || inner.issuesEdited != wantWrites || (dry && (n != 0 || ref.URL != "")) || (!dry && n != 7) {
			t.Fatalf("dry=%v: created %d edited %d -> %d %+v", dry, inner.issuesCreated, inner.issuesEdited, n, ref)
		}
		if len(aud.recs) != 2 || aud.recs[0].Op != "kb.queue-issue" || aud.recs[1].Op != "kb.queue-issue" ||
			aud.recs[0].Decision != want || aud.recs[0].Target != "issue/Knowledge queue" || aud.recs[1].Target != "issue/7" {
			t.Fatalf("dry=%v: %+v", dry, aud.recs)
		}
	}
}
