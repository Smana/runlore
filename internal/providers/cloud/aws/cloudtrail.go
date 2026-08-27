// SPDX-License-Identifier: Apache-2.0

package aws

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

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
func (c *Client) CloudChanges(ctx context.Context, sel providers.Selector, w providers.TimeWindow, f providers.CloudChangeFilter) ([]providers.Change, error) {
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
	// One value rather than two flags: the only question the loop answers is WHY it
	// stopped, and the answer IS the note the caller gets. Empty means it read to the
	// end. The cap and the budget are different stops carrying opposite advice, so
	// they must not share a message.
	stopNote := ""
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
			// Parsed once here and handed to eventToChange, which used to re-parse the
			// identical payload for the message. Note this is an if-init-free spelling on
			// purpose: `if code, _ := eventError(e); f.FailedOnly && ...` runs the parse
			// unconditionally, because Go evaluates the init statement before the guard.
			code, msg := eventError(out.Events[i])
			if f.FailedOnly && code == "" {
				continue
			}
			changes = append(changes, eventToChange(out.Events[i], code, msg))
		}
		if len(changes) > c.maxEvents {
			stopNote = providers.ChangeTruncatedNote(int64(c.maxEvents))
			break // we already have more than the cap; further pages cannot change the kept top-N
		}
		// A failure scan has to look past the cap — failures are sparse, and the
		// answer is typically behind pages of successful churn. Bound it anyway: a
		// window with no failures at all would otherwise walk the whole retention
		// period one page at a time.
		//
		// See scanBoundedNote for why this does not reuse the cap's message.
		if f.FailedOnly && pages >= maxFailureScanPages {
			// Only partial if there was actually more to read. A scan that happens to
			// finish on its last allowed page is complete, and saying otherwise tells
			// the model its one real answer might be missing a sibling.
			if p.HasMorePages() {
				stopNote = scanBoundedNote()
			}
			break
		}
	}
	// Sort most-recent-first BEFORE capping, so the cap keeps the newest events
	// regardless of the API's return order.
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].When.After(changes[j].When) })
	if len(changes) > c.maxEvents {
		if stopNote == "" {
			stopNote = providers.ChangeTruncatedNote(int64(c.maxEvents))
		}
		changes = changes[:c.maxEvents]
	}
	// Appended AFTER the sort+slice so it always lands last (a zero When would
	// otherwise sort it among real events). It is appended even when nothing else
	// matched: "the scan stopped reading" is the one thing a caller must never read
	// as "the window was quiet", and that is precisely the case with no other row to
	// carry it. A note-only slice is therefore not an empty one — callers separate
	// the two with providers.IsChangeNote.
	if stopNote != "" {
		changes = append(changes, providers.ChangeNote(providers.EngineAWS, stopNote))
	}
	return changes, nil
}

// maxFailureScanPages bounds the extra pagination a FailedOnly scan performs. A
// CloudTrail page is <=50 events, so this is up to ~1000 events examined to find
// the cap's worth of failures — enough to reach past a busy cluster's routine
// churn, and small enough that a quiet window returns promptly.
const maxFailureScanPages = 20

// eventError returns the errorCode and errorMessage from an event's raw CloudTrail
// payload; code is "" when the call succeeded. Single reader of that payload — the
// FailedOnly filter and the rendered "FAILED:" annotation both key on it, so the two
// cannot disagree about what failed.
func eventError(e cttypes.Event) (code, message string) {
	raw := deref(e.CloudTrailEvent)
	// A successful call omits errorCode entirely, which is almost every event on a
	// failure scan sized to examine ~1000 of them. A substring test costs no
	// allocation; unmarshalling a multi-KB document to learn the same thing costs two
	// and an order of magnitude more time. A hit inside requestParameters just falls
	// through to the real parse, so this cannot produce a false negative.
	if raw == "" || !strings.Contains(raw, `"errorCode"`) {
		return "", ""
	}
	var payload ctEventJSON
	if err := json.Unmarshal([]byte(raw), &payload); err != nil {
		return "", ""
	}
	return payload.ErrorCode, payload.ErrorMessage
}

// ctEventJSON is the minimal shape of the raw CloudTrail JSON payload we need
// to surface failed-call context. errorCode and errorMessage are omitted on
// successful calls; their presence signals a failed API call (e.g.
// InsufficientInstanceCapacity, UnauthorizedOperation).
type ctEventJSON struct {
	ErrorCode    string `json:"errorCode"`
	ErrorMessage string `json:"errorMessage"`
}

// scanBoundedNote is what the BUDGET says, which is the opposite advice. Nothing was
// truncated at the cap; the scan simply stopped reading, and a sparse failure is
// typically OLDER than the newest events — so "narrow the window" would push it
// further out of reach. The page count is the constant rather than a variable:
// this note exists only at the moment the budget is reached.
func scanBoundedNote() string {
	return fmt.Sprintf("scan stopped after %d pages of successful events; any FAILED calls older "+
		"than that were not examined — narrow the window's END, or scope to a resource", maxFailureScanPages)
}

// eventToChange maps a CloudTrail event to an engine-agnostic Change.
func eventToChange(e cttypes.Event, code, msg string) providers.Change {
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
			Kind: providers.CloudKind(providers.EngineAWS, deref(e.Resources[0].ResourceType)),
			Name: deref(e.Resources[0].ResourceName),
		}
	} else {
		// The EventSource fallback ("ec2.amazonaws.com") is dotted and carries no colon,
		// so unlike a real "AWS::EC2::Instance" it did NOT read as non-Kubernetes
		// downstream and the card dropped the resource identity. CloudKind qualifies both.
		ch.Workload = providers.Workload{
			Kind: providers.CloudKind(providers.EngineAWS, deref(e.EventSource)),
			Name: deref(e.EventName),
		}
	}
	// Source.Path carries "eventName by username", plus a FAILED suffix when the
	// raw CloudTrail JSON carries an errorCode — so the model sees failed calls
	// (InsufficientInstanceCapacity, UnauthorizedOperation, etc.) not as successes.
	// Shared with the GCP lens so the two clouds cannot spell this field differently.
	// It also drops the dangling " by " a service-initiated event used to render with —
	// an empty Username produced "RunInstances by ", which claims a caller whose identity
	// was lost rather than an action nobody called.
	ch.Source = providers.SourceRef{
		Path: providers.CallPath(deref(e.EventName), deref(e.Username), code, msg),
	}
	return ch
}
