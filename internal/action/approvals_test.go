// SPDX-License-Identifier: Apache-2.0

package action

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/providers"
)

type fakeExec struct {
	mu  sync.Mutex
	ran []providers.Action
	err error
}

func (f *fakeExec) Execute(_ context.Context, a providers.Action) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ran = append(f.ran, a)
	return f.err
}

func (f *fakeExec) count() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.ran)
}

// newApprovals builds an approval queue whose policy allows the "apps" namespace.
func newApprovals(exec Executor) *Approvals {
	pol := New(config.ActionPolicy{Mode: config.ActionApprove, Allow: config.ActionAllow{ReversibleOnly: true, Namespaces: []string{"apps"}}})
	return NewApprovals(exec, pol, audit.Nop{}, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func allowedAction() providers.Action {
	return providers.Action{Op: "suspend", Target: providers.Workload{Kind: "Kustomization", Name: "web", Namespace: "apps"}}
}

func TestApproveExecutes(t *testing.T) {
	exec := &fakeExec{}
	a := newApprovals(exec)
	id := a.Register(allowedAction())
	if len(a.List()) != 1 {
		t.Fatalf("want 1 pending, got %d", len(a.List()))
	}
	if _, err := a.Approve(context.Background(), id, "test"); err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if exec.count() != 1 || exec.ran[0].Op != "suspend" {
		t.Fatalf("executor not run: %+v", exec.ran)
	}
	if len(a.List()) != 0 {
		t.Fatal("approved action should be removed from pending")
	}
	if _, err := a.Approve(context.Background(), id, "test"); err == nil {
		t.Fatal("re-approving a consumed id should error")
	}
}

// TestApproveConcurrentExecutesOnce is the regression test for the H1 TOCTOU:
// two concurrent approvals of the same id must execute the action exactly once.
// Run with -race to also catch a double Execute racing on the executor.
func TestApproveConcurrentExecutesOnce(t *testing.T) {
	exec := &fakeExec{}
	a := newApprovals(exec)
	id := a.Register(allowedAction())

	var wg sync.WaitGroup
	var ok int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := a.Approve(context.Background(), id, "test"); err == nil {
				atomic.AddInt32(&ok, 1)
			}
		}()
	}
	wg.Wait()
	if exec.count() != 1 {
		t.Fatalf("action executed %d times; want exactly 1", exec.count())
	}
	if ok != 1 {
		t.Fatalf("%d approvals succeeded; want exactly 1", ok)
	}
}

func TestApproveRejectsProtectedNamespace(t *testing.T) {
	exec := &fakeExec{}
	a := newApprovals(exec)
	// flux-system is a built-in protected namespace — never an action target.
	id := a.Register(providers.Action{Op: "suspend", Target: providers.Workload{Kind: "Kustomization", Name: "x", Namespace: "flux-system"}})
	if _, err := a.Approve(context.Background(), id, "test"); err == nil {
		t.Fatal("expected refusal: flux-system is protected")
	}
	if exec.count() != 0 {
		t.Fatal("executor must not run for a protected-namespace target")
	}
}

func TestApproveRejectsUnknownOp(t *testing.T) {
	exec := &fakeExec{}
	a := newApprovals(exec)
	id := a.Register(providers.Action{Op: "delete", Target: providers.Workload{Kind: "Kustomization", Name: "x", Namespace: "apps"}})
	if _, err := a.Approve(context.Background(), id, "test"); err == nil {
		t.Fatal("expected refusal: unknown op")
	}
	if exec.count() != 0 {
		t.Fatal("executor must not run for an unknown op")
	}
}

func TestReject(t *testing.T) {
	a := newApprovals(&fakeExec{})
	id := a.Register(allowedAction())
	if err := a.Reject(id); err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if _, err := a.Approve(context.Background(), id, "test"); err == nil {
		t.Fatal("approving a rejected id should error")
	}
}

func TestExpiry(t *testing.T) {
	a := newApprovals(&fakeExec{})
	id := a.Register(allowedAction())
	a.now = func() time.Time { return time.Now().Add(time.Hour) } // past the 30m TTL
	if _, err := a.Approve(context.Background(), id, "test"); err == nil {
		t.Fatal("expired action should not be approvable")
	}
}
