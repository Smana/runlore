package action

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

type fakeExec struct {
	ran []providers.Action
	err error
}

func (f *fakeExec) Execute(_ context.Context, a providers.Action) error {
	f.ran = append(f.ran, a)
	return f.err
}

func newApprovals(exec Executor) *Approvals {
	pol := New(config.ActionPolicy{Mode: config.ActionApprove, Allow: config.ActionAllow{ReversibleOnly: true}})
	return NewApprovals(exec, pol, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func TestApproveExecutes(t *testing.T) {
	exec := &fakeExec{}
	a := newApprovals(exec)
	id := a.Register(providers.Action{Op: "suspend", Reversible: true, Target: providers.Workload{Kind: "Kustomization", Name: "apps", Namespace: "flux-system"}})
	if len(a.List()) != 1 {
		t.Fatalf("want 1 pending, got %d", len(a.List()))
	}
	if _, err := a.Approve(context.Background(), id); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if len(exec.ran) != 1 || exec.ran[0].Op != "suspend" {
		t.Fatalf("executor not run: %+v", exec.ran)
	}
	if len(a.List()) != 0 {
		t.Fatal("approved action should be removed from pending")
	}
	if _, err := a.Approve(context.Background(), id); err == nil {
		t.Fatal("re-approving a consumed id should error")
	}
}

func TestApproveReChecksEnvelope(t *testing.T) {
	exec := &fakeExec{}
	a := newApprovals(exec)
	// An irreversible action slips into the queue; Approve must re-check + refuse.
	id := a.Register(providers.Action{Op: "suspend", Reversible: false})
	if _, err := a.Approve(context.Background(), id); err == nil {
		t.Fatal("expected refusal: irreversible under reversible_only")
	}
	if len(exec.ran) != 0 {
		t.Fatal("executor must not run for an out-of-policy action")
	}
}

func TestReject(t *testing.T) {
	a := newApprovals(&fakeExec{})
	id := a.Register(providers.Action{Op: "suspend", Reversible: true})
	if err := a.Reject(id); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if _, err := a.Approve(context.Background(), id); err == nil {
		t.Fatal("approving a rejected id should error")
	}
}

func TestExpiry(t *testing.T) {
	a := newApprovals(&fakeExec{})
	id := a.Register(providers.Action{Op: "suspend", Reversible: true})
	a.now = func() time.Time { return time.Now().Add(time.Hour) } // past the 30m TTL
	if _, err := a.Approve(context.Background(), id); err == nil {
		t.Fatal("expired action should not be approvable")
	}
}
