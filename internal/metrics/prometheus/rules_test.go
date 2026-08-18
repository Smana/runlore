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
	var gotPath, gotType string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath, gotType = r.URL.Path, r.URL.Query().Get("type")
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

// An alerting rule with no `for:` is fires-immediately, which must not be
// misreported as a 5m rule.
func TestAlertRulesZeroFor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"groups":[{"name":"g","rules":[
		  {"type":"alerting","name":"Instant","query":"up == 0"}]}]}}`))
	}))
	defer srv.Close()

	rules, err := New(srv.URL).AlertRules(context.Background())
	if err != nil {
		t.Fatalf("AlertRules: %v", err)
	}
	if len(rules) != 1 || rules[0].For != 0 {
		t.Fatalf("unexpected rules: %+v", rules)
	}
}

// vmalert omits "type" on some builds; a rule with a query and no explicit type
// must still be treated as alerting rather than dropped.
func TestAlertRulesUntypedRuleIsKept(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"status":"success","data":{"groups":[{"name":"g","rules":[
		  {"name":"Untyped","query":"up == 0","duration":60}]}]}}`))
	}))
	defer srv.Close()

	rules, err := New(srv.URL).AlertRules(context.Background())
	if err != nil {
		t.Fatalf("AlertRules: %v", err)
	}
	if len(rules) != 1 || rules[0].Name != "Untyped" || rules[0].For != time.Minute {
		t.Fatalf("unexpected rules: %+v", rules)
	}
}
