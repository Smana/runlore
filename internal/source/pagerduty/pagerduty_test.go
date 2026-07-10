package pagerduty

import (
	"os"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/source"
)

// TestDecodeTriggeredProducesRequest locks in the full V3 incident.triggered →
// investigate.Request mapping against a recorded payload (the documented V3
// incident example, event_type set to incident.triggered).
func TestDecodeTriggeredProducesRequest(t *testing.T) {
	body, err := os.ReadFile("testdata/incident_triggered.json")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Source{}.Decode(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(res.Requests))
	}
	r := res.Requests[0]
	if r.Source != investigate.SourcePagerDuty {
		t.Fatalf("want Source=pagerduty, got %q", r.Source)
	}
	if r.Title != "A little bump in the road" {
		t.Fatalf("want incident title, got %q", r.Title)
	}
	if r.Severity != "P1" { // priority wins over urgency when present
		t.Fatalf("want Severity=P1, got %q", r.Severity)
	}
	if r.Reason != "P1" {
		t.Fatalf("want Reason=P1 (mirrors alertmanager Reason=severity), got %q", r.Reason)
	}
	// PagerDuty has no Kubernetes context: workload + environment stay empty. Recall
	// can still fire via the scopeless tier — but only against entries that are
	// themselves resource-less (see investigate.resourceAgrees).
	if r.Workload.Namespace != "" || r.Workload.Kind != "" || r.Workload.Name != "" {
		t.Fatalf("want zero Workload, got %+v", r.Workload)
	}
	if r.Environment != "" {
		t.Fatalf("want empty Environment, got %q", r.Environment)
	}
	// The V3 incident object has no description field, so the message is a
	// composed human-readable summary of the incident metadata.
	for _, part := range []string{"#2", `"API Service"`, "urgency high", "priority P1", "https://acme.pagerduty.com/incidents/PGR0VU2"} {
		if !strings.Contains(r.Message, part) {
			t.Fatalf("Message %q should contain %q", r.Message, part)
		}
	}
	wantLabels := map[string]string{
		"service":         "API Service",
		"service_id":      "PF9KMXH",
		"incident_number": "2",
		"html_url":        "https://acme.pagerduty.com/incidents/PGR0VU2",
		"urgency":         "high",
		"priority":        "P1",
	}
	for k, want := range wantLabels {
		if got := r.Labels[k]; got != want {
			t.Fatalf("Labels[%q] = %q, want %q", k, got, want)
		}
	}
	if r.Fingerprint != "PGR0VU2" || len(r.Fingerprints) != 1 || r.Fingerprints[0] != "PGR0VU2" {
		t.Fatalf("want incident-id fingerprint PGR0VU2 (and Fingerprints=[PGR0VU2]), got %q %v", r.Fingerprint, r.Fingerprints)
	}
	// Per-class dedup key: title + service (in the cluster slot — the only scoping
	// dimension PagerDuty has), same normalization as the alertmanager source.
	if r.TriggerKey != "a little bump in the road||||api service" {
		t.Fatalf("want per-class TriggerKey, got %q", r.TriggerKey)
	}
	wantAt, _ := time.Parse(time.RFC3339, "2020-10-02T18:45:22.169Z")
	if !r.At.Equal(wantAt) {
		t.Fatalf("want At=%v (event.occurred_at), got %v", wantAt, r.At)
	}
	if len(res.Resolved) != 0 {
		t.Fatalf("triggered must not resolve, got %+v", res.Resolved)
	}
}

