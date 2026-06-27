// Package alertmanager is the Alertmanager/VMAlert webhook source adapter.
package alertmanager

import (
	"bytes"
	"net/http"
	"time"

	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/source"
	"github.com/Smana/runlore/internal/trigger"
)

type Source struct{}

func (Source) Decode(body []byte, _ http.Header) (source.DecodeResult, error) {
	incidents, err := trigger.ParseAlertmanager(bytes.NewReader(body))
	if err != nil {
		return source.DecodeResult{}, err
	}
	var out source.DecodeResult
	for _, inc := range incidents {
		if inc.Status == "resolved" {
			out.Resolved = append(out.Resolved, source.Resolution{Fingerprint: inc.Fingerprint, At: time.Now()})
			continue
		}
		out.Requests = append(out.Requests, investigate.FromIncident(inc))
	}
	return out, nil
}

func init() {
	source.Register(source.Descriptor{
		Name: "alertmanager", ConfigKey: "sources.alertmanager",
		Kind: source.Webhook, Admission: source.MatchGated, Path: "/webhook/alertmanager",
		Build: func(d source.Deps) (any, error) {
			// Enabled when the incident trigger is enabled (Phase 3 moves this to sources.alertmanager).
			if !d.Cfg.Triggers.Incidents.Enabled {
				return nil, nil
			}
			return Source{}, nil
		},
	})
}
