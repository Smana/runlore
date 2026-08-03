// SPDX-License-Identifier: Apache-2.0

package docsguard

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/metric/noop"

	"github.com/Smana/runlore/internal/telemetry"
)

// dashboardPath and observabilityDoc are the two published surfaces that quote
// RunLore metric names back at operators. Both are hand-maintained; neither is
// compiled, imported, or executed by anything, so a rename in internal/telemetry
// leaves them silently pointing at series the agent no longer emits.
const (
	dashboardPath      = "deploy/observability/grafana/runlore.json"
	observabilityDoc   = "website/content/docs/operations/observability.md"
	metricNamePattern  = `runlore_[a-z0-9_]+`
	histogramSuffixBkt = "_bucket"
)

var metricNameRE = regexp.MustCompile(metricNamePattern)

// TestPublishedMetricNamesExist pins every `runlore_*` series the Grafana dashboard
// queries and the observability docs tabulate to a name the agent can actually emit.
//
// This is the guard the recall_tokens_saved_total → recall_tokens_spent_total rename
// needed and did not have. That rename was carried out by hand-grepping the tree, and
// the grep missed a reference: a chart-template comment naming the retired series
// survived into the rendered manifest. Nothing failed, because nothing checks these
// files against the instrument set — the dashboard is JSON, the doc table is Markdown,
// and a PromQL selector for a series that does not exist is not an error anywhere. It
// renders an empty panel, which looks exactly like "no incidents yet".
//
// The source of truth is the REAL exporter output, not a parse of metrics.go: this
// records on every instrument in the set (found by reflection, so a new instrument is
// covered the day it is added) and scrapes the same Prometheus handler /metrics serves.
// What comes back is precisely what a dashboard can query — including the _bucket/_sum/
// _count expansions the OTel Prometheus exporter derives from a histogram, which a
// source parse would have to reimplement and get wrong.
func TestPublishedMetricNamesExist(t *testing.T) {
	emitted := emittedSeries(t)

	for _, tc := range []struct {
		name  string
		path  string
		found func(t *testing.T, body []byte) map[string][]string
	}{
		{name: "grafana_dashboard", path: dashboardPath, found: dashboardMetricRefs},
		{name: "observability_docs", path: observabilityDoc, found: markdownMetricRefs},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, err := os.ReadFile(filepath.Join(repoRoot(t), tc.path))
			if err != nil {
				t.Fatalf("read %s: %v", tc.path, err)
			}
			refs := tc.found(t, body)
			if len(refs) == 0 {
				t.Fatalf("no runlore_* metric names found in %s — the file moved or its shape changed, and this guard is inert", tc.path)
			}
			for _, name := range sortedKeys(refs) {
				if seriesExists(emitted, name) {
					continue
				}
				t.Errorf("%s references %q, which no instrument emits.\n"+
					"  seen in: %s\n"+
					"  Either the metric was renamed in internal/telemetry/metrics.go and this file was "+
					"not updated, or the name is a typo. A panel querying a non-existent series renders "+
					"empty, which is indistinguishable from a healthy idle agent.",
					tc.path, name, strings.Join(refs[name], ", "))
			}
		})
	}
}

// seriesExists accepts either an exact exported series or a histogram FAMILY name.
// Prometheus never exposes a bare histogram name — only name_bucket/_sum/_count — but
// the docs table names the family (`runlore_investigation_input_tokens`), which is the
// conventional way to document one and is what an operator types to find it.
func seriesExists(emitted map[string]bool, name string) bool {
	return emitted[name] || emitted[name+histogramSuffixBkt]
}

