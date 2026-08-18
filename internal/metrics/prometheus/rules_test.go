// SPDX-License-Identifier: Apache-2.0

package prometheus

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// The Client must satisfy the OPTIONAL rules capability, so the alert_rule tool's
// type assertion succeeds against a real Prometheus/VictoriaMetrics backend.
var _ providers.AlertRuleReader = (*Client)(nil)

// rulesPayload is the /api/v1/rules envelope both Prometheus and vmalert serve.
const rulesPayload = `{"status":"success","data":{"groups":[
  {"name":"rds","file":"/etc/vmalert/rds.yaml","rules":[
    {"type":"alerting","name":"RDSCriticalLatency",
     "query":"(aws_rds_read_latency_maximum > 0.050) or (aws_rds_write_latency_maximum{cluster=~\".*\"} > 0.050)",
     "duration":300,"state":"firing","health":"ok",
     "labels":{"severity":"critical"},"annotations":{"summary":"RDS latency above 50ms"}},
    {"type":"recording","name":"rds:latency:avg","query":"avg(aws_rds_read_latency_average)"}]},
  {"name":"kube","file":"/etc/vmalert/kube.yaml","rules":[
    {"type":"alerting","name":"KubePodCrashLooping","query":"rate(restarts[5m]) > 0",
     "duration":0,"state":"inactive","health":"err","lastError":"vector selector must have at least one matcher"}]}]}}`

func TestAlertRules(t *testing.T) {
	var gotPath, gotType, gotExclude string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotType = r.URL.Path, r.URL.Query().Get("type")
		gotExclude = r.URL.Query().Get("exclude_alerts")
		_, _ = w.Write([]byte(rulesPayload))
	}))
	defer srv.Close()

	rules, err := New(srv.URL).AlertRules(context.Background())
	if err != nil {
		t.Fatalf("AlertRules: %v", err)
	}
	if gotPath != "/api/v1/rules" {
		t.Fatalf("path=%q, want /api/v1/rules", gotPath)
	}
	// type=alert keeps recording rules off the wire on a big ruleset.
	if gotType != "alert" {
		t.Fatalf("type=%q, want alert", gotType)
	}
	// exclude_alerts drops the per-rule active-instance array, which nothing decodes
	// and which grows with the size of the incident — the payload that would
	// otherwise push a busy backend past httpx.MaxResponseBytes mid-outage.
	if gotExclude != "true" {
		t.Fatalf("exclude_alerts=%q, want true", gotExclude)
	}
	// The recording rule must not be returned even if the backend ignored type=alert.
	if len(rules) != 2 {
		t.Fatalf("got %d rules, want 2 alerting rules: %+v", len(rules), rules)
	}
	r := rules[0]
	if r.Name != "RDSCriticalLatency" {
		t.Fatalf("name=%q", r.Name)
	}
	if !strings.Contains(r.Query, "aws_rds_write_latency_maximum") {
		t.Fatalf("query=%q", r.Query)
	}
	if r.For != 5*time.Minute {
		t.Fatalf("for=%v, want 5m", r.For)
	}
	if r.State != "firing" || r.Health != "ok" {
		t.Fatalf("state=%q health=%q", r.State, r.Health)
	}
	if r.Group != "rds" || r.File != "/etc/vmalert/rds.yaml" {
		t.Fatalf("group=%q file=%q", r.Group, r.File)
	}
	if r.Labels["severity"] != "critical" || r.Annotations["summary"] == "" {
		t.Fatalf("labels=%v annotations=%v", r.Labels, r.Annotations)
	}
	// A rule whose evaluation is broken carries its error, so "the alert is stale"
	// becomes checkable rather than assumed.
	if rules[1].Health != "err" || !strings.Contains(rules[1].LastError, "vector selector") {
		t.Fatalf("second rule health=%q lastError=%q", rules[1].Health, rules[1].LastError)
	}
}

// A backend without a rules endpoint (404) must surface an error the caller can
// degrade on — never a panic or a silently empty ruleset that reads as "no rules".
func TestAlertRulesNotFound(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("unsupported path"))
	}))
	defer srv.Close()

	if _, err := New(srv.URL).AlertRules(context.Background()); err == nil {
		t.Fatal("want an error for a 404 rules endpoint")
	} else if !strings.Contains(err.Error(), "404") {
		t.Fatalf("error must name the status: %v", err)
	}
}

