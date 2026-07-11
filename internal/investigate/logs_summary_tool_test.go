// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// fakeLogStats implements LogsProvider + LogStats.
type fakeLogStats struct {
	buckets  []providers.Bucket
	msgs     []providers.MsgCount
	gotQuery string
}

func (f *fakeLogStats) Query(context.Context, string, providers.TimeWindow) (providers.LogResult, error) {
	return nil, nil
}
func (f *fakeLogStats) Hits(_ context.Context, query string, _ providers.TimeWindow, _ time.Duration) ([]providers.Bucket, error) {
	f.gotQuery = query
	return f.buckets, nil
}
func (f *fakeLogStats) TopMessages(context.Context, string, providers.TimeWindow, int) ([]providers.MsgCount, error) {
	return f.msgs, nil
}

func TestLogsErrorSummaryTool(t *testing.T) {
	base := time.Date(2024, 1, 1, 10, 0, 0, 0, time.UTC)
	f := &fakeLogStats{
		// A flat ~3/5m baseline for 40m, then a 412 spike — the run compressor should
		// collapse the baseline into one run and flag the jump SPIKE.
		buckets: []providers.Bucket{
			{Time: base, Level: "error", Count: 3},
			{Time: base.Add(5 * time.Minute), Level: "error", Count: 4},
			{Time: base.Add(10 * time.Minute), Level: "error", Count: 3},
			{Time: base.Add(15 * time.Minute), Level: "error", Count: 412},
		},
		msgs: []providers.MsgCount{
			{Message: "connection refused to db:5432", Count: 388, First: base, Last: base.Add(15 * time.Minute)},
			{Message: "timeout", Count: 24, First: base, Last: base.Add(10 * time.Minute)},
		},
	}
	tool := LogsErrorSummaryTool{Logs: f}
	if tool.Name() != "logs_error_summary" {
		t.Fatalf("name=%q", tool.Name())
	}
	out, err := tool.Call(context.Background(), `{"namespace":"apps","container":"harbor-core"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	// Default level=error must be woven into the built query.
	if !strings.Contains(f.gotQuery, "log.level:error") {
		t.Fatalf("query missing error filter: %q", f.gotQuery)
	}
	// Histogram: a SPIKE flag and the /5m step label.
	if !strings.Contains(out, "SPIKE") {
		t.Fatalf("missing SPIKE flag:\n%s", out)
	}
	if !strings.Contains(out, "/5m") {
		t.Fatalf("missing step label:\n%s", out)
	}
	if !strings.Contains(out, "412/5m") {
		t.Fatalf("missing spike bucket:\n%s", out)
	}
	// Top messages with counts + span.
	if !strings.Contains(out, "×388") || !strings.Contains(out, "connection refused to db:5432") {
		t.Fatalf("missing top message:\n%s", out)
	}
}

func TestLogsErrorSummaryToolNoCapability(t *testing.T) {
	tool := LogsErrorSummaryTool{Logs: fakeLogsNoCaps{}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call must not error without LogStats: %v", err)
	}
	if !strings.Contains(out, "logs_error_summary is unavailable") {
		t.Fatalf("want graceful fallback, got:\n%s", out)
	}
}

func TestLogsErrorSummaryToolEmpty(t *testing.T) {
	tool := LogsErrorSummaryTool{Logs: &fakeLogStats{}}
	out, err := tool.Call(context.Background(), `{"namespace":"apps"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "no log lines matched") {
		t.Fatalf("want empty-result note, got:\n%s", out)
	}
}

func TestSimilarRuns(t *testing.T) {
	cases := []struct {
		a, c int64
		want bool
	}{
		{3, 4, true},    // within 2x baseline noise
		{3, 6, true},    // exactly 2x
		{3, 7, false},   // >2x breaks
		{0, 2, true},    // 0→small noise stays
		{0, 400, false}, // 0→flood breaks
		{100, 412, false},
	}
	for _, tc := range cases {
		if got := similar(tc.a, tc.c); got != tc.want {
			t.Errorf("similar(%d,%d)=%v, want %v", tc.a, tc.c, got, tc.want)
		}
	}
}
