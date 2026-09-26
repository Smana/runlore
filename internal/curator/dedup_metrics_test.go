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
// returns the instrument set bound to it, plus a reader that sums one int64 counter's
// data points carrying every attribute in want. Global provider: one test at a time.
func meteredInstruments(t *testing.T) (*telemetry.Metrics, func(series string, want ...attribute.KeyValue) int64) {
	t.Helper()
	reader := sdkmetric.NewManualReader()
	otel.SetMeterProvider(sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader)))
	t.Cleanup(func() { otel.SetMeterProvider(noop.NewMeterProvider()) })
	return telemetry.NewMetrics(), func(series string, want ...attribute.KeyValue) int64 {
		var rm metricdata.ResourceMetrics
		if err := reader.Collect(context.Background(), &rm); err != nil {
			t.Fatalf("collect metrics: %v", err)
		}
		var total int64
		for _, sm := range rm.ScopeMetrics {
			for _, md := range sm.Metrics {
				if md.Name != series {
					continue
				}
				sum, ok := md.Data.(metricdata.Sum[int64])
				if !ok {
					t.Fatalf("series %q is not an int64 sum (%T)", series, md.Data)
				}
			points:
				for _, dp := range sum.DataPoints {
					for _, kv := range want {
						if v, ok := dp.Attributes.Value(kv.Key); !ok || v != kv.Value {
							continue points
						}
					}
					total += dp.Value
				}
			}
		}
		return total
	}
}

// TestDedupDeciderCallsAreMetered: the reranker's decider calls count on
// runlore_model_requests_total{provider="decide"} with their result, and the
// curator's did not — so a decider returning 401 on every curation fell back to BM25
// behind a Warn line while the series the observability page points operators at
// stayed flat. Both consumers now record through one helper.
func TestDedupDeciderCallsAreMetered(t *testing.T) {
	m, sum := meteredInstruments(t)
	hit := catalog.ScoredEntry{Entry: catalog.Entry{Path: "incidents/x.md", Title: "x"}, Score: 0.49}
	for _, tc := range []struct {
		name   string
		dec    providers.Decider
		result string
	}{
		{"a successful call counts as ok", fakeDecider{noul: 0.2}, "ok"},
		{"a failed call counts as error", fakeDecider{err: errors.New("401")}, "error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newCurator(&fakeForge{}, multiScored{hits: []catalog.ScoredEntry{hit}})
			c.Decider, c.Metrics = tc.dec, m
			c.DedupSkipAbove, c.DedupAnnotateAbove = 0.85, 0.60
			before := sum("runlore_model_requests_total",
				attribute.String("provider", "decide"), attribute.String("result", tc.result))
			if _, err := c.Curate(context.Background(), goodFinding()); err != nil {
				t.Fatalf("Curate: %v", err)
			}
			after := sum("runlore_model_requests_total",
				attribute.String("provider", "decide"), attribute.String("result", tc.result))
			if after-before != 1 {
				t.Fatalf("want one model_requests_total{provider=decide,result=%s}, got %d", tc.result, after-before)
			}
		})
	}
}

// TestDedupDeciderSkipsNoiseFloorHits: the decider is a paid third-party call that
// also carries the finding's text off-host, and the curator made it on every curation
// that had ANY BM25 hit — a 200-entry catalog returns some hit at 0.02 for every
// query. The reranker guards its call with rerank_min_score; the curator's guard is
// the floor relatedEntries already applies, below which a hit is not even shown to
// the reviewer as related. Below it, the BM25 gate decides as it always did.
func TestDedupDeciderSkipsNoiseFloorHits(t *testing.T) {
	noise := catalog.ScoredEntry{Entry: catalog.Entry{Path: "incidents/unrelated.md", Title: "unrelated"}, Score: relatedFloor / 4}
	dec := &recordingDecider{noul: 0.99} // would be a confident duplicate, if asked
	f := &fakeForge{}
	c := newCurator(f, multiScored{hits: []catalog.ScoredEntry{noise}})
	c.Decider = dec
	c.DedupSkipAbove, c.DedupAnnotateAbove = 0.85, 0.60

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
