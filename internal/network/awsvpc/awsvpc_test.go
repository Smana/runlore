// SPDX-License-Identifier: Apache-2.0

package awsvpc

import (
	"context"
	"strings"
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
	c := &Client{cwl: fake, logGroup: "vpc-flow-logs", maxEvents: 100, fieldIndex: nil}

	win := providers.TimeWindow{
		Start: time.Date(2023, 7, 22, 5, 0, 0, 0, time.UTC),
		End:   time.Date(2023, 7, 22, 6, 0, 0, 0, time.UTC),
	}
	got, err := c.Drops(context.Background(), providers.Selector{}, win)
	if err != nil {
		t.Fatalf("Drops returned error: %v", err)
	}

	// got[0] is the scoping note; got[1..3] are the parsed flows.
	if len(got) != 4 {
		t.Fatalf("got %d lines, want 4 (1 note + 3 flows)", len(got))
	}

	// First parsed line (index 1 — after the scoping note).
	first := got[1]
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
	// Even when there are no flow events the scoping note is always present.
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1 (scoping note only)", len(got))
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
	// got[0] is the scoping note; got[1] is the one valid flow (malformed skipped).
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2 (1 note + 1 valid flow, malformed skipped)", len(got))
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
	// got[0] is the scoping note; got[1..2] are flows from pages 1 and 2.
	if len(got) != 3 {
		t.Fatalf("got %d lines across pages, want 3 (1 note + 2 flows)", len(got))
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

// TestDropsScopingNoteFirst asserts that:
//   - The scoping note is always the first entry (index 0) in the result.
//   - The note message contains "NOT scoped to".
//   - When the Selector carries a namespace and/or name, they appear in the note.
func TestDropsScopingNoteFirst(t *testing.T) {
	singleEvent := &cloudwatchlogs.FilterLogEventsOutput{
		Events: []cwltypes.FilteredLogEvent{
			event("2 123456789010 eni-abc123 10.0.1.5 10.0.2.9 49152 443 6 5 300 1690000000 1690000060 REJECT OK", 1_690_000_059_000),
		},
	}

	tests := []struct {
		name      string
		sel       providers.Selector
		wantScope string // substring expected in the note message
	}{
		{
			name:      "empty selector shows placeholder",
			sel:       providers.Selector{},
			wantScope: "<namespace>/<pod>",
		},
		{
			name:      "namespace only",
			sel:       providers.Selector{Namespace: "production"},
			wantScope: "production/<pod>",
		},
		{
			name:      "namespace and name",
			sel:       providers.Selector{Namespace: "production", Name: "api-server"},
			wantScope: "production/api-server",
		},
		{
			name:      "name only",
			sel:       providers.Selector{Name: "api-server"},
			wantScope: "<namespace>/api-server",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeCWL{out: singleEvent}
			c := &Client{cwl: fake, logGroup: "vpc-flow-logs", maxEvents: 100}

			got, err := c.Drops(context.Background(), tc.sel, providers.TimeWindow{})
			if err != nil {
				t.Fatalf("Drops: %v", err)
			}
			if len(got) == 0 {
				t.Fatal("Drops returned empty result, want at least the scoping note")
			}
			note := got[0]
			if !strings.Contains(note.Message, "NOT scoped to") {
				t.Errorf("note missing 'NOT scoped to': %q", note.Message)
			}
			if !strings.Contains(note.Message, tc.wantScope) {
				t.Errorf("note does not contain selector %q: %q", tc.wantScope, note.Message)
			}
			// The note must carry no Time or Fields so it cannot be mistaken for a
			// real flow record.
			if !note.Time.IsZero() {
				t.Errorf("note Time = %v, want zero", note.Time)
			}
			if len(note.Fields) != 0 {
				t.Errorf("note Fields = %v, want empty", note.Fields)
			}
		})
	}
}

// TestDropsScopingNoteEmptyResult asserts the scoping note is present even when
// the CloudWatch query returns no events (empty window / no REJECTs).
func TestDropsScopingNoteEmptyResult(t *testing.T) {
	fake := &fakeCWL{out: &cloudwatchlogs.FilterLogEventsOutput{}}
	c := &Client{cwl: fake, logGroup: "vpc-flow-logs", maxEvents: 100}
	sel := providers.Selector{Namespace: "staging", Name: "worker"}

	got, err := c.Drops(context.Background(), sel, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1 (scoping note only)", len(got))
	}
	if !strings.Contains(got[0].Message, "staging/worker") {
		t.Errorf("note does not contain selector 'staging/worker': %q", got[0].Message)
	}
}

// TestDropsTruncationSentinel asserts that a truncation sentinel is appended when
// the cap binds with more events remaining in the page (N1).
func TestDropsTruncationSentinel(t *testing.T) {
	// Three events on a single page, but cap = 2: the third is "more" on the page.
	fake := &fakeCWL{
		out: &cloudwatchlogs.FilterLogEventsOutput{
			Events: []cwltypes.FilteredLogEvent{
				event("2 123456789010 eni-abc123 10.0.1.5 10.0.2.9 49152 443 6 5 300 1690000000 1690000060 REJECT OK", 1_690_000_059_000),
				event("2 123456789010 eni-def456 10.0.3.7 10.0.4.2 51000 80 6 3 180 1690000010 1690000070 REJECT OK", 1_690_000_069_000),
				event("2 123456789010 eni-ghi789 10.0.5.1 10.0.6.8 53000 53 17 1 60 1690000020 1690000080 REJECT OK", 1_690_000_079_000),
			},
		},
	}
	c := &Client{cwl: fake, logGroup: "vpc-flow-logs", maxEvents: 2}

	got, err := c.Drops(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops: %v", err)
	}
	// want: 1 scoping note + 2 flows + 1 truncation sentinel = 4 total
	if len(got) != 4 {
		t.Fatalf("got %d lines, want 4 (1 note + 2 flows + 1 sentinel)", len(got))
	}
	last := got[len(got)-1]
	if !strings.Contains(last.Message, "results truncated at") {
		t.Errorf("last line is not the truncation sentinel: %q", last.Message)
	}
	// Sentinel must carry no Time or Fields.
	if !last.Time.IsZero() {
		t.Errorf("sentinel Time = %v, want zero", last.Time)
	}
	if len(last.Fields) != 0 {
		t.Errorf("sentinel Fields = %v, want empty", last.Fields)
	}
}

// TestDropsTruncationSentinelNextToken asserts that the truncation sentinel is
// appended when the cap binds and there is a next page token (more pages remain).
func TestDropsTruncationSentinelNextToken(t *testing.T) {
	// Single event on page 1 but cap = 1 and a NextToken signals more pages.
	fake := &fakeCWL{
		out: &cloudwatchlogs.FilterLogEventsOutput{
			Events: []cwltypes.FilteredLogEvent{
				event("2 123456789010 eni-abc123 10.0.1.5 10.0.2.9 49152 443 6 5 300 1690000000 1690000060 REJECT OK", 1_690_000_059_000),
			},
			NextToken: strptr("page-2"),
		},
	}
	c := &Client{cwl: fake, logGroup: "vpc-flow-logs", maxEvents: 1}

	got, err := c.Drops(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops: %v", err)
	}
	// want: 1 scoping note + 1 flow + 1 sentinel = 3 total
	if len(got) != 3 {
		t.Fatalf("got %d lines, want 3 (1 note + 1 flow + 1 sentinel)", len(got))
	}
	if !strings.Contains(got[len(got)-1].Message, "results truncated at") {
		t.Errorf("last line is not the truncation sentinel: %q", got[len(got)-1].Message)
	}
}

// TestDropsNoSentinelWhenExact asserts that NO sentinel is appended when the
// number of events exactly equals the cap with no next-page token (nothing more).
func TestDropsNoSentinelWhenExact(t *testing.T) {
	fake := &fakeCWL{
		out: &cloudwatchlogs.FilterLogEventsOutput{
			Events: []cwltypes.FilteredLogEvent{
				event("2 123456789010 eni-abc123 10.0.1.5 10.0.2.9 49152 443 6 5 300 1690000000 1690000060 REJECT OK", 1_690_000_059_000),
			},
		},
	}
	c := &Client{cwl: fake, logGroup: "vpc-flow-logs", maxEvents: 1}

	got, err := c.Drops(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops: %v", err)
	}
	// want: 1 scoping note + 1 flow (no sentinel because nothing more)
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2 (1 note + 1 flow, no sentinel)", len(got))
	}
	for _, l := range got {
		if strings.Contains(l.Message, "results truncated at") {
			t.Errorf("unexpected truncation sentinel when cap exactly met: %q", l.Message)
		}
	}
}

