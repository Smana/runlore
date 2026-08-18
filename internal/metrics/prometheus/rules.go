// SPDX-License-Identifier: Apache-2.0

package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// rulesResponse is the `data` shape of GET /api/v1/rules, which Prometheus and
// VictoriaMetrics/vmalert serve identically: rule groups, each with its file and a
// mixed list of alerting and recording rules.
type rulesResponse struct {
	Groups []struct {
		Name  string `json:"name"`
		File  string `json:"file"`
		Rules []struct {
			Type string `json:"type"` // "alerting" | "recording"; omitted by some vmalert builds
			Name string `json:"name"`
			// Query is the thresholded expression — the field the whole capability
			// exists to deliver.
			Query string `json:"query"`
			// Duration is the `for:` hold-down IN SECONDS (both backends render it as a
			// number, not a duration string), absent for a fires-immediately rule.
			Duration    float64           `json:"duration"`
			State       string            `json:"state"`
			Health      string            `json:"health"`
			LastError   string            `json:"lastError"`
			Labels      map[string]string `json:"labels"`
			Annotations map[string]string `json:"annotations"`
		} `json:"rules"`
	} `json:"groups"`
}

// AlertRules reads the backend's ALERTING rule definitions, implementing the
// optional providers.AlertRuleReader capability via GET /api/v1/rules?type=alert.
//
// It is strictly read-only. `type=alert` keeps recording rules off the wire on a
// large ruleset, and the response is filtered again in Go because a backend that
// does not honour the parameter would otherwise return them anyway. A rule with no
// explicit "type" is KEPT as alerting: some vmalert builds omit the field, and
// dropping those would silently return an empty ruleset on exactly the backend
// this was built for.
//
// A backend that does not serve the endpoint yields the usual non-200 error (e.g.
// "metrics status 404: …"), which the caller degrades on — the error is returned
// rather than swallowed so "the endpoint is absent" never reads as "there are no
// rules".
func (c *Client) AlertRules(ctx context.Context) ([]providers.AlertRule, error) {
	raw, err := c.getRaw(ctx, "/api/v1/rules", url.Values{"type": {"alert"}})
	if err != nil {
		return nil, err
	}
	var resp rulesResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return nil, fmt.Errorf("parse alerting rules: %w", err)
	}
	var out []providers.AlertRule
	for _, g := range resp.Groups {
		for _, r := range g.Rules {
			if r.Type != "" && r.Type != "alerting" {
				continue
			}
			out = append(out, providers.AlertRule{
				Name:        r.Name,
				Query:       r.Query,
				For:         time.Duration(r.Duration * float64(time.Second)),
				State:       r.State,
				Health:      r.Health,
				LastError:   r.LastError,
				Group:       g.Name,
				File:        g.File,
				Labels:      r.Labels,
				Annotations: r.Annotations,
			})
		}
	}
	return out, nil
}
