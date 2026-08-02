// SPDX-License-Identifier: Apache-2.0

package grafana

import (
	"net/http"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/source"
)

// TestDefaultMappingMatchesDocs is the defining test: the built-in mapping
// must equal, field for field, the table documented at
// website/content/docs/integrations/grafana.md (carried forward from the
// hand-written example that used to live at
// website/content/docs/integrations/custom-webhook.md). A change to either
// side must show up here as a visible diff — never a silent behaviour change.
func TestDefaultMappingMatchesDocs(t *testing.T) {
	documented := map[string]string{
		"title":         "labels.alertname",
		"message":       "annotations.summary",
		"severity":      "labels.severity",
		"namespace":     "labels.namespace",
		"workload_name": "labels.pod",
		"fingerprint":   "fingerprint",
		"resolved":      "status",
	}
	if len(defaultFields) != len(documented) {
		t.Fatalf("defaultFields has %d entries, documented table has %d", len(defaultFields), len(documented))
	}
	for field, path := range documented {
		if got := defaultFields[field]; got != path {
			t.Errorf("fields.%s = %q, documented default is %q", field, got, path)
		}
	}
	if defaultItems != "alerts" {
		t.Errorf("items = %q, documented default is %q", defaultItems, "alerts")
	}
	if defaultLabels != "labels" {
		t.Errorf("labels = %q, documented default is %q", defaultLabels, "labels")
	}
}

// mustNode parses y as a YAML document and unwraps it to the root content
// node, mirroring internal/source/custom's own test helper.
func mustNode(t *testing.T, y string) yaml.Node {
	t.Helper()
	var n yaml.Node
	if err := yaml.Unmarshal([]byte(y), &n); err != nil {
		t.Fatal(err)
	}
	return *n.Content[0]
}

// buildFor drives the registered "grafana" descriptor's Build func directly,
// the same way internal/source/custom's own TestBuildFailClosedUnderAuto
// does, so these tests exercise the actual registration path rather than a
// look-alike.
func buildFor(t *testing.T, grafanaYAML string, cfg *config.Config) (*Source, error) {
	t.Helper()
	var build func(source.Deps) (any, error)
	for _, d := range source.Registered() {
		if d.Name == "grafana" {
			build = d.Build
		}
	}
	if build == nil {
		t.Fatal("grafana source not registered")
	}
	raw := map[string]yaml.Node{}
	if grafanaYAML != "" {
		raw["grafana"] = mustNode(t, grafanaYAML)
	}
	if cfg == nil {
		cfg = &config.Config{}
	}
	impl, err := build(source.Deps{Cfg: cfg, Raw: raw})
	if err != nil {
		return nil, err
	}
	if impl == nil {
		return nil, nil
	}
	s, ok := impl.(*Source)
	if !ok {
		t.Fatalf("Build returned %T, want *Source", impl)
	}
	return s, nil
}

const batchBody = `{"alerts":[
  {"status":"firing","fingerprint":"fp1","labels":{"alertname":"HighCPU","severity":"critical","namespace":"payments","pod":"api-0"},"annotations":{"summary":"CPU is high"}},
  {"status":"firing","fingerprint":"fp2","labels":{"alertname":"DiskFull","severity":"warning","namespace":"billing","pod":"billing-2"},"annotations":{"summary":"disk almost full"}},
  {"status":"resolved","fingerprint":"fp3","labels":{"alertname":"OldAlert"}}
]}`

