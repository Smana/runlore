// SPDX-License-Identifier: Apache-2.0

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
	Title            string          `json:"title"`
	Confidence       float64         `json:"confidence"`
	Namespace        string          `json:"namespace,omitempty"`
	Resource         string          `json:"resource,omitempty"`
	CuratedURL       string          `json:"curated_url,omitempty"`
	Text             string          `json:"text"`
	Verdict          string          `json:"verdict,omitempty"`
	Severity         string          `json:"severity,omitempty"`
	Cluster          string          `json:"cluster,omitempty"`
	Environment      string          `json:"environment,omitempty"`
	Tenant           string          `json:"tenant,omitempty"`
	AlertName        string          `json:"alert_name,omitempty"`
	StartedAt        string          `json:"started_at,omitempty"` // RFC3339; "" when unknown
	Occurrences      int             `json:"occurrences,omitempty"`
	PrevCuratedURL   string          `json:"prev_curated_url,omitempty"`
	RuledOut         []string        `json:"ruled_out,omitempty"`
	DataGaps         []string        `json:"data_gaps,omitempty"`
	Prior            *priorPayload   `json:"prior,omitempty"`
	MatchedKnowledge *matchedPayload `json:"matched_knowledge,omitempty"`
}

// matchedPayload mirrors providers.MatchedEntry for webhook consumers: the
// pre-existing KB entry this investigation's kb_search matched at clear-match
// strength (distinct from prior, which reports recurrence).
type matchedPayload struct {
	Path  string  `json:"path,omitempty"`
	Title string  `json:"title,omitempty"`
	URL   string  `json:"url,omitempty"`
	Score float64 `json:"score,omitempty"`
}

// priorPayload mirrors providers.PriorKnowledge for webhook consumers: what the
// merged KB entry said last time this incident fired.
type priorPayload struct {
	Cause      string `json:"cause,omitempty"`
	Resolution string `json:"resolution,omitempty"`
	EntryPath  string `json:"entry_path,omitempty"`
	Recalls    int    `json:"recalls,omitempty"`
	Resolved   int    `json:"resolved,omitempty"`
}

// Deliver marshals the investigation to JSON and POSTs it to the configured URL.
func (n *Notifier) Deliver(ctx context.Context, inv providers.Investigation) error {
	startedAt := ""
	if !inv.StartedAt.IsZero() {
		startedAt = inv.StartedAt.UTC().Format(time.RFC3339)
	}
	var prior *priorPayload
	if p := inv.Prior; p != nil {
		prior = &priorPayload{Cause: p.Cause, Resolution: p.Resolution, EntryPath: p.EntryPath, Recalls: p.Recalls, Resolved: p.Resolved}
	}
	// Existing-KB match, mirroring the shared Format text's guard: surface it only when
	// Prior is nil, so the structured field never disagrees with the payload's `text`
	// (Prior/recurrence already covers the "seen before" case — don't double-signal).
	var matched *matchedPayload
	if mk := inv.MatchedKnowledge; mk != nil && inv.Prior == nil {
		matched = &matchedPayload{Path: mk.Path, Title: mk.Title, URL: mk.URL, Score: mk.Score}
	}
	body, err := json.Marshal(payload{
		Title:            inv.Title,
		Confidence:       inv.Confidence,
		Namespace:        inv.Resource.Namespace,
		Resource:         inv.Resource.Name,
		CuratedURL:       inv.CuratedURL,
		Text:             notify.Format(inv),
		Verdict:          string(inv.Verdict),
		Severity:         inv.Severity,
		Cluster:          inv.Cluster,
		Environment:      inv.Environment,
		Tenant:           inv.Tenant,
		AlertName:        inv.AlertName,
		StartedAt:        startedAt,
		Occurrences:      inv.Occurrences,
		PrevCuratedURL:   inv.PrevCuratedURL,
		RuledOut:         inv.RuledOut,
		DataGaps:         inv.DataGaps,
		Prior:            prior,
		MatchedKnowledge: matched,
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
