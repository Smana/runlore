// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/aws/aws-sdk-go-v2/service/cloudtrail"
	cttypes "github.com/aws/aws-sdk-go-v2/service/cloudtrail/types"

	"github.com/Smana/runlore/internal/providers"
)

// CloudChanges returns recent MUTATING AWS control-plane events (CloudTrail
// LookupEvents) in the window, normalized to the engine-agnostic Change model so
// they join the same "what changed" timeline as GitOps diffs. When the selector
// carries a Name, it scopes the lookup to that resource; when it sets FailedOnly,
// only rejected calls are kept.
//
// FailedOnly exists because the result cap is applied to the NEWEST events, and on
// a Karpenter cluster the newest mutating events are routine instance and tag
// churn. Live: four investigations of a failing AWS Backup job concluded the
// resource "could not be identified", each correctly reporting that the cluster-wide
// lookup was full of Karpenter/SSM noise — while the CreateDBClusterSnapshot calls
// that answered the question sat in CloudTrail just past the cap. Filtering first
// spends the cap on events that can answer a "why did this fail" question.
// CloudTrail accepts exactly one LookupAttribute, already spent on ResourceName or
// ReadOnly, so the filter is client-side and the scan is bounded below.
//
// Note: CloudTrail is eventually consistent (~15 min), so a too-narrow window can
// miss a just-made change — callers should use a generous lookback.
func (c *Client) CloudChanges(ctx context.Context, sel providers.Selector, w providers.TimeWindow) ([]providers.Change, error) {
	// CloudTrail LookupEvents accepts exactly ONE LookupAttribute per request.
	// When a resource name is given, scope by ResourceName and filter read-only
	// events client-side (the Event carries a ReadOnly field). When no resource
	// is given, filter to mutating events server-side with a single ReadOnly=false
	// attribute — the cheaper and more common path.
	var resourceScoped bool
	if sel.Name != "" {
		resourceScoped = true
	}

	var attrs []cttypes.LookupAttribute
	if resourceScoped {
		attrs = []cttypes.LookupAttribute{{
			AttributeKey:   cttypes.LookupAttributeKeyResourceName,
			AttributeValue: ptr(sel.Name),
		}}
	} else {
		attrs = []cttypes.LookupAttribute{{
			AttributeKey:   cttypes.LookupAttributeKeyReadOnly,
			AttributeValue: ptr("false"), // mutating events only
		}}
	}

	in := &cloudtrail.LookupEventsInput{LookupAttributes: attrs}
	if !w.Start.IsZero() {
		in.StartTime = ptr(w.Start)
	}
	if !w.End.IsZero() {
		in.EndTime = ptr(w.End)
	}

	// Paginate via the SDK paginator (a CloudTrail page is ≤50 events); a single
	// LookupEvents call would silently drop pages 2+ when the window has more
	// mutating events than fit one page. Over-collect by one past the cap so we
	// can tell the cap is *binding* (more existed) versus an exactly-full result.
	p := cloudtrail.NewLookupEventsPaginator(c.ct, in)
	var changes []providers.Change
	truncated := false
	pages := 0
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("cloudtrail lookup: %w", err)
		}
		pages++
		for i := range out.Events {
			// When resource-scoped the server cannot also filter by ReadOnly, so
			// drop read-only events here. e.ReadOnly is "true"/"false" (string).
			if resourceScoped && deref(out.Events[i].ReadOnly) == "true" {
				continue
			}
			if sel.FailedOnly && eventError(out.Events[i]) == "" {
				continue
			}
			changes = append(changes, eventToChange(out.Events[i]))
		}
		if len(changes) > c.maxEvents {
			truncated = true
			break // we already have more than the cap; further pages cannot change the kept top-N
		}
		// A failure scan has to look past the cap — failures are sparse, and the
		// answer is typically behind pages of successful churn. Bound it anyway: a
		// window with no failures at all would otherwise walk the whole retention
		// period one page at a time. Hitting the budget is a partial view, so it
		// reuses the truncation sentinel rather than reporting a clean empty result.
		if sel.FailedOnly && pages >= maxFailureScanPages {
			truncated = true
			break
		}
	}
	// Sort most-recent-first BEFORE capping, so the cap keeps the newest events
	// regardless of the API's return order.
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].When.After(changes[j].When) })
	if len(changes) > c.maxEvents {
		truncated = true
		changes = changes[:c.maxEvents]
	}
	// Append the sentinel AFTER the sort+slice so it always lands last (a zero
	// When would otherwise sort it among real events), signalling a partial view.
	if truncated {
		changes = append(changes, truncatedChange(c.maxEvents))
	}
	return changes, nil
}

