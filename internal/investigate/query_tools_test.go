package investigate

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

type fakeMetrics struct{ samples providers.Samples }

func (f fakeMetrics) Query(context.Context, string, time.Time) (providers.Samples, error) {
	return f.samples, nil
}
func (f fakeMetrics) QueryRange(context.Context, string, providers.TimeWindow, time.Duration) (providers.Matrix, error) {
	return nil, nil
}

func TestQueryMetricsTool(t *testing.T) {
	tool := QueryMetricsTool{Metrics: fakeMetrics{samples: providers.Samples{
		{Metric: map[string]string{"__name__": "up", "job": "api"}, Value: 0},
		{Metric: map[string]string{"__name__": "up", "job": "db"}, Value: 1},
	}}}
	if tool.Name() != "query_metrics" {
		t.Fatalf("name=%q", tool.Name())
	}
	out, err := tool.Call(context.Background(), `{"query":"up"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, `up{job="api"} = 0`) || !strings.Contains(out, `up{job="db"} = 1`) {
		t.Fatalf("unexpected output:\n%s", out)
	}
}

type fakeLogs struct{ lines providers.LogResult }

func (f fakeLogs) Query(context.Context, string, providers.TimeWindow) (providers.LogResult, error) {
	return f.lines, nil
}

func TestQueryLogsTool(t *testing.T) {
	tool := QueryLogsTool{Logs: fakeLogs{lines: providers.LogResult{
		{Time: time.Unix(1700000000, 0).UTC(), Message: "db connection refused"},
	}}}
	if tool.Name() != "query_logs" {
		t.Fatalf("name=%q", tool.Name())
	}
	out, err := tool.Call(context.Background(), `{"query":"error","since_minutes":30}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}
	if !strings.Contains(out, "db connection refused") {
		t.Fatalf("unexpected output:\n%s", out)
	}
}
