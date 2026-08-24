// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cttypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"

	"github.com/Smana/runlore/internal/providers"
)

// ctFailedEvent builds a CloudTrail event whose raw payload carries an errorCode —
// the shape of a control-plane call that was attempted and rejected.
func ctFailedEvent(id, name, code, msg string, t time.Time) cttypes.Event {
	e := ctEvent(id, name, t)
	e.CloudTrailEvent = ptr(`{"errorCode":"` + code + `","errorMessage":"` + msg + `"}`)
	return e
}

// TestCloudChangesFailedOnly reproduces the BackupJobFailed dead end.
//
// Four investigations across three days concluded "the failing RDS resource could
// not be identified", each reporting the same blocker: the lookup returns the most
// recent mutating events cluster-wide, and at 25 those are all Karpenter and SSM
// churn. The failing CreateDBClusterSnapshot calls were in CloudTrail the whole
// time — just past the cap. Filtering to failures applies the cap to the events
// that answer a "why did this fail" question instead of to the noise ahead of them.
func TestCloudChangesFailedOnly(t *testing.T) {
	t0 := time.Date(2026, 8, 23, 3, 55, 0, 0, time.UTC)
	// Noise first (newest), the answer buried behind it — the live ordering.
	noise := make([]cttypes.Event, 0, 30)
	for i := range 30 {
		id := fmt.Sprintf("noise-%d", i)
		noise = append(noise, ctEvent(id, "CreateTags", t0.Add(-time.Duration(i)*time.Second)))
	}
	answer := ctFailedEvent("snap", "CreateDBClusterSnapshot", "InvalidDBClusterStateFault",
		"Can't create a snapshot because the database cluster aurora-serverless-postgres-old isn't currently in the available state.",
		t0.Add(-40*time.Minute))

	pages := []*cloudtrail.LookupEventsOutput{
		{Events: noise, NextToken: ptr("page2")},
		{Events: []cttypes.Event{answer}},
	}

	// Control: without the filter the answer is past the cap, exactly as observed.
	c := &Client{ct: &fakeCT{pages: pages}, maxEvents: 25}
	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{}, providers.CloudChangeFilter{})
	if err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if containsPath(got, "CreateDBClusterSnapshot") {
		t.Fatalf("fixture does not reproduce the bug: the answer was inside the cap")
	}

	// With the filter, the failed call is the whole result.
	c = &Client{ct: &fakeCT{pages: pages}, maxEvents: 25}
	got, err = c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{}, providers.CloudChangeFilter{FailedOnly: true})
	if err != nil {
		t.Fatalf("CloudChanges(FailedOnly): %v", err)
	}
	if !containsPath(got, "CreateDBClusterSnapshot") {
		t.Fatalf("FailedOnly must surface the failed call; got %d changes: %+v", len(got), got)
	}
	for _, ch := range got {
		if providers.IsChangeNote(ch) {
			continue
		}
		if !strings.Contains(ch.Source.Path, "FAILED") {
			t.Errorf("FailedOnly returned a successful event: %q", ch.Source.Path)
		}
	}
	// And the error code the on-call needs is carried through.
	if !containsPath(got, "InvalidDBClusterStateFault") {
		t.Errorf("the error code must reach the caller; got %+v", got)
	}
}

// TestCloudChangesFailedOnlyBoundsItsScan: failures are rare, so the filter must
// keep paginating past the cap to find them — but not without limit, or a quiet
// window walks the entire retention period one page at a time.
func TestCloudChangesFailedOnlyBoundsItsScan(t *testing.T) {
	t0 := time.Date(2026, 8, 23, 3, 55, 0, 0, time.UTC)
	// Every page is noise and there is always another one.
	page := func(i int) *cloudtrail.LookupEventsOutput {
		return &cloudtrail.LookupEventsOutput{
			Events:    []cttypes.Event{ctEvent(fmt.Sprintf("n-%d", i), "CreateTags", t0)},
			NextToken: ptr("more"),
		}
	}
	pages := make([]*cloudtrail.LookupEventsOutput, 0, 500)
	for i := range 500 {
		pages = append(pages, page(i))
	}
	ct := &fakeCT{pages: pages}
	c := &Client{ct: ct, maxEvents: 25}
	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{}, providers.CloudChangeFilter{FailedOnly: true})
	if err != nil {
		t.Fatalf("CloudChanges(FailedOnly): %v", err)
	}
	// Pinned to the constant, not to a loose ceiling. An earlier revision asserted
	// `> 50`, which passed for any budget from 2 to 50 — so raising the constant to a
	// 2.5x latency regression against a ~2 TPS API was invisible to CI.
	if ct.call != maxFailureScanPages {
		t.Errorf("the failure scan made %d LookupEvents calls, want exactly %d", ct.call, maxFailureScanPages)
	}
	// The RESULT of a budget-exhausted scan matters as much as its cost. It must carry
	// the note — "we stopped reading" is the one thing a caller must never read as
	// "the window was quiet" — and NOTHING else, since no real failure was found.
	if n := len(got); n != 1 {
		t.Fatalf("want the bounded-scan note and no events, got %d rows: %+v", n, got)
	}
	if !providers.IsChangeNote(got[0]) {
		t.Errorf("a scan that found no failures returned an event row: %+v", got[0])
	}
	if !strings.Contains(got[0].Workload.Name, "scan stopped after") {
		t.Errorf("the note must say the scan stopped reading: %q", got[0].Workload.Name)
	}
}

