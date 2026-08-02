// SPDX-License-Identifier: Apache-2.0

package grafana

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Smana/runlore/internal/config"
	"github.com/Smana/runlore/internal/investigate"
	"github.com/Smana/runlore/internal/source"
	"github.com/Smana/runlore/internal/source/custom"
)

// grafanaDocPath is the PUBLISHED page this guard parses. Relative to this
// package directory, which is where `go test` runs.
//
// The guard lives here rather than in internal/docsguard (the repo's home for
// this drift class) because it needs this package's unexported defaults
// (defaultItems, defaultLabels, defaultMapping) to compare against. Moving it
// would mean exporting them for a test's benefit; parsing the page from here is
// the smaller cost.
const grafanaDocPath = "../../../website/content/docs/integrations/triggers/grafana.md"

// mappingRow matches one row of the published "Built-in mapping" table:
//
//	| `title` | `labels.alertname` | the alert rule's name |
//
// The header row (`| Field | Path | …`) has no backticks and does not match.
var mappingRow = regexp.MustCompile("(?m)^\\|\\s*`([a-z_]+)`\\s*\\|\\s*`([^`]+)`\\s*\\|")

// docMapping parses the mapping table out of the real grafana.md. It is
// deliberately scoped to the "## Built-in mapping" section so a table added
// elsewhere on the page can never feed the guard, and it fails loudly when it
// parses nothing — a guard that silently compares against an empty table is
// worse than no guard.
func docMapping(t *testing.T) map[string]string {
	t.Helper()
	raw, err := os.ReadFile(grafanaDocPath)
	if err != nil {
		t.Fatalf("read %s: %v", grafanaDocPath, err)
	}
	const heading = "## Built-in mapping"
	doc := string(raw)
	i := strings.Index(doc, heading)
	if i < 0 {
		t.Fatalf("%s has no %q section — the published mapping table moved or was renamed", grafanaDocPath, heading)
	}
	sec := doc[i+len(heading):]
	if j := strings.Index(sec, "\n## "); j >= 0 {
		sec = sec[:j]
	}
	rows := mappingRow.FindAllStringSubmatch(sec, -1)
	if len(rows) == 0 {
		t.Fatalf("parsed 0 rows out of %s's %q table — did the table change shape?", grafanaDocPath, heading)
	}
	out := make(map[string]string, len(rows))
	for _, m := range rows {
		out[m[1]] = m[2]
	}
	return out
}

