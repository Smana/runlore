// SPDX-License-Identifier: Apache-2.0

package factory

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"time"

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

// deliverTimeout bounds one Deliver, retries included: notify.Multi delivers sequentially on the
// investigation path with no deadline of its own.
const deliverTimeout = 20 * time.Second

// Deliver posts the finding to the factory under the TriggerKey, as File does, and logs a refusal.
// A 202 or a 429 is logged and not an error; a 503 that outlasts the client's retries is. The Slack
// root belongs to this delivery and is kept across a new generation.
func (n *Notifier) Deliver(ctx context.Context, inv providers.Investigation) error {
	got, err := n.file(ctx, inv, false)
	if err != nil {
		return err
	}
	n.logRefusal(ctx, got, inv)
	return nil
}

// File posts the finding to the factory under the TriggerKey and records a 201 or a 200 in the
// ledger, returning the answer for the caller to act on. An investigation without a TriggerKey, or a
// disabled client, posts nothing and answers a zero Created. Filing comes before delivery here, so
// the Slack root on the record is the old generation's and a new generation clears it.
func (n *Notifier) File(ctx context.Context, inv providers.Investigation) (Created, error) {
	return n.file(ctx, inv, true)
}

func (n *Notifier) file(ctx context.Context, inv providers.Investigation, clearSlack bool) (Created, error) {
	if inv.TriggerKey == "" || !n.Client.Enabled() {
		return Created{}, nil
	}
	ctx, cancel := context.WithTimeout(ctx, deliverTimeout)
	defer cancel()
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
		return got, fmt.Errorf("factory: filing the task: %w", err)
	}
	if (got.Status != http.StatusCreated && got.Status != http.StatusOK) || n.Ledger == nil {
		return got, nil
	}
	workload := ""
	if inv.Resource.Kind != "" || inv.Resource.Name != "" {
		workload = inv.Resource.Kind + "/" + inv.Resource.Name
	}
	return got, n.Ledger.Upsert(inv.TriggerKey, func(r *Record) {
		if r.Room != "" && r.Room != got.Room {
			r.startGeneration(clearSlack)
		}
		r.Alert, r.Namespace, r.Workload = inv.AlertName, inv.Resource.Namespace, workload
		r.Task, r.Room, r.Issue = got.Task, got.Room, got.Issue
	})
}

// logRefusal logs an answer that is neither a take nor the zero Created of nothing filed. 202 (below
// the factory's threshold) and 429 (a limit) are the factory deciding; any other status is a refusal
// the operator should see.
func (n *Notifier) logRefusal(ctx context.Context, got Created, inv providers.Investigation) {
	lvl := slog.LevelWarn
	switch got.Status {
	case 0, http.StatusCreated, http.StatusOK:
		return
	case http.StatusAccepted, http.StatusTooManyRequests:
		lvl = slog.LevelInfo
	}
	n.log().Log(ctx, lvl, "factory did not take the finding", "status", got.Status, "reason", got.Reason, "alert", inv.AlertName)
}

func (n *Notifier) log() *slog.Logger {
	if n.Log != nil {
		return n.Log
	}
	return slog.Default()
}
