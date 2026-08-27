// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cttypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"

	"github.com/Smana/runlore/internal/investigate"
)

// TestAScopedMissWidensAndExplainsItWithCloudTrailsOwnRule proves the AWS banner
// actually reaches the model.
//
// The two halves are each covered already — the tool knows to widen on a scoped miss,
// and the vocabulary holds AWS's wording — but nothing joined them, so nothing would
// have caught a vocabulary whose WidenedBanner was left unset, or one copied from
// another cloud. The banner is where the exact-match rule is explained, and that
// explanation is the whole reason the widen exists: an investigation once reported a
// deleted Secrets Manager secret as uncapturable with the answer one unscoped lookup
// away, because CloudTrail's ResourceName is an exact match and a miss is
// indistinguishable from silence.
//
// This mirrors the GCP-side test of the same composition. It lives here, at the leaf,
// because internal/investigate must never import a concrete provider — so its own
// suite can only ever prove that SOME vocabulary flows through, never that this one
// does.
func TestAScopedMissWidensAndExplainsItWithCloudTrailsOwnRule(t *testing.T) {
	t0 := time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC)
	deletion := ctEvent("evt-1", "DeleteSecret", t0)

	// The scoped leg misses and the unscoped retry finds the event — the shape of the
	// real incident.
	var calls int
	ct := &fakeCTFunc{fn: func(in *cloudtrail.LookupEventsInput) *cloudtrail.LookupEventsOutput {
		calls++
		for _, a := range in.LookupAttributes {
			if a.AttributeKey == cttypes.LookupAttributeKeyResourceName {
				return &cloudtrail.LookupEventsOutput{}
			}
		}
		return &cloudtrail.LookupEventsOutput{Events: []cttypes.Event{deletion}}
	}}

	out, err := (investigate.CloudWhatChangedTool{Cloud: &Client{ct: ct, maxEvents: 25}}).
		Call(context.Background(), `{"resource":"secretsmanager"}`)
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if calls != 2 {
		t.Errorf("want a scoped lookup then an unscoped retry, got %d calls", calls)
	}
	if !strings.Contains(out, "DeleteSecret") {
		t.Errorf("the event the widen exists to surface did not survive it:\n%s", out)
	}
	for _, want := range []struct{ name, sub string }{
		{"the banner says the filter was dropped, so the rows are not read as scoped", "exact match"},
		{"the resource that missed is quoted back", "secretsmanager"},
	} {
		t.Run(want.name, func(t *testing.T) {
			if !strings.Contains(out, want.sub) {
				t.Errorf("widened output does not contain %q:\n%s", want.sub, out)
			}
		})
	}
	// The GCP banner explains a SUBSTRING match. Rendering it here would tell the model
	// something false about CloudTrail's semantics — the specific confusion the
	// per-cloud vocabulary exists to prevent.
	if strings.Contains(out, "SUBSTRING") {
		t.Errorf("the GCP banner rendered on an AWS provider:\n%s", out)
	}
}

// fakeCTFunc answers each lookup from fn, so a test can vary the reply by request
// rather than by call order — the scoped and unscoped legs differ by attribute, not by
// position.
type fakeCTFunc struct {
	fn func(*cloudtrail.LookupEventsInput) *cloudtrail.LookupEventsOutput
}

func (f *fakeCTFunc) LookupEvents(_ context.Context, in *cloudtrail.LookupEventsInput, _ ...func(*cloudtrail.Options)) (*cloudtrail.LookupEventsOutput, error) {
	return f.fn(in), nil
}
