// SPDX-License-Identifier: Apache-2.0

package argocd

import (
	"context"
	"testing"

	act "github.com/Smana/runlore/internal/action"
	"github.com/Smana/runlore/internal/audit"
)

// recorder captures audit records in memory.
type recorder struct{ records []audit.Record }

func (r *recorder) Log(rec audit.Record) error { r.records = append(r.records, rec); return nil }

// TestAuditedExecutionRecordsApplicationTarget proves the audit seam
// (action.NewAuditedExecutor) is engine-agnostic in practice: an executed
// Argo CD pause is recorded with the approver actor and the Application
// target, byte-identical in shape to a Flux record.
func TestAuditedExecutionRecordsApplicationTarget(t *testing.T) {
	c := newClient(app(map[string]any{"prune": true}, nil))
	rec := &recorder{}
	exec := act.NewAuditedExecutor(New(c), rec)
	ctx := act.ContextWithActor(context.Background(), "approve:slack:U_TEST")
	if err := exec.Execute(ctx, action("suspend")); err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if len(rec.records) != 1 {
		t.Fatalf("audit records = %d, want 1", len(rec.records))
	}
	r := rec.records[0]
	if r.Actor != "approve:slack:U_TEST" || r.Op != "suspend" ||
		r.Target != "Application/argocd/web" || r.Decision != audit.DecisionExecuted {
		t.Fatalf("record = %+v, want executed suspend on Application/argocd/web by the approver", r)
	}
}