// emittedSeries records on every instrument in the set and returns the series names the
// Prometheus exporter actually publishes. Instruments are discovered by reflecting over
// telemetry.Metrics rather than listed here: a hand-written list is a second thing to
// maintain, and it would rot in exactly the direction this guard exists to catch — a new
// metric added, never listed, so the docs can omit or misname it with impunity.
func emittedSeries(t *testing.T) map[string]bool {
	t.Helper()
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	handler, shutdown, err := telemetry.Setup(context.Background())
	if err != nil {
		t.Fatalf("telemetry setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	ctx := context.Background()
	m := telemetry.NewMetrics()
	v := reflect.ValueOf(m).Elem()
	for i := 0; i < v.NumField(); i++ {
		// The SDK backs counters and histograms with the SAME concrete type, whose Add and
		// Record are literally the same call — so an instrument satisfies both interfaces
		// regardless of how it was constructed, and which case matches here is arbitrary.
		// Only the int64/float64 split is real. What this switch genuinely rejects is an
		// instrument offering NEITHER method: an async/observable gauge, which is never
		// recorded synchronously and so would silently contribute nothing to the scrape.
		switch inst := v.Field(i).Interface().(type) {
		case metric.Int64Counter:
			inst.Add(ctx, 1)
		case metric.Float64Histogram:
			inst.Record(ctx, 1)
		default:
			t.Fatalf("telemetry.Metrics.%s is a %T, which cannot be recorded synchronously — "+
				"it contributes no series to the scrape, so every doc reference to it would pass "+
				"this guard vacuously. Emit it explicitly before the scrape below.",
				v.Type().Field(i).Name, v.Field(i).Interface())
		}
	}
	// The async gauges live outside the struct (registered at startup, not constructed).
	if err := telemetry.RegisterRuntimeGauges("0.0.0-test", func() bool { return true }); err != nil {
		t.Fatalf("register runtime gauges: %v", err)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("/metrics returned %d", rec.Code)
	}

	emitted := map[string]bool{}
	families := map[string]bool{}
	for _, line := range strings.Split(rec.Body.String(), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		name := line
		if i := strings.IndexAny(name, "{ "); i >= 0 {
			name = name[:i]
		}
		emitted[name] = true
		if strings.HasPrefix(name, "runlore_") {
			families[strings.TrimSuffix(strings.TrimSuffix(strings.TrimSuffix(name, "_bucket"), "_sum"), "_count")] = true
		}
	}

	// Completeness. The loop above is only useful if every instrument it touched actually
	// reached the scrape; if one silently did not, its name is absent from `emitted` and
	// every doc reference to it fails as "no instrument emits this" — a confusing false
	// alarm — or, worse, a future instrument records under a name nothing checks. One
	// family per struct field, plus the two async gauges, is the whole contract.
	if want := v.NumField() + 2; len(families) != want {
		t.Fatalf("scraped %d runlore_* metric families, want %d (%d telemetry.Metrics fields + build_info + leader) — "+
			"an instrument did not reach /metrics, or a new one appeared outside the struct.\nscraped: %v",
			len(families), want, v.NumField(), sortedSet(families))
	}
	return emitted
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// dashboardMetricRefs pulls every runlore_* name out of the dashboard's PromQL. It walks
// the decoded JSON for "expr" keys rather than regexing the raw file, so a metric name
// that appears only in a panel title or description is not mistaken for a query.
func dashboardMetricRefs(t *testing.T, body []byte) map[string][]string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("parse dashboard JSON: %v", err)
	}
	refs := map[string][]string{}
	var walk func(node any, where string)
	walk = func(node any, where string) {
		switch n := node.(type) {
		case map[string]any:
			title, _ := n["title"].(string)
			if title != "" {
				where = title
			}
			for k, val := range n {
				if k == "expr" {
					if expr, ok := val.(string); ok {
						for _, name := range metricNameRE.FindAllString(expr, -1) {
							refs[name] = appendUnique(refs[name], "panel "+where)
						}
					}
					continue
				}
				walk(val, where)
			}
		case []any:
			for _, item := range n {
				walk(item, where)
			}
		}
	}
	walk(doc, "(dashboard root)")
	return refs
}

// markdownMetricRefs pulls every runlore_* name out of the whole doc — the metric table
// AND the PromQL recipes in prose. The recipes are the part most worth pinning: a metric
// table entry that goes stale is a wrong label, but a recipe that goes stale is an
// operator computing a number they will believe.
func markdownMetricRefs(_ *testing.T, body []byte) map[string][]string {
	refs := map[string][]string{}
	for i, line := range strings.Split(string(body), "\n") {
		for _, name := range metricNameRE.FindAllString(line, -1) {
			refs[name] = appendUnique(refs[name], "line "+strconv.Itoa(i+1))
		}
	}
	return refs
}

func appendUnique(in []string, s string) []string {
	for _, existing := range in {
		if existing == s {
			return in
		}
	}
	return append(in, s)
}

func sortedKeys(m map[string][]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
