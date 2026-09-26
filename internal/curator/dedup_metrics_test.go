// SPDX-License-Identifier: Apache-2.0

package curator

import (
	"context"
	"errors"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/metric/noop"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/Smana/runlore/internal/catalog"
	"github.com/Smana/runlore/internal/providers"
	"github.com/Smana/runlore/internal/telemetry"
)

// meteredInstruments installs a real SDK meter provider behind a manual reader and
// returns the instrument set bound to it plus the reader. Global provider: one test
// at a time (no t.Parallel), and the cleanup restores the no-op provider.
func meteredInstruments(t *testing.T) (*telemetry.Metrics, *sdkmetric.ManualReader) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	return telemetry.NewMetrics(), reader
}

// decideSeries reads back the decider's request count for one result and its latency
// sample count, off the two series the reranker's decider calls also land on.
func decideSeries(t *testing.T, reader *sdkmetric.ManualReader, result string) (requests int64, latency uint64) {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect metrics: %v", err)
	}
	byResult := attribute.NewSet(attribute.String("provider", "decide"), attribute.String("result", result))
	byProvider := attribute.NewSet(attribute.String("provider", "decide"))
	for _, sm := range rm.ScopeMetrics {
		for _, md := range sm.Metrics {
			switch md.Name {
			case "runlore_model_requests_total":
				for _, dp := range md.Data.(metricdata.Sum[int64]).DataPoints {
					if dp.Attributes.Equals(&byResult) {
						requests += dp.Value
					}
				}
			case "runlore_model_request_duration_seconds":
				for _, dp := range md.Data.(metricdata.Histogram[float64]).DataPoints {
					if dp.Attributes.Equals(&byProvider) {
						latency += dp.Count
					}
				}
			}
		}
	}
	return requests, latency
}

// TestDedupDeciderCallsAreMetered: the curator's decider calls land on the same
// request and latency series as the reranker's (see sameIncidentPattern), and a 200
// that carries no answer counts as an error, since curation falls back exactly as on
// one.
func TestDedupDeciderCallsAreMetered(t *testing.T) {
	hit := catalog.ScoredEntry{Entry: catalog.Entry{Path: "incidents/x.md", Title: "x"}, Score: 0.49}
	for _, tc := range []struct {
		name   string
		dec    providers.Decider
		result string
	}{
		{"a successful call counts as ok", fakeDecider{noul: 0.2}, "ok"},
		{"a failed call counts as error", fakeDecider{err: errors.New("401")}, "error"},
		{"a 200 with no answer counts as error", fakeDecider{noAnswer: true}, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m, reader := meteredInstruments(t)
			c := newCurator(&fakeForge{}, multiScored{hits: []catalog.ScoredEntry{hit}})
			c.Decider, c.Metrics = tc.dec, m
			c.DedupSkipAbove, c.DedupAnnotateAbove = 0.85, 0.60
			if _, err := c.Curate(context.Background(), goodFinding()); err != nil {
				t.Fatalf("Curate: %v", err)
			}
			if requests, latency := decideSeries(t, reader, tc.result); requests != 1 || latency != 1 {
				t.Fatalf("want one model_requests_total{provider=decide,result=%s} and one latency sample, got %d and %d",
					tc.result, requests, latency)
			}
		})
	}
}

// TestDedupDeciderSkipsNoiseFloorHits: below the dedup floor the decider is not asked
// and the BM25 gate decides, as it always did (see Curate).
func TestDedupDeciderSkipsNoiseFloorHits(t *testing.T) {
	noise := catalog.ScoredEntry{Entry: catalog.Entry{Path: "incidents/unrelated.md", Title: "unrelated"}, Score: relatedFloor / 4}
	dec := &recordingDecider{noul: 0.99} // would be a confident duplicate, if asked
	f := &fakeForge{}
	c := newCurator(f, multiScored{hits: []catalog.ScoredEntry{noise}})
	c.Decider = dec

	if _, err := c.Curate(context.Background(), goodFinding()); err != nil {
		t.Fatalf("Curate: %v", err)
	}
	if dec.state != "" {
		t.Fatalf("the decider was asked about a noise-floor hit (score %v):\n%s", noise.Score, dec.state)
	}
	if f.openedPR == nil {
		t.Fatal("a noise-floor hit is not a duplicate: the finding must file")
	}
}
