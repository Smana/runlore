package aws

import (
	"context"
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
	in := &cloudtrail.LookupEventsInput{
		LookupAttributes: []cttypes.LookupAttribute{{
			AttributeKey:   cttypes.LookupAttributeKeyReadOnly,
			AttributeValue: ptr("false"), // mutating events only
		}},
	}
	if !w.Start.IsZero() {
		in.StartTime = ptr(w.Start)
	}
	if !w.End.IsZero() {
		in.EndTime = ptr(w.End)
	}
	if sel.Name != "" {
		in.LookupAttributes = append(in.LookupAttributes, cttypes.LookupAttribute{
			AttributeKey:   cttypes.LookupAttributeKeyResourceName,
			AttributeValue: ptr(sel.Name),
		})
	}

	out, err := c.ct.LookupEvents(ctx, in)
	if err != nil {
		return nil, fmt.Errorf("cloudtrail lookup: %w", err)
	}
	changes := make([]providers.Change, 0, len(out.Events))
	for i := range out.Events {
		changes = append(changes, eventToChange(out.Events[i]))
	}
	// Sort most-recent-first BEFORE capping, so the cap keeps the newest events
	// regardless of the API's return order.
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].When.After(changes[j].When) })
	if len(changes) > c.maxEvents {
		changes = changes[:c.maxEvents]
	}
	return changes, nil
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
	// Source.Path repurposed to carry "eventName by username" — rendered by the tool.
	ch.Source = providers.SourceRef{Path: deref(e.EventName) + " by " + deref(e.Username)}
	return ch
}