// maxFailureScanPages bounds the extra pagination a FailedOnly scan performs. A
// CloudTrail page is <=50 events, so this is up to ~1000 events examined to find
// the cap's worth of failures — enough to reach past a busy cluster's routine
// churn, and small enough that a quiet window returns promptly.
const maxFailureScanPages = 20

// eventError returns the errorCode from an event's raw CloudTrail payload, or ""
// when the call succeeded (successful calls omit the field). It is the single
// reader of that payload: both the FailedOnly filter and the rendered "FAILED:"
// annotation key on it, so the two can never disagree about what failed.
func eventError(e cttypes.Event) string {
	raw := deref(e.CloudTrailEvent)
	if raw == "" {
		return ""
	}
	var payload ctEventJSON
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return ""
	}
	return payload.ErrorCode
}

// truncatedChange is the sentinel appended when CloudChanges stops at its cap
// with more events upstream, so the model knows the timeline is partial. It is
// not a real event: Kind "(truncated)" is the recognizable marker, and it is
// appended last so cloud_tools renders it as a trailing note.
func truncatedChange(limit int) providers.Change {
	return providers.Change{
		Engine: providers.EngineAWS,
		Type:   providers.ChangeCloudAPI,
		Workload: providers.Workload{
			Kind: "(truncated)",
			Name: fmt.Sprintf("results truncated at %d — more events matched; narrow the window or resource", limit),
		},
	}
}

// ctEventJSON is the minimal shape of the raw CloudTrail JSON payload we need
// to surface failed-call context. errorCode and errorMessage are omitted on
// successful calls; their presence signals a failed API call (e.g.
// InsufficientInstanceCapacity, UnauthorizedOperation).
type ctEventJSON struct {
	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
}

// eventToChange maps a CloudTrail event to an engine-agnostic Change.
func eventToChange(e cttypes.Event) providers.Change {
	ch := providers.Change{
		Engine:    providers.EngineAWS,
		Type:      providers.ChangeCloudAPI,
		ManagedBy: deref(e.EventSource), // e.g. autoscaling.amazonaws.com
		ToRev:     deref(e.EventId),     // stable handle for the model's change_ref
	}
	if e.EventTime != nil {
		ch.When = *e.EventTime
	}
	// Workload: the first resource the event touched, else the event name.
	if len(e.Resources) > 0 {
		ch.Workload = providers.Workload{
			Kind: deref(e.Resources[0].ResourceType),
			Name: deref(e.Resources[0].ResourceName),
		}
	} else {
		ch.Workload = providers.Workload{Kind: deref(e.EventSource), Name: deref(e.EventName)}
	}
	// Source.Path carries "eventName by username", plus a FAILED suffix when the
	// raw CloudTrail JSON carries an errorCode — so the model sees failed calls
	// (InsufficientInstanceCapacity, UnauthorizedOperation, etc.) not as successes.
	path := deref(e.EventName) + " by " + deref(e.Username)
	if code := eventError(e); code != "" {
		path += " — FAILED: " + code
		var payload ctEventJSON
		if err := json.Unmarshal([]byte(deref(e.CloudTrailEvent)), &payload); err == nil && payload.ErrorMessage != "" {
			path += " (" + payload.ErrorMessage + ")"
		}
	}
	ch.Source = providers.SourceRef{Path: path}
	return ch
}