// TestDropsCustomFieldLayout asserts that parseFlowLine uses a custom field-index
// map when one is configured, correctly extracting fields from a non-v2 layout (N2).
func TestDropsCustomFieldLayout(t *testing.T) {
	// Custom layout: version(0) srcaddr(1) dstaddr(2) srcport(3) dstport(4) protocol(5) action(6)
	customIdx := map[string]int{
		"srcaddr":  1,
		"dstaddr":  2,
		"srcport":  3,
		"dstport":  4,
		"protocol": 5,
	}
	fake := &fakeCWL{
		out: &cloudwatchlogs.FilterLogEventsOutput{
			Events: []cwltypes.FilteredLogEvent{
				event("2 192.168.1.1 10.10.10.10 8080 9090 17 REJECT", 1_690_000_059_000),
			},
		},
	}
	c := &Client{cwl: fake, logGroup: "custom-group", maxEvents: 100, fieldIndex: customIdx}

	got, err := c.Drops(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops: %v", err)
	}
	// got[0] is the scoping note; got[1] is the parsed flow.
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2 (1 note + 1 flow)", len(got))
	}
	flow := got[1]
	wantMsg := "192.168.1.1:8080 -> 10.10.10.10:9090 REJECT (proto 17)"
	if flow.Message != wantMsg {
		t.Errorf("Message = %q, want %q", flow.Message, wantMsg)
	}
	if flow.Fields["source"] != "192.168.1.1" {
		t.Errorf("source = %q, want 192.168.1.1", flow.Fields["source"])
	}
	if flow.Fields["destination"] != "10.10.10.10" {
		t.Errorf("destination = %q, want 10.10.10.10", flow.Fields["destination"])
	}
	if flow.Fields["srcport"] != "8080" {
		t.Errorf("srcport = %q, want 8080", flow.Fields["srcport"])
	}
	if flow.Fields["dstport"] != "9090" {
		t.Errorf("dstport = %q, want 9090", flow.Fields["dstport"])
	}
	if flow.Fields["protocol"] != "17" {
		t.Errorf("protocol = %q, want 17", flow.Fields["protocol"])
	}
}

