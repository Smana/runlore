// Package webhook is a generic outgoing-webhook notifier: it POSTs each
// investigation's findings as JSON to an operator-configured URL. It exists as
// much to prove RunLore's notifier extensibility (drop one self-registering
// file) as to be useful in production.
//
// To enable, add one block under notify: in your values.yaml:
//
//	webhook:
//	  url_env: RUNLORE_WEBHOOK_NOTIFY_URL   # POST findings JSON to this URL
//
// Adding a notifier is "drop a self-registering file under internal/notify/<name>/
// + a blank import in main" — zero edits to config.Config.
package webhook

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/Smana/runlore/internal/httpx"
	"github.com/Smana/runlore/internal/notify"
	"github.com/Smana/runlore/internal/providers"
)

// Notifier POSTs investigation findings as JSON to a configured URL.
type Notifier struct {
	url    string
	client *http.Client
}

// New builds a webhook Notifier for the given URL.
func New(url string) *Notifier {
	return &Notifier{url: url, client: httpx.SecureClient(10 * time.Second)}
}

var _ providers.Notifier = (*Notifier)(nil)

// payload is the JSON body sent to the webhook endpoint.
type payload struct {
	Title      string  `json:"title"`
	Confidence float64 `json:"confidence"`
	Namespace  string  `json:"namespace,omitempty"`
	Resource   string  `json:"resource,omitempty"`
	CuratedURL string  `json:"curated_url,omitempty"`
	Text       string  `json:"text"`
}

// Deliver marshals the investigation to JSON and POSTs it to the configured URL.
func (n *Notifier) Deliver(ctx context.Context, inv providers.Investigation) error {
	body, err := json.Marshal(payload{
		Title:      inv.Title,
		Confidence: inv.Confidence,
		Namespace:  inv.Resource.Namespace,
		Resource:   inv.Resource.Name,
		CuratedURL: inv.CuratedURL,
		Text:       notify.Format(inv),
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, n.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := n.client.Do(req)
	if err != nil {
		return err
	}
	defer func() { _, _ = io.Copy(io.Discard, resp.Body); _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return &deliverError{status: resp.StatusCode}
	}
	return nil
}

type deliverError struct{ status int }

func (e *deliverError) Error() string {
	return fmt.Sprintf("webhook notify: HTTP %d %s", e.status, http.StatusText(e.status))
}

// cfg is the YAML schema for the notify.webhook block.
type cfg struct {
	URLEnv string `yaml:"url_env"`
}

func init() {
	notify.Register(notify.Descriptor{
		Name: "webhook",
		Build: func(d notify.Deps) (providers.Notifier, error) {
			node, ok := d.Cfg.Notify.Extra["webhook"]
			if !ok {
				return nil, nil // not configured
			}
			var c cfg
			if err := node.Decode(&c); err != nil {
				return nil, err
			}
			if c.URLEnv == "" {
				return nil, nil
			}
			url := os.Getenv(c.URLEnv)
			if url == "" {
				return nil, nil
			}
			return New(url), nil
		},
	})
}
