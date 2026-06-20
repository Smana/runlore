package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// Slack delivers via a Slack incoming webhook.
type Slack struct {
	webhookURL string
	http       *http.Client
}

// NewSlack builds a Slack webhook notifier.
func NewSlack(webhookURL string) *Slack {
	return &Slack{webhookURL: webhookURL, http: &http.Client{Timeout: 15 * time.Second}}
}

var _ providers.Notifier = (*Slack)(nil)

// Deliver posts the formatted investigation to the webhook. When an action carries
// an ApprovalID, it renders interactive Approve/Reject buttons (Block Kit).
func (s *Slack) Deliver(ctx context.Context, inv providers.Investigation) error {
	body, err := json.Marshal(slackMessage(inv))
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.webhookURL, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return fmt.Errorf("slack post: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("slack status %d", resp.StatusCode)
	}
	return nil
}

// Slack interaction action_ids — must match the server's /slack/interactions handler.
const (
	approveActionID = "runlore_approve"
	rejectActionID  = "runlore_reject"
)

// slackMessage builds the webhook payload: always a text fallback, plus Block Kit
// Approve/Reject buttons for any action carrying an ApprovalID (rung-2).
func slackMessage(inv providers.Investigation) map[string]any {
	text := Format(inv)
	msg := map[string]any{"text": text}
	var actionBlocks []map[string]any
	for _, a := range inv.Actions {
		if a.ApprovalID == "" {
			continue
		}
		actionBlocks = append(actionBlocks,
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": "*Proposed action:* " + a.Description}},
			map[string]any{"type": "actions", "elements": []map[string]any{
				{"type": "button", "style": "primary", "action_id": approveActionID, "value": a.ApprovalID,
					"text": map[string]any{"type": "plain_text", "text": "Approve"}},
				{"type": "button", "style": "danger", "action_id": rejectActionID, "value": a.ApprovalID,
					"text": map[string]any{"type": "plain_text", "text": "Reject"}},
			}},
		)
	}
	if len(actionBlocks) > 0 {
		blocks := []map[string]any{{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": text}}}
		msg["blocks"] = append(blocks, actionBlocks...)
	}
	return msg
}

// Multi delivers to several notifiers, best-effort: a failing notifier is logged,
// not propagated, so one bad sink doesn't block the others.
type Multi struct {
	notifiers []providers.Notifier
	log       *slog.Logger
}

// NewMulti builds a fan-out notifier.
func NewMulti(log *slog.Logger, notifiers ...providers.Notifier) *Multi {
	return &Multi{notifiers: notifiers, log: log}
}

var _ providers.Notifier = (*Multi)(nil)

// Deliver fans out to every notifier (best-effort: one bad sink never blocks the
// others), logs each failure, and returns the joined errors so the caller can tell
// delivery was incomplete. Returns nil when all sinks succeed.
func (m *Multi) Deliver(ctx context.Context, inv providers.Investigation) error {
	var errs []error
	for _, n := range m.notifiers {
		if err := n.Deliver(ctx, inv); err != nil {
			m.log.Error("delivery failed", "err", err)
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// Len reports how many notifiers are configured.
func (m *Multi) Len() int { return len(m.notifiers) }
