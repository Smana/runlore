// SPDX-License-Identifier: Apache-2.0

package prometheus

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// rulesResponse is the `data` shape of GET /api/v1/rules, which Prometheus and
// VictoriaMetrics/vmalert serve identically: rule groups, each with its file and a
// mixed list of alerting and recording rules.
type rulesResponse struct {
	Groups []struct {
		Name  string    `json:"name"`
		File  string    `json:"file"`
		Rules []apiRule `json:"rules"`
	} `json:"groups"`
}

// apiRule is one rule inside a group. Alerting and recording rules share this one
// shape on BOTH backends — neither emits a distinct `record` key, both report the
// output series and the alertname in the same "name" field — so "type", and
// failing that isAlerting's shape test, is the only discriminator available.
type apiRule struct {
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
}

// isAlerting reports whether this rule is an ALERTING rule.
//
// "type" is authoritative when present, and both currently supported backends set
// it. It is absent only on older vmalert builds, and dropping those outright would
// return an empty ruleset on exactly the backend this was built for — so an untyped
// rule is judged by SHAPE instead, using fields only an alerting rule can carry
// under the /api/v1/rules contract: a `for:` hold-down, alert annotations, or one
// of the alert-only states (a recording rule reports "ok", never firing/pending/
// inactive).
//
// Defaulting an untyped, alert-shaped-in-no-way rule to "alerting" is NOT a
// harmless over-inclusion, which is why the shape test has to be positive: a
// recording rule that leaks through is rendered to the model with `expr:` as
// though it were a threshold, and its name is offered by renderUnmatchedAlert as
// an alertname to "correct" to — reintroducing, through the degraded path, exactly
// the wrong-expression misdiagnosis this capability exists to remove. Dropping a
// genuine alerting rule instead costs a "no rule found" note, which is safe.
func (r apiRule) isAlerting() bool {
	switch r.Type {
	case "alerting":
		return true
	case "":
		// Untyped: fall through to the shape test below.
	default: // "recording", or any type a future backend invents
		return false
	}
	return r.Duration > 0 || len(r.Annotations) > 0 ||
		r.State == "firing" || r.State == "pending" || r.State == "inactive"
}

// AlertRules reads the backend's ALERTING rule definitions, implementing the
// optional providers.AlertRuleReader capability via GET /api/v1/rules?type=alert.
//
// names scopes the read server-side through repeated `rule_name[]` parameters,
// which Prometheus and vmalert both honour. Measured against the real backend
// (74 groups / 278 alerting rules / 254 alertnames), the unscoped alerting ruleset
// is 348,429 B and the same request scoped to one alertname is 25,531 B — 93% less
// to serve, transfer, decode and hold, to answer the one question the caller has.
//
// Do not expect better than that: vmalert still emits ALL 74 group envelopes,
// carrying `rules: []` for the groups that matched nothing, so ~24 KB of envelope
// is the floor here regardless of how narrow the scope is.
//
// The scoping stays a HINT, per the capability contract. A backend that ignores the
// parameter returns the full set (harmless — the caller filters anyway), and one
// that mishandles it may return nothing. That second case is not hypothetical
// bookkeeping: on this very backend a scoped read for an alertname that does not
// exist returns 74 groups and ZERO rules, which is byte-for-byte what a mishandled
// parameter would produce. An empty scoped result therefore proves nothing, and the
// caller must re-read unscoped before concluding "that alertname has no rule".
//
// Blank names are dropped rather than sent: `rule_name[]=` on a backend that DOES
// scope matches nothing, manufacturing exactly that false negative.
//
// It is strictly read-only. `type=alert` keeps recording rules off the wire on a
// large ruleset, and `exclude_alerts=true` drops the per-rule `alerts[]` array of
// ACTIVE INSTANCES, which nothing below decodes. On a real 278-rule backend that
// array is ~160 KB of a ~509 KB response, and it grows with how broken the cluster
// is — i.e. exactly when this tool is called. Without it, a large enough incident
// pushes the body past httpx.MaxResponseBytes (a hard error, not a truncation) and
// the tool degrades to "unavailable" during the outage it exists for. Prometheus
// and vmalert both honour the parameter; a backend that does not simply sends the
// array and it is ignored.
//
// `type=alert` is the primary recording-rule guard and both supported backends
// honour it; isAlerting re-filters in Go so a backend that ignores the parameter
// cannot feed a recording rule to the model as though it were a threshold.
//
// A backend that does not serve the endpoint yields the usual non-200 error (e.g.
// "metrics status 404: …"), which the caller degrades on — the error is returned
// rather than swallowed so "the endpoint is absent" never reads as "there are no
// rules".
func (c *Client) AlertRules(ctx context.Context, names ...string) ([]providers.AlertRule, error) {
	v := url.Values{"type": {"alert"}, "exclude_alerts": {"true"}}
	for _, n := range names {
		if n = strings.TrimSpace(n); n != "" {
			v.Add("rule_name[]", n)
		}
	}
	raw, err := c.getRaw(ctx, "/api/v1/rules", v)
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
			if !r.isAlerting() {
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