// TestDefaultMappingMatchesDocs is the defining test: the built-in mapping
// must equal, field for field, the table PUBLISHED at
// website/content/docs/integrations/grafana.md. It parses the real markdown
// rather than a transcription of it — a transcription only pins a constant
// against a second constant, so editing the published table (say
// workload_name from labels.pod to labels.deployment) would leave the test
// green and the docs lying.
func TestDefaultMappingMatchesDocs(t *testing.T) {
	documented := docMapping(t)

	// items and labels ride in the same table but are not field paths.
	if got := documented["items"]; got != defaultItems {
		t.Errorf("items = %q, %s documents %q", defaultItems, grafanaDocPath, got)
	}
	if got := documented["labels"]; got != defaultLabels {
		t.Errorf("labels = %q, %s documents %q", defaultLabels, grafanaDocPath, got)
	}
	delete(documented, "items")
	delete(documented, "labels")

	// Both directions: a row the code does not implement, and a default the
	// page does not document, are equally a lie.
	for field, path := range documented {
		got, ok := defaultFields[field]
		if !ok {
			t.Errorf("%s documents fields.%s = %q, but defaultFields has no such field", grafanaDocPath, field, path)
			continue
		}
		if got != path {
			t.Errorf("fields.%s = %q, %s documents %q", field, got, grafanaDocPath, path)
		}
	}
	for field, path := range defaultFields {
		if _, ok := documented[field]; !ok {
			t.Errorf("defaultFields has fields.%s = %q, which %s does not document", field, path, grafanaDocPath)
		}
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
	if r0.Source != investigate.SourceGrafana || r0.Title != "HighCPU" || r0.Message != "CPU is high" {
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

// TestStartupErrorsNameTheOperatorsConfigPath: failing closed is only half the
// requirement — the message has to name a path the operator can find in their
// own file. `grafana` compiles its mapping as a synthetic custom instance
// called "grafana", so an unguarded delegation reports
// `sources.custom.instances.grafana`, which appears nowhere in a config that
// only has `sources.grafana`.
func TestStartupErrorsNameTheOperatorsConfigPath(t *testing.T) {
	cases := []struct{ name, yml string }{
		{"unknown key", `feilds: {title: labels.alertname}`},
		{"unparseable field path", `fields: {title: "x["}`},
		{"unparseable items path", `items: "x["`},
		{"unparseable labels path", `labels: "x["`},
		{"empty token_env", `token_env: GRAFANA_TOK_DEFINITELY_UNSET`},
	}
	for _, c := range cases {
		_, err := buildFor(t, c.yml, nil)
		if err == nil {
			t.Errorf("%s: want a startup error, got nil", c.name)
			continue
		}
		if !strings.Contains(err.Error(), "sources.grafana") {
			t.Errorf("%s: error %q must name sources.grafana", c.name, err)
		}
		if strings.Contains(err.Error(), "sources.custom") {
			t.Errorf("%s: error %q names sources.custom, a path the operator never wrote", c.name, err)
		}
	}
}

// TestFailClosedErrorNamesGrafana covers the actions.mode=auto message, which
// is built in custom.Build rather than parseConfig.
func TestFailClosedErrorNamesGrafana(t *testing.T) {
	cfg := &config.Config{}
	cfg.Actions.Mode = config.ActionAuto
	_, err := buildFor(t, `{}`, cfg) // no token anywhere: must fail closed
	if err == nil {
		t.Fatal("mode=auto with no token must fail closed")
	}
	if !strings.Contains(err.Error(), "sources.grafana") || strings.Contains(err.Error(), "sources.custom") {
		t.Errorf("fail-closed error %q must name sources.grafana, not sources.custom", err)
	}
}

// spyInvestigator records what the queue dispatched.
type spyInvestigator struct {
	mu   sync.Mutex
	got  []investigate.Request
	done chan struct{}
}

func (s *spyInvestigator) Investigate(_ context.Context, r investigate.Request) error {
	s.mu.Lock()
	s.got = append(s.got, r)
	s.mu.Unlock()
	s.done <- struct{}{}
	return nil
}

// TestGrafanaDoesNotCoalesceWithCustom is the reason grafana claims its own
// investigate.Source. investigate.keyOf coalesces on
// {Source, Namespace, Name, Title}: an operator running BOTH
// sources.custom.instances.datadog and sources.grafana, each firing "HighCPU"
// on payments/api-0, would have the second incident silently swallowed by the
// first if both stamped Source="custom".
//
// Both requests are enqueued BEFORE Run so they sit in the queue together —
// that is what makes the coalescing observable; enqueuing against a running
// queue could dispatch the first before the second arrives and pass either way.
func TestGrafanaDoesNotCoalesceWithCustom(t *testing.T) {
	g, err := buildFor(t, `{}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	gres, err := g.Decode([]byte(`{"alerts":[{"status":"firing","fingerprint":"gfp","labels":{"alertname":"HighCPU","severity":"critical","namespace":"payments","pod":"api-0"},"annotations":{"summary":"CPU is high"}}]}`), http.Header{})
	if err != nil {
		t.Fatal(err)
	}

	// The same incident, reported by a plain sources.custom instance.
	c, err := custom.Build(mustNode(t, `
instances:
  datadog:
    fields: {title: alertname, severity: severity, namespace: namespace, workload_name: pod}
`), &config.Config{})
	if err != nil {
		t.Fatal(err)
	}
	ch := http.Header{}
	ch.Set(source.InstanceHeader, "datadog")
	cres, err := c.Decode([]byte(`{"alertname":"HighCPU","severity":"critical","namespace":"payments","pod":"api-0"}`), ch)
	if err != nil {
		t.Fatal(err)
	}
	if len(gres.Requests) != 1 || len(cres.Requests) != 1 {
		t.Fatalf("want 1 request from each source, got %d / %d", len(gres.Requests), len(cres.Requests))
	}
	if gres.Requests[0].Source != investigate.SourceGrafana {
		t.Errorf("grafana request stamped Source=%q, want %q", gres.Requests[0].Source, investigate.SourceGrafana)
	}
	if cres.Requests[0].Source != investigate.SourceCustom {
		t.Errorf("custom request stamped Source=%q, want %q — sources.custom's behaviour must not change",
			cres.Requests[0].Source, investigate.SourceCustom)
	}

	spy := &spyInvestigator{done: make(chan struct{}, 4)}
	q := investigate.NewQueue(spy, slog.New(slog.NewTextHandler(io.Discard, nil)))
	q.Enqueue(gres.Requests[0])
	q.Enqueue(cres.Requests[0])
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go q.Run(ctx)
	for i := 0; i < 2; i++ {
		select {
		case <-spy.done:
		case <-time.After(2 * time.Second):
			t.Fatal("only one investigation dispatched: the Grafana and custom incidents coalesced into a single queue key")
		}
	}
	spy.mu.Lock()
	defer spy.mu.Unlock()
	seen := map[investigate.Source]bool{}
	for _, r := range spy.got {
		seen[r.Source] = true
	}
	if !seen[investigate.SourceGrafana] || !seen[investigate.SourceCustom] {
		t.Errorf("both sources must reach the investigator, saw %v", seen)
	}
}

// TestDecodeNilHeader: Decode/Authenticate are exported, and
// http.Header(nil).Clone() returns nil — Set on which panics. Unreachable from
// Built.Handler, but a panic in a webhook path is never acceptable.
func TestDecodeNilHeader(t *testing.T) {
	s, err := buildFor(t, `{}`, nil)
	if err != nil {
		t.Fatal(err)
	}
	res, err := s.Decode([]byte(batchBody), nil)
	if err != nil {
		t.Fatalf("Decode with a nil header: %v", err)
	}
	if len(res.Requests) != 2 {
		t.Errorf("got %d requests, want 2", len(res.Requests))
	}
	if !s.Authenticate(nil, nil) { // no token configured for this build: open
		t.Error("Authenticate with a nil header should not panic and should stay open when no token is set")
	}
}
