package action

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// Executor runs an approved action against the cluster.
type Executor interface {
	Execute(ctx context.Context, a providers.Action) error
}

// Pending is an action awaiting human approval.
type Pending struct {
	ID     string
	Action providers.Action
}

// Approvals queues actions proposed under "approve" mode until a human approves
// (then it executes via the Executor) or rejects them. In-memory, held on the
// leader; entries expire after a TTL.
type Approvals struct {
	exec   Executor
	policy *Policy
	ttl    time.Duration
	log    *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	seq     int
	pending map[string]entry
}

type entry struct {
	action  providers.Action
	created time.Time
}

// NewApprovals builds an approval queue.
func NewApprovals(exec Executor, policy *Policy, log *slog.Logger) *Approvals {
	return &Approvals{exec: exec, policy: policy, ttl: 30 * time.Minute, log: log, now: time.Now, pending: map[string]entry{}}
}

// Register queues an action for approval and returns its id.
func (a *Approvals) Register(act providers.Action) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.seq++
	id := fmt.Sprintf("a%d", a.seq)
	a.pending[id] = entry{action: act, created: a.now()}
	return id
}

// List returns the non-expired pending actions.
func (a *Approvals) List() []Pending {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.gc()
	out := make([]Pending, 0, len(a.pending))
	for id, e := range a.pending {
		out = append(out, Pending{ID: id, Action: e.action})
	}
	return out
}

// Approve executes the pending action after re-checking the envelope, then removes
// it. Errors if the id is unknown/expired, the action is now out of policy, or
// execution fails.
func (a *Approvals) Approve(ctx context.Context, id string) (providers.Action, error) {
	a.mu.Lock()
	e, ok := a.pending[id]
	if ok && a.now().Sub(e.created) > a.ttl {
		delete(a.pending, id)
		ok = false
	}
	a.mu.Unlock()
	if !ok {
		return providers.Action{}, fmt.Errorf("no pending action %q", id)
	}
	// Defense in depth: re-evaluate the envelope at execution time.
	if reason := a.policy.violation(e.action); reason != "" {
		a.remove(id)
		return providers.Action{}, fmt.Errorf("action no longer within policy: %s", reason)
	}
	a.log.Info("executing approved action", "id", id, "op", e.action.Op,
		"target", e.action.Target.Kind+"/"+e.action.Target.Namespace+"/"+e.action.Target.Name)
	if err := a.exec.Execute(ctx, e.action); err != nil {
		a.log.Error("approved action failed", "id", id, "err", err)
		return providers.Action{}, err
	}
	a.remove(id)
	a.log.Info("approved action executed", "id", id)
	return e.action, nil
}

// Reject drops a pending action.
func (a *Approvals) Reject(id string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, ok := a.pending[id]; !ok {
		return fmt.Errorf("no pending action %q", id)
	}
	delete(a.pending, id)
	return nil
}

func (a *Approvals) remove(id string) {
	a.mu.Lock()
	delete(a.pending, id)
	a.mu.Unlock()
}

func (a *Approvals) gc() {
	for id, e := range a.pending {
		if a.now().Sub(e.created) > a.ttl {
			delete(a.pending, id)
		}
	}
}
