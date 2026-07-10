// SPDX-License-Identifier: Apache-2.0

package action

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/providers"
)

// ErrNoPending is returned when an approval id is unknown, already consumed, or expired.
var ErrNoPending = errors.New("no pending action")

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
	audit  audit.Auditor
	ttl    time.Duration
	log    *slog.Logger
	now    func() time.Time

	mu      sync.Mutex
	pending map[string]entry
}

type entry struct {
	action  providers.Action
	created time.Time
}

// NewApprovals builds an approval queue. A nil auditor falls back to a no-op.
func NewApprovals(exec Executor, policy *Policy, aud audit.Auditor, log *slog.Logger) *Approvals {
	if aud == nil {
		aud = audit.Nop{}
	}
	return &Approvals{exec: exec, policy: policy, audit: aud, ttl: 30 * time.Minute, log: log, now: time.Now, pending: map[string]entry{}}
}

// Register queues an action for approval and returns its id.
func (a *Approvals) Register(act providers.Action) string {
	a.mu.Lock()
	defer a.mu.Unlock()
	id := newID()
	a.pending[id] = entry{action: act, created: a.now()}
	return id
}

// newID returns an unguessable approval id. Ids appear in Slack button values and
// on the HTTP approve/reject path; a sequential "a%d" id is trivially enumerable,
// so a crypto/rand id is defense-in-depth behind the auth gates. crypto/rand.Read
// does not return an error on supported platforms (Go 1.24+).
func newID() string {
	var b [12]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
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

// Approve executes the pending action after re-checking the envelope. The entry
// is CLAIMED (removed) under the same lock that reads it, so two concurrent
// approvals of the same id cannot both execute (TOCTOU double-execute). actor
// identifies the approver for the audit trail. Errors if the id is
// unknown/expired, the action is now out of policy, or execution fails.
func (a *Approvals) Approve(ctx context.Context, id, actor string) (providers.Action, error) {
	a.mu.Lock()
	e, ok := a.pending[id]
	if ok && a.now().Sub(e.created) > a.ttl {
		ok = false
	}
	if ok {
		delete(a.pending, id) // claim under the lock before releasing
	}
	a.mu.Unlock()
	if !ok {
		return providers.Action{}, fmt.Errorf("%w %q", ErrNoPending, id)
	}
	// Defense in depth: re-evaluate the server-authoritative envelope at exec time.
	act := deriveSafety(e.action)
	if reason := a.policy.violation(act); reason != "" {
		recordAttempt(a.audit, actor, act, audit.DecisionDenied, reason)
		return providers.Action{}, fmt.Errorf("action no longer within policy: %s", reason)
	}
	a.log.Info("executing approved action", "id", id, "actor", actor, "op", act.Op, "target", target(act))
	// executed/failed are audited at the executor seam (NewAuditedExecutor); the
	// actor is carried in the context so the record attributes the right approver.
	if err := a.exec.Execute(ContextWithActor(ctx, actor), act); err != nil {
		a.log.Error("approved action failed", "id", id, "err", err)
		return providers.Action{}, err
	}
	a.log.Info("approved action executed", "id", id)
	return act, nil
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

func (a *Approvals) gc() {
	for id, e := range a.pending {
		if a.now().Sub(e.created) > a.ttl {
			delete(a.pending, id)
		}
	}
}