// rulesFrom serves one /api/v1/rules payload and returns what AlertRules made of
// it, so a new parse edge case is a table row rather than a fourth copy of the
// httptest scaffold.
func rulesFrom(t *testing.T, payload string) []providers.AlertRule {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(payload))
	}))
	defer srv.Close()
	rules, err := New(srv.URL).AlertRules(context.Background())
	if err != nil {
		t.Fatalf("AlertRules: %v", err)
	}
	return rules
}

// The parse edge cases that decide WHICH rules reach the model. Getting these
// wrong is not a cosmetic bug: a recording rule presented as an alerting one is
// rendered with `expr:` as though it were a threshold, which is the misdiagnosis
// the whole capability exists to prevent.
func TestAlertRulesParsing(t *testing.T) {
	tests := []struct {
		name     string
		payload  string
		wantName []string
		wantFor  []time.Duration
	}{
		{
			// An alerting rule with no `for:` fires immediately, which must not be
			// misreported as a 5m rule.
			name: "missing duration is a fires-immediately rule, not a defaulted one",
			payload: `{"status":"success","data":{"groups":[{"name":"g","rules":[
			  {"type":"alerting","name":"Instant","query":"up == 0"}]}]}}`,
			wantName: []string{"Instant"},
			wantFor:  []time.Duration{0},
		},
		{
			// vmalert omits "type" on some builds; a rule that is alert-SHAPED (it has a
			// `for:`) must still be treated as alerting rather than dropped, or the
			// ruleset comes back empty on exactly the backend this was built for.
			name: "untyped but alert-shaped rule is kept (older vmalert omits type)",
			payload: `{"status":"success","data":{"groups":[{"name":"g","rules":[
			  {"name":"Untyped","query":"up == 0","duration":60}]}]}}`,
			wantName: []string{"Untyped"},
			wantFor:  []time.Duration{time.Minute},
		},
		{
			// An alert-only state is enough on its own — a recording rule reports "ok".
			name: "untyped rule in an alert-only state is kept",
			payload: `{"status":"success","data":{"groups":[{"name":"g","rules":[
			  {"name":"Pending","query":"up == 0","state":"pending"}]}]}}`,
			wantName: []string{"Pending"},
			wantFor:  []time.Duration{0},
		},
		{
			// The dangerous case: a backend that BOTH omits "type" and ignores
			// ?type=alert. Keeping every untyped rule would hand the recording rule to
			// the model as an alertname it could "correct" to, and render its expr as a
			// threshold. Only the alert-shaped rule may survive.
			name: "untyped recording rule is dropped even when the backend ignores type=alert",
			payload: `{"status":"success","data":{"groups":[{"name":"g","rules":[
			  {"name":"job:latency:avg","query":"avg(rds_latency)","state":"ok","health":"ok"},
			  {"name":"RealAlert","query":"up == 0","duration":300,"state":"firing",
			   "annotations":{"summary":"down"}}]}]}}`,
			wantName: []string{"RealAlert"},
			wantFor:  []time.Duration{5 * time.Minute},
		},
		{
			name: "explicitly typed recording rules are dropped",
			payload: `{"status":"success","data":{"groups":[{"name":"g","rules":[
			  {"type":"recording","name":"job:latency:avg","query":"avg(rds_latency)","duration":60}]}]}}`,
			wantName: []string{},
			wantFor:  []time.Duration{},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			rules := rulesFrom(t, tc.payload)
			if len(rules) != len(tc.wantName) {
				t.Fatalf("got %d rules, want %d: %+v", len(rules), len(tc.wantName), rules)
			}
			for i, want := range tc.wantName {
				if rules[i].Name != want {
					t.Fatalf("rule %d name=%q, want %q", i, rules[i].Name, want)
				}
				if rules[i].For != tc.wantFor[i] {
					t.Fatalf("rule %d for=%v, want %v", i, rules[i].For, tc.wantFor[i])
				}
			}
		})
	}
}
