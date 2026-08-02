// SPDX-License-Identifier: Apache-2.0

//go:build liveverify

// Live verification against REAL Elasticsearch and OpenSearch backends.
//
// Everything else in this package tests against mocked responses, and a mock is only
// as correct as its author's understanding of the wire format — which is exactly the
// assumption this file exists to close. Build-tagged so it never runs in CI.
//
//	ES_URL=http://localhost:9200 OS_URL=http://localhost:9201 \
//	  go test -tags liveverify ./internal/logs/elasticsearch/ -run TestLive -v
package elasticsearch

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

func TestLiveBothDistributions(t *testing.T) {
	for _, tc := range []struct{ name, env string }{
		{"elasticsearch", "ES_URL"},
		{"opensearch", "OS_URL"},
	} {
		base := os.Getenv(tc.env)
		if base == "" {
			t.Fatalf("%s is unset — this test is meaningless without a real backend", tc.env)
		}
		t.Run(tc.name, func(t *testing.T) {
			c := New(base, "logs-*")
			ctx := context.Background()
			// The seeded documents are stamped around now; a wide window keeps this
			// from being a clock-skew test.
			w := providers.TimeWindow{
				Start: time.Now().Add(-24 * time.Hour),
				End:   time.Now().Add(1 * time.Hour),
			}
			const q = `kubernetes.namespace:"payments" AND log.level:"error"`

			// query_logs
			lines, err := c.Query(ctx, q, w)
			if err != nil {
				t.Fatalf("Query: %v", err)
			}
			if len(lines) == 0 {
				t.Fatal("Query returned no lines against a seeded index")
			}
			first := lines[0]
			t.Logf("query_logs: %d lines; newest = %s | %s",
				len(lines), first.Time.Format(time.RFC3339), first.Message)
			t.Logf("           fields = %v", first.Fields)
			if first.Time.Before(lines[len(lines)-1].Time) {
				t.Error("results are not newest-first")
			}
			if first.Time.IsZero() {
				t.Error("timestamp did not parse — every LogLine.Time would be zero")
			}
			// The ECS field mapping is the whole point of the defaults; if namespace
			// and pod do not survive into Fields, the model gets untethered log lines.
			if first.Fields["kubernetes.namespace"] == "" || first.Fields["kubernetes.pod.name"] == "" {
				t.Errorf("ECS mapping lost namespace/pod: %v", first.Fields)
			}

			// logs_error_summary — histogram
			buckets, err := c.Hits(ctx, q, w, time.Minute)
			if err != nil {
				t.Fatalf("Hits: %v", err)
			}
			var total int64
			for _, b := range buckets {
				total += b.Count
			}
			t.Logf("logs_error_summary histogram: %d buckets, %d hits", len(buckets), total)
			if total == 0 {
				t.Error("histogram counted nothing against a seeded index")
			}

			// logs_error_summary — top messages. ECS maps `message` as text-only, so
			// the server-side terms aggregation MUST be rejected and the client-side
			// fallback must carry it. isTextFieldAggError is the fallback's ONLY
			// trigger, and the rejection wording is NOT identical across the two
			// distributions — this assertion is the reason this file exists.
			msgs, err := c.TopMessages(ctx, q, w, 5)
			if err != nil {
				t.Fatalf("TopMessages: %v — the text-field fallback did not trigger", err)
			}
			if len(msgs) == 0 {
				t.Fatal("TopMessages returned nothing — the fallback did not carry")
			}
			t.Logf("logs_error_summary top messages (via client-side fallback): %d distinct", len(msgs))
			for _, m := range msgs {
				t.Logf("    %4d  %s", m.Count, m.Message)
			}

			// discover_log_fields
			fields, err := c.FieldNames(ctx, "", w)
			if err != nil {
				t.Fatalf("FieldNames: %v", err)
			}
			if len(fields) == 0 {
				t.Fatal("FieldNames returned nothing")
			}
			var sawTimestamp, sawNamespace bool
			for _, f := range fields {
				switch f.Name {
				case "@timestamp":
					sawTimestamp = true
				case "kubernetes.namespace":
					sawNamespace = true
				}
			}
			t.Logf("discover_log_fields: %d fields", len(fields))
			if !sawTimestamp || !sawNamespace {
				t.Errorf("expected @timestamp and kubernetes.namespace among the %d fields", len(fields))
			}
		})
	}
}
