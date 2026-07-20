// SPDX-License-Identifier: Apache-2.0

package custom

import (
	"net/http"
	"testing"

	"github.com/Smana/runlore/internal/investigate"
)

func grafanaInstance(t *testing.T) *Source {
	t.Helper()
	insts, err := parseConfig(mustNode(t, `
instances:
  grafana:
    items: alerts
    fields:
      title: labels.alertname
      message: annotations.summary
      severity: labels.severity
      namespace: labels.namespace
      workload_name: labels.pod
      fingerprint: fingerprint
      resolved: status
    labels: labels
    severity_map: {P1: critical}
    defaults: {environment: prod, severity: warning}
`))
	if err != nil {
		t.Fatal(err)
	}
	return &Source{instances: insts}
}

func hdr(instance string) http.Header {
	h := http.Header{}
	h.Set("X-Runlore-Instance", instance)
	return h
}

const grafanaBody = `{"alerts":[
  {"status":"firing","fingerprint":"fp1","labels":{"alertname":"HighCPU","severity":"P1","namespace":"payments","pod":"api-0"},"annotations":{"summary":"CPU is high"}},
  {"status":"resolved","fingerprint":"fp2","labels":{"alertname":"OldAlert"}},
  {"status":"firing","labels":{"alertname":"NoSeverity"}}
]}`

func TestDecodeGrafanaShape(t *testing.T) {
	s := grafanaInstance(t)
	res, err := s.Decode([]byte(grafanaBody), hdr("grafana"))
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 2 || len(res.Resolved) != 1 {
		t.Fatalf("got %d requests / %d resolved, want 2 / 1", len(res.Requests), len(res.Resolved))
	}
	r := res.Requests[0]
	if r.Source != investigate.SourceCustom || r.Title != "HighCPU" || r.Message != "CPU is high" {
		t.Errorf("request basics wrong: %+v", r)
	}
	if r.Severity != "critical" { // P1 through severity_map
		t.Errorf("severity = %q, want critical (mapped)", r.Severity)
	}
	if r.Workload.Namespace != "payments" || r.Workload.Name != "api-0" {
		t.Errorf("workload wrong: %+v", r.Workload)
	}
	if r.Environment != "prod" { // default applied
		t.Errorf("environment = %q, want prod (default)", r.Environment)
	}
	if r.Fingerprint != "fp1" || r.TriggerKey == "" || r.Labels["instance"] != "grafana" || r.Labels["alertname"] != "HighCPU" {
		t.Errorf("identity fields wrong: %+v", r)
	}
	if res.Resolved[0].Fingerprint != "fp2" {
		t.Errorf("resolution fingerprint = %q", res.Resolved[0].Fingerprint)
	}
	if res.Requests[1].Severity != "warning" { // default when path yields nothing
		t.Errorf("default severity not applied: %q", res.Requests[1].Severity)
	}
}

func TestDecodeSingleEventAtRoot(t *testing.T) {
	insts, err := parseConfig(mustNode(t, `
instances:
  datadog:
    fields: {title: title, message: body, severity: alert_type}
`))
	if err != nil {
		t.Fatal(err)
	}
	s := &Source{instances: insts}
	res, err := s.Decode([]byte(`{"title":"[Triggered] disk","body":"disk full","alert_type":"error"}`), hdr("datadog"))
	if err != nil || len(res.Requests) != 1 {
		t.Fatalf("res=%+v err=%v", res, err)
	}
	if res.Requests[0].Title != "[Triggered] disk" || res.Requests[0].Severity != "error" {
		t.Errorf("request wrong: %+v", res.Requests[0])
	}
}

func TestDecodeUnknownInstanceErrors(t *testing.T) {
	s := grafanaInstance(t)
	if _, err := s.Decode([]byte(`{}`), hdr("nope")); err == nil {
		t.Fatal("want error for unknown instance")
	}
	if _, err := s.Decode([]byte(`{}`), http.Header{}); err == nil {
		t.Fatal("want error for missing instance header")
	}
}
