package notify

import (
	"bytes"
	"context"
	"encoding/json"
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

// Deliver posts the formatted investigation to the webhook.
func (s *Slack) Deliver(ctx context.Context, inv providers.Investigation) error {
	body, err := json.Marshal(map[string]string{"text": Format(inv)})
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

// Deliver fans out to every notifier; errors are logged, never returned.
func (m *Multi) Deliver(ctx context.Context, inv providers.Investigation) error {
	for _, n := range m.notifiers {
		if err := n.Deliver(ctx, inv); err != nil {
			m.log.Error("delivery failed", "err", err)
		}
	}
	return nil
}

// Len reports how many notifiers are configured.
func (m *Multi) Len() int { return len(m.notifiers) }
