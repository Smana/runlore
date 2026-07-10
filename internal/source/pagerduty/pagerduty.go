// SPDX-License-Identifier: Apache-2.0

// Package pagerduty is the PagerDuty V3 webhook source adapter. It turns
// PagerDuty incident webhooks into investigation requests (incident.triggered)
// and resolutions (incident.resolved), and authenticates each delivery with the
// X-PagerDuty-Signature HMAC scheme.
package pagerduty

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/source"
)

// pdConfig is the `sources.pagerduty` config block. secret_env names the env var
// holding the webhook signing secret (config stores the NAME, never the value —
// consistent with the repo's env-var-indirection pattern).
type pdConfig struct {
	SecretEnv string `yaml:"secret_env"`
}

// osGetenv is a package-level indirection point for os.Getenv (kept trivial;
// tests set real env vars via t.Setenv).
var osGetenv = os.Getenv

// Source is the PagerDuty V3 webhook source adapter. secret is the signing
// secret used to verify X-PagerDuty-Signature; an empty secret leaves the
// webhook open (mirroring the alertmanager source's optional bearer token).
type Source struct {
	secret string
}

// pdPayload is the subset of the PagerDuty V3 webhook envelope we consume. Each
// delivery carries a single event; the incident lives under event.data.
type pdPayload struct {
	Event pdEvent `json:"event"`
}

type pdEvent struct {
	EventType  string     `json:"event_type"`
	OccurredAt string     `json:"occurred_at"`
	Data       pdIncident `json:"data"`
}

// pdIncident is the V3 `incident` event-data object (the shape shared by
// incident.triggered / incident.resolved / …).
type pdIncident struct {
	ID       string `json:"id"`
	HTMLURL  string `json:"html_url"`
	Number   int    `json:"number"`
	Title    string `json:"title"`
	Urgency  string `json:"urgency"`
	Priority *pdRef `json:"priority"` // null when the incident has no priority
	Service  pdRef  `json:"service"`
}

// pdRef is a PagerDuty resource reference; only id + summary are consumed.
type pdRef struct {
	ID      string `json:"id"`
	Summary string `json:"summary"`
}

// Decode parses a PagerDuty V3 webhook body. incident.triggered becomes an
// investigation Request; incident.resolved becomes a Resolution keyed by the
// incident id (stable across the triggered↔resolved pair); every other event
// type is ignored. PagerDuty carries no Kubernetes namespace/workload, so those
// fields stay empty — such workload-less requests can recall only entries that
// are themselves resource-less (the scopeless tier; see investigate.resourceAgrees).
func (Source) Decode(body []byte, _ http.Header) (source.DecodeResult, error) {
	var p pdPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return source.DecodeResult{}, fmt.Errorf("pagerduty: decode webhook body: %w", err)
	}
	inc := p.Event.Data
	switch p.Event.EventType {
	case "incident.resolved":
		return source.DecodeResult{
			Resolved: []source.Resolution{{Fingerprint: inc.ID, At: time.Now()}},
		}, nil
	case "incident.triggered":
		return source.DecodeResult{Requests: []investigate.Request{toRequest(p.Event)}}, nil
	default:
		// acknowledged, priority_updated, service.*, … — not investigation triggers.
		return source.DecodeResult{}, nil
	}
}

