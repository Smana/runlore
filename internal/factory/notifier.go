// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/redact"
)

// Notifier files each finished investigation as a factory task and records the answer in the
// ledger. It is a providers.Notifier, called by notify.Multi after each investigation.
type Notifier struct {
	Client     *Client
	Ledger     *Ledger
	Repository string
	MaxCostUSD float64
	Log        *slog.Logger
}

var _ providers.Notifier = (*Notifier)(nil)

// Deliver posts the finding to the factory under the TriggerKey. An investigation without a
// TriggerKey, or a disabled client, is skipped. A 202 or a 429 is logged and not an error; a 503
// that outlasts the client's retries is.
func (n *Notifier) Deliver(ctx context.Context, inv providers.Investigation) error {
	if inv.TriggerKey == "" || !n.Client.Enabled() {
		return nil
	}
	p := notify.NewPayload(inv)
	req := TaskRequest{
		Repository: n.Repository,
		Text:       redact.Secrets(p.Text),
		DataClass:  "internal",
		MaxCostUSD: n.MaxCostUSD,
		Incident: Incident{
			Alert: p.AlertName, Resource: p.ResourceRef, Severity: p.Severity,
			Cluster: p.Cluster, Verdict: p.Verdict, Confidence: p.Confidence,
		},
	}
	got, err := n.Client.CreateTask(ctx, IdempotencyKey(inv.TriggerKey), req)
	if err != nil {
		return fmt.Errorf("factory: filing the task: %w", err)
	}
	switch got.Status {
	case http.StatusCreated, http.StatusOK:
		if n.Ledger == nil {
			return nil
		}
		workload := ""
		if inv.Resource.Kind != "" || inv.Resource.Name != "" {
			workload = inv.Resource.Kind + "/" + inv.Resource.Name
		}
		return n.Ledger.Upsert(inv.TriggerKey, func(r *Record) {
			r.Alert, r.Namespace, r.Workload = inv.AlertName, inv.Resource.Namespace, workload
			r.Task, r.Room, r.Issue = got.Task, got.Room, got.Issue
		})
	default:
		n.log().Info("factory did not take the finding", "status", got.Status, "reason", got.Reason, "alert", inv.AlertName)
		return nil
	}
}

func (n *Notifier) log() *slog.Logger {
	if n.Log != nil {
		return n.Log
	}
	return slog.Default()
}