// TestCloudChangesFailedOnlyScanNoteNotTruncation: when the budget runs out with
// real failures in hand, the note must describe what actually happened. The cap's
// sentinel says "more events matched; narrow the window", which is both false (the
// cap never bound) and backwards — a sparse failure is OLDER than the newest events,
// so narrowing the window moves it further out of reach.
func TestCloudChangesFailedOnlyScanNoteNotTruncation(t *testing.T) {
	t0 := time.Date(2026, 8, 23, 3, 55, 0, 0, time.UTC)
	pages := make([]*cloudtrail.LookupEventsOutput, 0, 500)
	// One real failure up front, then endless churn, so the scan ends on its budget.
	pages = append(pages, &cloudtrail.LookupEventsOutput{
		Events: []cttypes.Event{ctFailedEvent("snap", "CreateDBClusterSnapshot",
			"InvalidDBClusterStateFault", "cluster is stopped", t0)},
		NextToken: ptr("more"),
	})
	for i := range 500 {
		pages = append(pages, &cloudtrail.LookupEventsOutput{
			Events:    []cttypes.Event{ctEvent(fmt.Sprintf("n-%d", i), "CreateTags", t0)},
			NextToken: ptr("more"),
		})
	}
	c := &Client{ct: &fakeCT{pages: pages}, maxEvents: 25}
	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{}, providers.CloudChangeFilter{FailedOnly: true})
	if err != nil {
		t.Fatalf("CloudChanges(FailedOnly): %v", err)
	}
	var note string
	for _, ch := range got {
		if providers.IsChangeNote(ch) {
			note = ch.Workload.Name
		}
	}
	if note == "" {
		t.Fatalf("a bounded scan must say so; got %+v", got)
	}
	if strings.Contains(note, "truncated at") {
		t.Errorf("a bounded scan reused the CAP's sentinel, whose advice is backwards here: %q", note)
	}
	if !strings.Contains(note, "older") {
		t.Errorf("the note must say what was not examined: %q", note)
	}
}

// TestCloudChangesFailedOnlyCompleteScanIsNotPartial: a scan that finishes on its
// last allowed page is COMPLETE. Reporting it as partial tells the model its one
// real answer might be missing a sibling that does not exist.
func TestCloudChangesFailedOnlyCompleteScanIsNotPartial(t *testing.T) {
	t0 := time.Date(2026, 8, 23, 3, 55, 0, 0, time.UTC)
	pages := make([]*cloudtrail.LookupEventsOutput, 0, maxFailureScanPages)
	for i := range maxFailureScanPages - 1 {
		pages = append(pages, &cloudtrail.LookupEventsOutput{
			Events:    []cttypes.Event{ctEvent(fmt.Sprintf("n-%d", i), "CreateTags", t0)},
			NextToken: ptr("more"),
		})
	}
	// The last page carries the answer and NO NextToken — there is nothing more.
	pages = append(pages, &cloudtrail.LookupEventsOutput{
		Events: []cttypes.Event{ctFailedEvent("snap", "CreateDBClusterSnapshot",
			"InvalidDBClusterStateFault", "cluster is stopped", t0)},
	})
	c := &Client{ct: &fakeCT{pages: pages}, maxEvents: 25}
	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{}, providers.CloudChangeFilter{FailedOnly: true})
	if err != nil {
		t.Fatalf("CloudChanges(FailedOnly): %v", err)
	}
	if n := countTruncated(got); n != 0 {
		t.Errorf("a complete scan was reported as partial: %+v", got)
	}
	if !containsPath(got, "InvalidDBClusterStateFault") {
		t.Errorf("the answer on the last page must survive; got %+v", got)
	}
}

func containsPath(changes []providers.Change, substr string) bool {
	for _, c := range changes {
		if strings.Contains(c.Source.Path, substr) || strings.Contains(c.Workload.Name, substr) {
			return true
		}
	}
	return false
}