// TestDropsCustomFieldLayoutMissingField asserts that parseFlowLine rejects a
// record when a required field index from the custom map is out of bounds.
func TestDropsCustomFieldLayoutMissingField(t *testing.T) {
	// Custom layout requires srcaddr at column 10, but the record is too short.
	customIdx := map[string]int{
		"srcaddr":  10, // out of bounds for a short record
		"dstaddr":  2,
		"srcport":  3,
		"dstport":  4,
		"protocol": 5,
	}
	fake := &fakeCWL{
		out: &cloudwatchlogs.FilterLogEventsOutput{
			Events: []cwltypes.FilteredLogEvent{
				event("2 192.168.1.1 10.10.10.10 8080 9090 17 REJECT", 1_690_000_059_000),
			},
		},
	}
	c := &Client{cwl: fake, logGroup: "custom-group", maxEvents: 100, fieldIndex: customIdx}

	got, err := c.Drops(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops: %v", err)
	}
	// Malformed record skipped: only the scoping note.
	if len(got) != 1 {
		t.Fatalf("got %d lines, want 1 (scoping note only, malformed skipped)", len(got))
	}
}

// TestDropsDefaultV2LayoutUnchanged asserts that a nil fieldIndex still correctly
// parses standard v2 records (default behavior unchanged after N2).
func TestDropsDefaultV2LayoutUnchanged(t *testing.T) {
	fake := &fakeCWL{
		out: &cloudwatchlogs.FilterLogEventsOutput{
			Events: []cwltypes.FilteredLogEvent{
				event("2 123456789010 eni-abc123 10.0.1.5 10.0.2.9 49152 443 6 5 300 1690000000 1690000060 REJECT OK", 1_690_000_059_000),
			},
		},
	}
	// fieldIndex explicitly nil → v2 default
	c := &Client{cwl: fake, logGroup: "vpc-flow-logs", maxEvents: 100, fieldIndex: nil}

	got, err := c.Drops(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("Drops: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d lines, want 2 (1 note + 1 flow)", len(got))
	}
	wantMsg := "10.0.1.5:49152 -> 10.0.2.9:443 REJECT (proto 6)"
	if got[1].Message != wantMsg {
		t.Errorf("Message = %q, want %q", got[1].Message, wantMsg)
	}
}
