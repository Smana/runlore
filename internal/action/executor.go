package action

import (
	"context"

	"github.com/Smana/runlore/internal/audit"
	"github.com/Smana/runlore/internal/providers"
)

// actorKey types the context value carrying the actor responsible for an action.
type actorKey struct{}

// ContextWithActor tags ctx with the actor accountable for an execution (e.g.
// "auto" or "approve:slack:U123"), so the audited executor can attribute its record.
func ContextWithActor(ctx context.Context, actor string) context.Context {
	return context.WithValue(ctx, actorKey{}, actor)
}

func actorFromContext(ctx context.Context) string {
	if a, ok := ctx.Value(actorKey{}).(string); ok && a != "" {
		return a
	}
	return "unknown"
}

// auditedExecutor wraps an Executor so every execution attempt is audited at the
// one seam both rungs converge on — the executed/failed records are guaranteed by
// construction, regardless of caller. Gate decisions (skipped/denied/dry-run)
// never reach Execute and stay with the caller.
type auditedExecutor struct {
	inner Executor
	aud   audit.Auditor
}

// NewAuditedExecutor wraps inner so executed/failed attempts are recorded to aud,
// attributed to the actor carried in the context (see ContextWithActor).
func NewAuditedExecutor(inner Executor, aud audit.Auditor) Executor {
	if aud == nil {
		aud = audit.Nop{}
	}
	return &auditedExecutor{inner: inner, aud: aud}
}

func (e *auditedExecutor) Execute(ctx context.Context, a providers.Action) error {
	actor := actorFromContext(ctx)
	if err := e.inner.Execute(ctx, a); err != nil {
		recordAttempt(e.aud, actor, a, audit.DecisionFailed, err.Error())
		return err
	}
	recordAttempt(e.aud, actor, a, audit.DecisionExecuted, "")
	return nil
}
