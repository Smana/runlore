// SPDX-License-Identifier: Apache-2.0

package telemetry

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric/noop"
)

// TestLatencyHistogramsUseSLOBuckets asserts the seconds-scale latency histograms
// carry the explicit SLO-aligned bucket boundaries, not the OTel SDK defaults.
// The defaults are {5,10,25,50,75,100,250,...}; a le="2.5" bucket can only exist
// when the explicit view is installed, so this test fails before the buckets change.
func TestLatencyHistogramsUseSLOBuckets(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	h, shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	m := NewMetrics()
	ctx := context.Background()
	// Record one sample into each latency histogram so its bucket series materialize.
	m.ToolCallDuration.Record(ctx, 0.2)
	m.ModelRequestDuration.Record(ctx, 0.9)
	m.InvestigationDuration.Record(ctx, 1.5)
	m.IncidentResolutionSeconds.Record(ctx, 90)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	// le="2.5" is an SLO boundary present only under the explicit view; absent
	// under the SDK defaults. Checking it on every latency histogram pins the view.
	for _, series := range []string{
		"runlore_tool_call_duration_seconds_bucket",
		"runlore_model_request_duration_seconds_bucket",
		"runlore_investigation_duration_seconds_bucket",
		"runlore_incident_resolution_seconds_bucket",
	} {
		if !bucketHasBoundary(body, series, "2.5") {
			t.Errorf("%s missing SLO boundary le=\"2.5\" — buckets not SLO-aligned\n", series)
		}
	}
}

// TestScoreHistogramsUseScoreBuckets asserts the BM25-score histograms carry the
// score-scale boundaries rather than the OTel SDK defaults. The defaults start at
// 5, so a real corpus (~0.1–1.2) lands entirely in the first bucket and the
// distribution is unreadable; le="0.5" can only exist under the explicit view.
func TestScoreHistogramsUseScoreBuckets(t *testing.T) {
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })

	h, shutdown, err := Setup(context.Background())
	if err != nil {
		t.Fatalf("setup: %v", err)
	}
	defer func() { _ = shutdown(context.Background()) }()

	m := NewMetrics()
	ctx := context.Background()
	// Two samples in the real-corpus range that the SDK defaults cannot separate:
	// both fall in the default (0,5] bucket, but land in different score buckets.
	m.RecallScore.Record(ctx, 0.494)
	m.CurationDedupScore.Record(ctx, 0.9)

	req := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	body := rec.Body.String()

	for _, series := range []string{
		"runlore_recall_score_bucket",
		"runlore_curation_dedup_score_bucket",
	} {
		// A sub-1 boundary proves the view is installed.
		if !bucketHasBoundary(body, series, "0.5") {
			t.Errorf("%s missing boundary le=\"0.5\" — score buckets not installed\n", series)
		}
		// Each decision threshold must be its own boundary, so a bucket count
		// answers "how much of the corpus clears this gate?" directly.
		for _, gate := range []string{"0.1", "1", "4", "5"} {
			if !bucketHasBoundary(body, series, gate) {
				t.Errorf("%s missing decision-threshold boundary le=%q\n", series, gate)
			}
		}
	}

	// The point of the change: the two sub-1 samples must be DISTINGUISHABLE.
	// Under the SDK defaults both fall in the same (0,5] bucket. Here 0.494 is
	// <= 0.5 and 0.9 is not, so the cumulative counts differ at that boundary.
	if got := bucketCount(t, body, "runlore_recall_score_bucket", "0.5"); got != 1 {
		t.Errorf("recall_score 0.494 should be in le=\"0.5\"; got count %d", got)
	}
	if got := bucketCount(t, body, "runlore_curation_dedup_score_bucket", "0.5"); got != 0 {
		t.Errorf("curation_dedup_score 0.9 should NOT be in le=\"0.5\"; got count %d", got)
	}
	// ...and both are counted by the time the ladder reaches 1.
	if got := bucketCount(t, body, "runlore_curation_dedup_score_bucket", "1"); got != 1 {
		t.Errorf("curation_dedup_score 0.9 should be in le=\"1\"; got count %d", got)
	}
}

// bucketCount returns the cumulative count on the given histogram bucket series at
// the given le boundary. Fails the test if the line is absent. Labels are matched
// individually rather than as a literal substring: the exporter emits otel_scope_*
// labels ahead of le, so the series name and le are not adjacent in the output.
func bucketCount(t *testing.T, body, series, le string) int {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, series) || !strings.Contains(line, `le="`+le+`"`) {
			continue
		}
		fields := strings.Fields(line)
		n, err := strconv.Atoi(fields[len(fields)-1])
		if err != nil {
			t.Fatalf("%s le=%q: unparseable count in %q: %v", series, le, line, err)
		}
		return n
	}
	t.Fatalf("%s has no line for le=%q\n%s", series, le, body)
	return 0
}

// bucketHasBoundary reports whether the exposition body has a line for the given
// histogram bucket series carrying the given le boundary value.
func bucketHasBoundary(body, series, le string) bool {
	for _, line := range strings.Split(body, "\n") {
		if strings.HasPrefix(line, series) && strings.Contains(line, `le="`+le+`"`) {
			return true
		}
	}
	return false
}
