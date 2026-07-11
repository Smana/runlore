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
// carries a Name, it scopes the lookup to that resource.
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
	for p.HasMorePages() {
		out, err := p.NextPage(ctx)
		if err != nil {
			return nil, fmt.Errorf("cloudtrail lookup: %w", err)
		}
		for i := range out.Events {
			// When resource-scoped the server cannot also filter by ReadOnly, so
			// drop read-only events here. e.ReadOnly is "true"/"false" (string).
			if resourceScoped && deref(out.Events[i].ReadOnly) == "true" {
				continue
			}
			changes = append(changes, eventToChange(out.Events[i]))
		}
		if len(changes) > c.maxEvents {
			truncated = true
			break // we already have more than the cap; further pages cannot change the kept top-N
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
	if raw := deref(e.CloudTrailEvent); raw != "" {
		var payload ctEventJSON
		if err := json.Unmarshal([]byte(raw), &payload); err == nil && payload.ErrorCode != "" {
			path += " — FAILED: " + payload.ErrorCode
			if payload.ErrorMessage != "" {
				path += " (" + payload.ErrorMessage + ")"
			}
		}
	}
	ch.Source = providers.SourceRef{Path: path}
	return ch
}
