package awsvpc

import (
	"context"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs"
	cwltypes "github.com/aws/aws-sdk-go-v2/service/cloudwatchlogs/types"

	"github.com/Smana/runlore/internal/providers"
)

// fakeCWL is an in-memory cwlAPI: it captures the last input and returns a canned
// output, so Drops is exercised with no credentials and no network.
type fakeCWL struct {
	captured *cloudwatchlogs.FilterLogEventsInput
	out      *cloudwatchlogs.FilterLogEventsOutput
	err      error
}

func (f *fakeCWL) FilterLogEvents(_ context.Context, in *cloudwatchlogs.FilterLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	f.captured = in
	return f.out, f.err
}

// event builds a FilteredLogEvent from a raw message and an epoch-ms timestamp.
func event(msg string, tsMillis int64) cwltypes.FilteredLogEvent {
	m := msg
	ts := tsMillis
	return cwltypes.FilteredLogEvent{Message: &m, Timestamp: &ts}
}

func strptr(s string) *string { return &s }

func TestDrops(t *testing.T) {
	const tsMillis = 1_690_000_059_000 // 2023-07-22T05:47:39Z
	fake := &fakeCWL{
		out: &cloudwatchlogs.FilterLogEventsOutput{
			Events: []cwltypes.FilteredLogEvent{
				event("2 123456789010 eni-abc123 10.0.1.5 10.0.2.9 49152 443 6 5 300 1690000000 1690000060 REJECT OK", tsMillis),
				event("2 123456789010 eni-def456 10.0.3.7 10.0.4.2 51000 80 6 3 180 1690000010 1690000070 REJECT OK", tsMillis),
				event("2 123456789010 eni-ghi789 10.0.5.1 10.0.6.8 53000 53 17 1 60 1690000020 1690000080 REJECT OK", tsMillis),
			},
		},
	}
	c := &Client{cwl: fake, logGroup: "vpc-flow-logs", maxEvents: 100}

	win := providers.TimeWindow{
		Start: time.Date(2023, 7, 22, 5, 0, 0, 0, time.UTC),
		End:   time.Date(2023, 7, 22, 6, 0, 0, 0, time.UTC),
	}
	got, err := c.Drops(context.Background(), providers.Selector{}, win)
	if err != nil {
		t.Fatalf("Drops returned error: %v", err)
	}

	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3", len(got))
	}

	// First parsed line.
	first := got[0]
	wantMsg := "10.0.1.5:49152 -> 10.0.2.9:443 REJECT (proto 6)"
	if first.Message != wantMsg {
		t.Errorf("Message = %q, want %q", first.Message, wantMsg)
	}
	wantFields := map[string]string{
		"action":      "REJECT",
		"source":      "10.0.1.5",
		"destination": "10.0.2.9",
		"srcport":     "49152",
		"dstport":     "443",
		"protocol":    "6",
	}
	for k, want := range wantFields {
		if got := first.Fields[k]; got != want {
			t.Errorf("Fields[%q] = %q, want %q", k, got, want)
		}
	}
	if len(first.Fields) != len(wantFields) {
		t.Errorf("Fields has %d keys, want %d: %v", len(first.Fields), len(wantFields), first.Fields)
	}
	if wantTime := time.UnixMilli(tsMillis); !first.Time.Equal(wantTime) {
		t.Errorf("Time = %v, want %v", first.Time, wantTime)
	}

	// Captured input: log group, REJECT filter pattern, and the window bounds.
	in := fake.captured
	if in == nil {
		t.Fatal("FilterLogEvents was not called")
	}
	if in.LogGroupName == nil || *in.LogGroupName != "vpc-flow-logs" {
		t.Errorf("LogGroupName = %v, want vpc-flow-logs", in.LogGroupName)
	}
	if in.FilterPattern == nil || *in.FilterPattern != rejectFilterPattern {
		t.Errorf("FilterPattern = %v, want %q", in.FilterPattern, rejectFilterPattern)
	}
	if in.StartTime == nil || *in.StartTime != win.Start.UnixMilli() {
		t.Errorf("StartTime = %v, want %d", in.StartTime, win.Start.UnixMilli())
	}
	if in.EndTime == nil || *in.EndTime != win.End.UnixMilli() {
		t.Errorf("EndTime = %v, want %d", in.EndTime, win.End.UnixMilli())
	}
	if in.Limit == nil || *in.Limit != 100 {
		t.Errorf("Limit = %v, want 100", in.Limit)
	}
}

func TestDropsEmpty(t *testing.T) {
	fake := &fakeCWL{out: &cloudwatchlogs.FilterLogEventsOutput{}}
	c := &Client{cwl: fake, logGroup: "vpc-flow-logs", maxEvents: 100}

	got, err := c.Drops(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops returned error: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("got %d lines, want 0", len(got))
	}
	// No window set: time bounds must be left nil.
	if fake.captured.StartTime != nil || fake.captured.EndTime != nil {
		t.Errorf("expected nil Start/End for zero window, got Start=%v End=%v", fake.captured.StartTime, fake.captured.EndTime)
	}
}

func TestDropsSkipsMalformed(t *testing.T) {
	fake := &fakeCWL{
		out: &cloudwatchlogs.FilterLogEventsOutput{
			Events: []cwltypes.FilteredLogEvent{
				event("too few fields here", 0),
				event("2 123456789010 eni-abc123 10.0.1.5 10.0.2.9 49152 443 6 5 300 1690000000 1690000060 REJECT OK", 1_690_000_059_000),
			},
		},
	}
	c := &Client{cwl: fake, logGroup: "vpc-flow-logs", maxEvents: 100}

	got, err := c.Drops(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops returned error: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1 (malformed skipped)", len(got))
	}
}

func TestDropsPaginates(t *testing.T) {
	page1 := &cloudwatchlogs.FilterLogEventsOutput{
		Events: []cwltypes.FilteredLogEvent{
			event("2 123456789010 eni-abc123 10.0.1.5 10.0.2.9 49152 443 6 5 300 1690000000 1690000060 REJECT OK", 1_690_000_059_000),
		},
		NextToken: strptr("page-2"),
	}
	page2 := &cloudwatchlogs.FilterLogEventsOutput{
		Events: []cwltypes.FilteredLogEvent{
			event("2 123456789010 eni-def456 10.0.3.7 10.0.4.2 51000 80 6 3 180 1690000010 1690000070 REJECT OK", 1_690_000_069_000),
		},
	}
	pager := &pagedCWL{pages: []*cloudwatchlogs.FilterLogEventsOutput{page1, page2}}
	c := &Client{cwl: pager, logGroup: "vpc-flow-logs", maxEvents: 100}

	got, err := c.Drops(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops returned error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d lines across pages, want 2", len(got))
	}
	if pager.lastToken == nil || *pager.lastToken != "page-2" {
		t.Errorf("second call NextToken = %v, want page-2", pager.lastToken)
	}
}

// pagedCWL returns its pages in order, capturing the NextToken on each call.
type pagedCWL struct {
	pages     []*cloudwatchlogs.FilterLogEventsOutput
	i         int
	lastToken *string
}

func (p *pagedCWL) FilterLogEvents(_ context.Context, in *cloudwatchlogs.FilterLogEventsInput, _ ...func(*cloudwatchlogs.Options)) (*cloudwatchlogs.FilterLogEventsOutput, error) {
	p.lastToken = in.NextToken
	out := p.pages[p.i]
	p.i++
	return out, nil
}
