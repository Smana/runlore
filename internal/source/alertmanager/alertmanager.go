// SPDX-License-Identifier: Apache-2.0

// Package alertmanager is the Alertmanager/VMAlert webhook source adapter.
package alertmanager

import (
	"cmp"
	"encoding/json"
	"net/http"
	"time"

	"github.com/Smana/runlore/internal/curator"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/source"
)

// Source is the Alertmanager/VMAlert webhook source adapter. It implements
// source.WebhookSource by parsing the Alertmanager webhook payload into
// investigation requests (firing alerts) and resolutions (resolved alerts).
type Source struct{}

// amPayload is the subset of the Alertmanager webhook payload we consume.
type amPayload struct {
	GroupKey string    `json:"groupKey"`
	Alerts   []amAlert `json:"alerts"`
}

type amAlert struct {
	Status      string            `json:"status"`
	Labels      map[string]string `json:"labels"`
	Annotations map[string]string `json:"annotations"`
	StartsAt    string            `json:"startsAt"`
	Fingerprint string            `json:"fingerprint"`
}

// workloadFromLabels derives the affected workload (kind, name) from Alertmanager
// labels, preferring a stable controller name over an ephemeral pod name.
func workloadFromLabels(labels map[string]string) (kind, name string) {
	for _, c := range []struct{ label, kind string }{
		{"deployment", "Deployment"},
		{"statefulset", "StatefulSet"},
		{"daemonset", "DaemonSet"},
		{"replicaset", "ReplicaSet"},
		{"cronjob", "CronJob"},
		{"job", "Job"},
	} {
		if v := labels[c.label]; v != "" {
			return c.kind, v
		}
	}
	if v := labels["workload"]; v != "" {
		return labels["workload_type"], v // kind may be empty
	}
	if v := labels["pod"]; v != "" {
		return "Pod", v
	}
	return "", ""
}

// Decode parses an Alertmanager webhook body into investigation requests (firing
// alerts) and resolutions (resolved alerts). "environment" is taken from the label
// of the same name, falling back to "env". Resolved alerts carry the receipt time.
func (Source) Decode(body []byte, _ http.Header) (source.DecodeResult, error) {
	var p amPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return source.DecodeResult{}, err
	}
	var out source.DecodeResult
	for _, a := range p.Alerts {
		if a.Status == "resolved" {
			out.Resolved = append(out.Resolved, source.Resolution{Fingerprint: a.Fingerprint, At: time.Now()})
			continue
		}
		startsAt, _ := time.Parse(time.RFC3339, a.StartsAt)
		kind, name := workloadFromLabels(a.Labels)
		var fps []string
		if a.Fingerprint != "" {
			fps = []string{a.Fingerprint}
		}
		out.Requests = append(out.Requests, investigate.Request{
			Source:      investigate.SourceAlert,
			Title:       a.Labels["alertname"],
			Severity:    a.Labels["severity"],
			Environment: cmp.Or(a.Labels["environment"], a.Labels["env"]),
			Workload:    providers.Workload{Namespace: a.Labels["namespace"], Kind: kind, Name: name},
			Reason:      a.Labels["severity"],
			// The description/summary annotation is the alert's most informative human
			// text (the templated "what is wrong") — without it the seed prompt carried
			// only the alertname. Remaining annotations (runbook_url, dashboards, …)
			// travel on Annotations for the seed prompt to surface.
			Message:      cmp.Or(a.Annotations["description"], a.Annotations["summary"], a.Annotations["message"]),
			Labels:       a.Labels,
			Annotations:  a.Annotations,
			At:           startsAt,
			Fingerprint:  a.Fingerprint,
			Fingerprints: fps,
			GroupKey:     p.GroupKey,
			// Host-invariant per-class dedup key (alertname + workload family +
			// cluster, pod-hash suffix stripped): dedupes re-fires of one series
			// (#137) AND the same alert on a different pod/node (CORE-681). Attribution
			// still uses the per-series Alertmanager Fingerprint above.
			TriggerKey: curator.IncidentKey(a.Labels["alertname"], a.Labels["namespace"], kind, name, a.Labels["cluster"]),
		})
	}
	return out, nil
}

func init() {
	source.Register(source.Descriptor{
		Name: "alertmanager",
		Kind: source.Webhook, Admission: source.MatchGated, Path: "/webhook/alertmanager",
		Build: func(d source.Deps) (any, error) {
			// Presence of the sources.alertmanager key enables this source. The match
			// policy stays at triggers.incidents; webhook auth stays server-level.
			if _, ok := d.Raw["alertmanager"]; !ok {
				return nil, nil
			}
			return Source{}, nil
		},
	})
}