// TestDecodeResolvedProducesResolution locks in incident.resolved → Resolution
// keyed by the incident id (stable triggered↔resolved), carrying receipt time.
func TestDecodeResolvedProducesResolution(t *testing.T) {
	body, err := os.ReadFile("testdata/incident_resolved.json")
	if err != nil {
		t.Fatal(err)
	}
	res, err := Source{}.Decode(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 0 {
		t.Fatalf("resolved must not enqueue, got %+v", res.Requests)
	}
	if len(res.Resolved) != 1 || res.Resolved[0].Fingerprint != "PGR0VU2" {
		t.Fatalf("want resolved PGR0VU2, got %+v", res.Resolved)
	}
	if res.Resolved[0].At.IsZero() {
		t.Fatal("Resolution.At must not be zero (should be receipt time)")
	}
}

// TestDecodeIgnoredEventTypes: every non-triggered/non-resolved event type is
// dropped without error (202 upstream, nothing ingested).
func TestDecodeIgnoredEventTypes(t *testing.T) {
	for _, et := range []string{
		"incident.acknowledged",
		"incident.annotated",
		"incident.priority_updated",
		"incident.unacknowledged",
		"service.updated",
	} {
		t.Run(et, func(t *testing.T) {
			body := []byte(`{"event":{"event_type":"` + et + `","data":{"id":"PGR0VU2","title":"x"}}}`)
			res, err := Source{}.Decode(body, nil)
			if err != nil {
				t.Fatalf("ignored event type must not error: %v", err)
			}
			if len(res.Requests) != 0 || len(res.Resolved) != 0 {
				t.Fatalf("event %q must be dropped, got %+v", et, res)
			}
		})
	}
}

// TestDecodeSeverityFallsBackToUrgency: no priority on the incident → urgency
// is the severity.
func TestDecodeSeverityFallsBackToUrgency(t *testing.T) {
	body := []byte(`{"event":{"event_type":"incident.triggered","occurred_at":"2020-10-02T18:45:22Z",
		"data":{"id":"P1","number":7,"title":"DB down","urgency":"high"}}}`)
	res, err := Source{}.Decode(body, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 1 {
		t.Fatalf("want 1 request, got %d", len(res.Requests))
	}
	r := res.Requests[0]
	if r.Severity != "high" {
		t.Fatalf("want Severity=high (urgency fallback), got %q", r.Severity)
	}
	if _, ok := r.Labels["priority"]; ok {
		t.Fatalf("empty priority must not appear in labels, got %v", r.Labels)
	}
}

func TestDecodeBadJSON(t *testing.T) {
	if _, err := (Source{}).Decode([]byte(`{not json`), nil); err == nil {
		t.Fatal("want decode error on malformed JSON")
	}
}

// descriptor returns the registered pagerduty source descriptor.
func descriptor(t *testing.T) source.Descriptor {
	t.Helper()
	for _, d := range source.Registered() {
		if d.Name == "pagerduty" {
			return d
		}
	}
	t.Fatal("pagerduty source not registered")
	return source.Descriptor{}
}

func rawSources(t *testing.T, doc string) map[string]yaml.Node {
	t.Helper()
	var raw map[string]yaml.Node
	if err := yaml.Unmarshal([]byte(doc), &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestBuild covers enablement, secret resolution, and the mode=auto fail-closed
// rule (mirrors config.Validate requiring server.webhook_token_env under auto).
func TestBuild(t *testing.T) {
	d := descriptor(t)
	if d.Kind != source.Webhook || d.Path != "/webhook/pagerduty" || d.Admission != source.MatchGated {
		t.Fatalf("bad descriptor: %+v", d)
	}

	t.Run("absent key disables", func(t *testing.T) {
		impl, err := d.Build(source.Deps{Cfg: &config.Config{}, Raw: rawSources(t, "alertmanager: {}")})
		if err != nil || impl != nil {
			t.Fatalf("want disabled (nil, nil), got %v, %v", impl, err)
		}
	})

	t.Run("secret_env resolves via env", func(t *testing.T) {
		t.Setenv("PD_TEST_SECRET", "s3cr3t")
		impl, err := d.Build(source.Deps{Cfg: &config.Config{}, Raw: rawSources(t, "pagerduty: { secret_env: PD_TEST_SECRET }")})
		if err != nil {
			t.Fatal(err)
		}
		if impl.(Source).secret != "s3cr3t" {
			t.Fatalf("secret not resolved from env: %+v", impl)
		}
	})

	t.Run("unset secret builds open source", func(t *testing.T) {
		impl, err := d.Build(source.Deps{Cfg: &config.Config{}, Raw: rawSources(t, "pagerduty: {}")})
		if err != nil {
			t.Fatal(err)
		}
		if impl.(Source).secret != "" {
			t.Fatalf("want empty secret, got %+v", impl)
		}
	})

	t.Run("mode=auto without secret fails closed", func(t *testing.T) {
		cfg := &config.Config{}
		cfg.Actions.Mode = config.ActionAuto
		_, err := d.Build(source.Deps{Cfg: cfg, Raw: rawSources(t, "pagerduty: {}")})
		if err == nil || !strings.Contains(err.Error(), "secret_env") {
			t.Fatalf("want fail-closed error naming secret_env, got %v", err)
		}
	})

	t.Run("malformed block errors", func(t *testing.T) {
		_, err := d.Build(source.Deps{Cfg: &config.Config{}, Raw: rawSources(t, "pagerduty: { secret_env: [1, 2] }")})
		if err == nil {
			t.Fatal("want error on malformed sources.pagerduty block")
		}
	})
}

// TestSecret covers the exported serve-path guard helper.
func TestSecret(t *testing.T) {
	t.Setenv("PD_TEST_SECRET", "s3cr3t")
	cases := []struct {
		name        string
		doc         string
		wantSecret  string
		wantEnabled bool
	}{
		{"absent", "alertmanager: {}", "", false},
		{"enabled with secret", "pagerduty: { secret_env: PD_TEST_SECRET }", "s3cr3t", true},
		{"enabled without secret_env", "pagerduty: {}", "", true},
		{"enabled with unset env var", "pagerduty: { secret_env: PD_TEST_UNSET }", "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			secret, enabled := Secret(rawSources(t, tc.doc))
			if secret != tc.wantSecret || enabled != tc.wantEnabled {
				t.Fatalf("Secret() = (%q, %v), want (%q, %v)", secret, enabled, tc.wantSecret, tc.wantEnabled)
			}
		})
	}
}