// toRequest maps an incident.triggered event to a normalized investigation Request.
func toRequest(ev pdEvent) investigate.Request {
	inc := ev.Data
	// Priority (e.g. "P1") is the operator-set incident importance; fall back to
	// urgency (high/low) when the incident has no priority.
	severity := inc.Urgency
	if inc.Priority != nil && inc.Priority.Summary != "" {
		severity = inc.Priority.Summary
	}
	occurredAt, _ := time.Parse(time.RFC3339, ev.OccurredAt)

	labels := map[string]string{}
	putLabel(labels, "service", inc.Service.Summary)
	putLabel(labels, "service_id", inc.Service.ID)
	if inc.Number != 0 {
		labels["incident_number"] = strconv.Itoa(inc.Number)
	}
	putLabel(labels, "html_url", inc.HTMLURL)
	putLabel(labels, "urgency", inc.Urgency)
	if inc.Priority != nil {
		putLabel(labels, "priority", inc.Priority.Summary)
	}

	var fps []string
	if inc.ID != "" {
		fps = []string{inc.ID}
	}

	return investigate.Request{
		Source:   investigate.SourcePagerDuty,
		Title:    inc.Title,
		Severity: severity,
		Reason:   severity,
		// The V3 incident object has no free-text description, so compose a compact
		// human-readable summary from the incident metadata for the seed prompt.
		Message:      composeMessage(inc),
		Labels:       labels,
		At:           occurredAt,
		Fingerprint:  inc.ID,
		Fingerprints: fps,
		// Host-invariant per-class dedup key. PagerDuty's only scoping dimension is
		// the service, so it takes the cluster slot; namespace/kind/name stay empty.
		// Re-fires of the same incident title on the same service dedupe to one PR (#137).
		TriggerKey: curator.IncidentKey(inc.Title, "", "", "", inc.Service.Summary),
	}
}

func composeMessage(inc pdIncident) string {
	var b strings.Builder
	fmt.Fprintf(&b, "PagerDuty incident #%d", inc.Number)
	if inc.Service.Summary != "" {
		fmt.Fprintf(&b, " on service %q", inc.Service.Summary)
	}
	if inc.Urgency != "" {
		fmt.Fprintf(&b, ", urgency %s", inc.Urgency)
	}
	if inc.Priority != nil && inc.Priority.Summary != "" {
		fmt.Fprintf(&b, ", priority %s", inc.Priority.Summary)
	}
	if inc.HTMLURL != "" {
		fmt.Fprintf(&b, " (%s)", inc.HTMLURL)
	}
	return b.String()
}

func putLabel(m map[string]string, k, v string) {
	if v != "" {
		m[k] = v
	}
}

// Secret reads the PagerDuty source config from the raw `sources:` map and
// resolves its signing secret from the env var named by secret_env. It reports
// the resolved secret and whether the source is enabled (its key is present).
// It lives here (not just in Build) so the serve path can enforce fail-closed
// auth before wiring the webhook. Config errors collapse to (empty, present):
// Build re-decodes the same block and surfaces the error at startup.
func Secret(raw map[string]yaml.Node) (secret string, enabled bool) {
	node, ok := raw["pagerduty"]
	if !ok {
		return "", false
	}
	var c pdConfig
	if err := node.Decode(&c); err != nil {
		return "", true
	}
	if c.SecretEnv == "" {
		return "", true
	}
	return osGetenv(c.SecretEnv), true
}

func init() {
	source.Register(source.Descriptor{
		Name: "pagerduty",
		Kind: source.Webhook, Admission: source.MatchGated, Path: "/webhook/pagerduty",
		Build: func(d source.Deps) (any, error) {
			node, ok := d.Raw["pagerduty"]
			if !ok {
				return nil, nil // disabled: no sources.pagerduty key
			}
			var c pdConfig
			if err := node.Decode(&c); err != nil {
				return nil, fmt.Errorf("pagerduty: decode sources.pagerduty: %w", err)
			}
			secret := ""
			if c.SecretEnv != "" {
				secret = osGetenv(c.SecretEnv)
			}
			// Fail closed under mode=auto: an unattended executor must not accept
			// unauthenticated incident webhooks. Mirrors config.Validate's
			// server.webhook_token_env requirement for the shared bearer webhook.
			if d.Cfg != nil && d.Cfg.Actions.Mode == config.ActionAuto && secret == "" {
				return nil, fmt.Errorf("actions.mode=auto requires sources.pagerduty.secret_env (the PagerDuty webhook must verify signatures)")
			}
			return Source{secret: secret}, nil
		},
	})
}
