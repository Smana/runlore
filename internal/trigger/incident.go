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
	Alerts []amAlert `json:"alerts"`
}

type amAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	StartsAt    string            `json:"startsAt"`
	Fingerprint string            `json:"fingerprint"`
}

// ParseAlertmanager reads an Alertmanager webhook body into incidents. Only
// firing alerts are returned. "environment" is taken from the label of the same
// name, falling back to "env".
func ParseAlertmanager(r io.Reader) ([]config.Incident, error) {
	var p amPayload
	if err := json.NewDecoder(r).Decode(&p); err != nil {
		return nil, err
	}
	out := make([]config.Incident, 0, len(p.Alerts))
	for _, a := range p.Alerts {
		if a.Status != "" && a.Status != "firing" {
			continue
		}
		startsAt, _ := time.Parse(time.RFC3339, a.StartsAt)
		out = append(out, config.Incident{
			AlertName:   a.Labels["alertname"],
			Severity:    a.Labels["severity"],
			Environment: cmp.Or(a.Labels["environment"], a.Labels["env"]),
			Namespace:   a.Labels["namespace"],
			Labels:      a.Labels,
			StartsAt:    startsAt,
			Fingerprint: a.Fingerprint,
		})
	}
	return out, nil
}
