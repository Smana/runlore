// Package trigger ingests incidents (Alertmanager/VMAlert webhooks) and decides,
// per the configured policy, which ones start an investigation.
package trigger

import (
	"cmp"
	"encoding/json"
	"io"
	"time"

	"github.com/Smana/runlore/internal/config"
)

// amPayload is the subset of the Alertmanager webhook payload we consume.
type amPayload struct {
	GroupKey string    `json:"groupKey"`
	Alerts   []amAlert `json:"alerts"`
}

type amAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	StartsAt    string            `json:"startsAt"`
	Fingerprint string            `json:"fingerprint"`
}

// ParseAlertmanager reads an Alertmanager webhook body into incidents. Both
// firing and resolved alerts are returned, each tagged with its Status (the
// caller routes resolved ones to the outcome ledger). "environment" is taken
// from the label of the same name, falling back to "env".
func ParseAlertmanager(r io.Reader) ([]config.Incident, error) {
	var p amPayload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, err
	}
	out := make([]config.Incident, 0, len(p.Alerts))
	for _, a := range p.Alerts {
		startsAt, _ := time.Parse(time.RFC3339, a.StartsAt)
		out = append(out, config.Incident{
			AlertName:   a.Labels["alertname"],
			Severity:    a.Labels["severity"],
			Environment: cmp.Or(a.Labels["environment"], a.Labels["env"]),
			Namespace:   a.Labels["namespace"],
			Labels:      a.Labels,
			StartsAt:    startsAt,
			Fingerprint: a.Fingerprint,
			GroupKey:    p.GroupKey,
			Status:      a.Status,
		})
	}
	return out, nil
}
