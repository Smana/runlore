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
	got, err := c.CloudChanges(context.Background(), providers.Selector{}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("CloudChanges: %v", err)
	}
	if containsEvent(got, "CreateDBClusterSnapshot") {
		t.Fatalf("fixture does not reproduce the bug: the answer was inside the cap")
	}

	// With the filter, the failed call is the whole result.
	c = &Client{ct: &fakeCT{pages: pages}, maxEvents: 25}
	got, err = c.CloudChanges(context.Background(), providers.Selector{FailedOnly: true}, providers.TimeWindow{})
	if err != nil {
		t.Fatalf("CloudChanges(FailedOnly): %v", err)
	}
	if !containsEvent(got, "CreateDBClusterSnapshot") {
		t.Fatalf("FailedOnly must surface the failed call; got %d changes: %+v", len(got), got)
	}
	for _, ch := range got {
		if ch.Workload.Kind == "(truncated)" {
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
	if _, err := c.CloudChanges(context.Background(), providers.Selector{FailedOnly: true}, providers.TimeWindow{}); err != nil {
		t.Fatalf("CloudChanges(FailedOnly): %v", err)
	}
	if ct.call > 50 {
		t.Errorf("the failure scan is unbounded: made %d LookupEvents calls", ct.call)
	}
	if ct.call < 2 {
		t.Errorf("the failure scan must look past the first page, made %d calls", ct.call)
	}
}

func containsEvent(changes []providers.Change, name string) bool {
	return containsPath(changes, name)
}

func containsPath(changes []providers.Change, substr string) bool {
	for _, c := range changes {
		if strings.Contains(c.Source.Path, substr) || strings.Contains(c.Workload.Name, substr) {
			return true
		}
	}
	return false
}