func TestDecodeBatch(t *testing.T) {
	s, err := buildFor(t, `{}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Decode([]byte(batchBody), http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 2 {
		t.Fatalf("got %d requests, want 2", len(res.Requests))
	}
	r0 := res.Requests[0]
	if r0.Source != investigate.SourceCustom || r0.Title != "HighCPU" || r0.Message != "CPU is high" {
		t.Errorf("request[0] basics wrong: %+v", r0)
	}
	if r0.Workload.Namespace != "payments" || r0.Workload.Name != "api-0" {
		t.Errorf("request[0] workload wrong: %+v", r0.Workload)
	}
	if r0.Severity != "critical" || r0.Fingerprint != "fp1" {
		t.Errorf("request[0] severity/fingerprint wrong: %+v", r0)
	}
	r1 := res.Requests[1]
	if r1.Title != "DiskFull" || r1.Workload.Namespace != "billing" || r1.Workload.Name != "billing-2" {
		t.Errorf("request[1] wrong: %+v", r1)
	}
}

// TestDecodeResolvedRecordsResolution: a resolved event must record a
// resolution, not trigger a second investigation.
func TestDecodeResolvedRecordsResolution(t *testing.T) {
	s, err := buildFor(t, `{}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Decode([]byte(batchBody), http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Resolved) != 1 {
		t.Fatalf("got %d resolutions, want 1", len(res.Resolved))
	}
	if res.Resolved[0].Fingerprint != "fp3" {
		t.Errorf("resolution fingerprint = %q, want fp3", res.Resolved[0].Fingerprint)
	}
	// The resolved alert must not also appear as a firing request.
	for _, r := range res.Requests {
		if r.Fingerprint == "fp3" {
			t.Errorf("resolved alert fp3 also produced an investigation request: %+v", r)
		}
	}
}

func TestTokenEnvFallsBackToServerWebhookTokenEnv(t *testing.T) {
	t.Setenv("SHARED_TOK", "s3cret")
	cfg := &config.Config{}
	cfg.Server.WebhookTokenEnv = "SHARED_TOK"
	cfg.Actions.Mode = config.ActionAuto // fail-closed path: must accept the shared token

	s, err := buildFor(t, `{}`, cfg) // no token_env of its own
	if err != nil {
		t.Fatalf("build should succeed via the shared token fallback: %v", err)
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer s3cret")
	if !s.Authenticate([]byte(`{}`), h) {
		t.Error("Authenticate should accept the shared server.webhook_token_env token")
	}
	h.Set("Authorization", "Bearer wrong")
	if s.Authenticate([]byte(`{}`), h) {
		t.Error("Authenticate should reject a token that does not match")
	}
}

func TestTokenEnvOverridesShared(t *testing.T) {
	t.Setenv("GRAFANA_TOK", "g3cret")
	t.Setenv("SHARED_TOK", "shared")
	cfg := &config.Config{}
	cfg.Server.WebhookTokenEnv = "SHARED_TOK"

	s, err := buildFor(t, `{token_env: GRAFANA_TOK}`, cfg)
	if err != nil {
		t.Fatal(err)
	}
	h := http.Header{}
	h.Set("Authorization", "Bearer g3cret")
	if !s.Authenticate(nil, h) {
		t.Error("Authenticate should accept the instance's own token_env value")
	}
	h.Set("Authorization", "Bearer shared")
	if s.Authenticate(nil, h) {
		t.Error("an instance token_env set should stop the shared token from being accepted")
	}
}

// TestFieldOverride: every field stays overridable — a single overridden
// field path must apply, and every other field must keep its default.
func TestFieldOverride(t *testing.T) {
	s, err := buildFor(t, `
fields:
  workload_name: labels.deployment
`, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"alerts":[{"status":"firing","fingerprint":"fp1","labels":{"alertname":"HighCPU","severity":"critical","namespace":"payments","deployment":"api"},"annotations":{"summary":"CPU is high"}}]}`
	res, err := s.Decode([]byte(body), http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 1 {
		t.Fatalf("got %d requests, want 1", len(res.Requests))
	}
	r := res.Requests[0]
	if r.Workload.Name != "api" {
		t.Errorf("overridden workload_name path not applied: %+v", r.Workload)
	}
	// Every other default field must still resolve.
	if r.Title != "HighCPU" || r.Severity != "critical" || r.Workload.Namespace != "payments" || r.Message != "CPU is high" {
		t.Errorf("overriding one field broke the others: %+v", r)
	}
}

// TestItemsOverride: overriding items (the batch path) must also work —
// not just leaf field paths.
func TestItemsOverride(t *testing.T) {
	s, err := buildFor(t, `items: data.alerts`, nil)
	if err != nil {
		t.Fatal(err)
	}
	body := `{"data":{"alerts":[{"status":"firing","fingerprint":"fp1","labels":{"alertname":"X","severity":"warning","namespace":"ns","pod":"p"},"annotations":{"summary":"m"}}]}}`
	res, err := s.Decode([]byte(body), http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Requests) != 1 || res.Requests[0].Title != "X" {
		t.Fatalf("items override not applied: %+v", res)
	}
}

func TestDisabledWithoutConfigKey(t *testing.T) {
	s, err := buildFor(t, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s != nil {
		t.Error("no sources.grafana key should leave the source disabled (nil)")
	}
}

func TestUnknownKeyRejected(t *testing.T) {
	if _, err := buildFor(t, `bogus_key: nope`, nil); err == nil {
		t.Fatal("a typo'd sources.grafana key should abort startup, not build silently")
	}
}
