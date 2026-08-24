// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// CloudWhatChangedTool exposes recent mutating AWS control-plane events (CloudTrail)
// as the AWS-layer "what changed" lens — infra/manual changes invisible to GitOps.
type CloudWhatChangedTool struct {
	Cloud providers.CloudProvider
}

// Name returns the tool name.
func (t CloudWhatChangedTool) Name() string { return "cloud_what_changed" }

// Description returns the tool description.
func (t CloudWhatChangedTool) Description() string {
	return "List recent MUTATING AWS control-plane events (CloudTrail) — ASG/EC2/EKS/RDS/SG changes, " +
		"manual actions, and other infra changes invisible to GitOps. Use when no Git change explains " +
		"the incident. Optional resource is an EXACT CloudTrail ResourceName — a full ARN, instance-id, " +
		"ASG name, or a resource's full path (e.g. a Secrets Manager secret's \"apps/team/name\") — never a " +
		"service name or substring; OMIT it to see every mutating event, which is the right move when you do " +
		"not know the exact identifier. Set failed_only=true when the incident IS a failed AWS operation and " +
		"you do not know which resource it happened to (a failed backup/snapshot job, a rejected API call): " +
		"results are capped at the NEWEST events, which on a Karpenter cluster are routine instance and tag " +
		"churn, so the rejected call you are looking for is usually just past the cap. failed_only spends the " +
		"cap on rejected calls instead and reports each one's error code. since_minutes default 90 " +
		"(CloudTrail lags ~15m)."
}

// Schema returns the JSON schema for the arguments.
func (t CloudWhatChangedTool) Schema() string {
	return `{"type":"object","properties":{"resource":{"type":"string"},"since_minutes":{"type":"integer"},` +
		`"failed_only":{"type":"boolean","description":"keep only control-plane calls that were REJECTED, reporting each error code; use when the incident is itself a failed AWS operation"}},"required":[]}`
}

// Call lists cloud changes over the window and renders them.
func (t CloudWhatChangedTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Resource     string `json:"resource"`
		SinceMinutes int    `json:"since_minutes"`
		FailedOnly   bool   `json:"failed_only"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	since := in.SinceMinutes
	if since <= 0 {
		since = 90
	}
	end := time.Now()
	start := end.Add(-time.Duration(since) * time.Minute)
	window := providers.TimeWindow{Start: start, End: end}
	changes, err := t.Cloud.CloudChanges(ctx, providers.Selector{Name: in.Resource, FailedOnly: in.FailedOnly}, window)
	if err != nil {
		return "", err
	}

	// CloudTrail's ResourceName lookup is an EXACT match on the full AWS resource
	// name or ARN — not a substring, not a service name. A guessed scope therefore
	// returns zero events that are indistinguishable from "nothing changed", and the
	// model reasonably concludes the control plane was quiet.
	//
	// That is not hypothetical. An AWS Secrets Manager secret was deleted, CloudTrail
	// recorded it under ResourceName "apps/app-wizard/llm", and the investigation
	// tried "secretsmanager", "secretsmanager.amazonaws.com" and "app-wizard" — all
	// exact-match misses. It reported the deletion event as uncapturable and left
	// "who deleted it" as an open question, with the answer sitting one unscoped
	// lookup away.
	//
	// So a scoped miss retries unscoped rather than dead-ending. The banner says the
	// filter was dropped, because silently widening a query the model asked to narrow
	// would be worse than the dead end.
	var widened bool
	if len(changes) == 0 && in.Resource != "" {
		all, aerr := t.Cloud.CloudChanges(ctx, providers.Selector{FailedOnly: in.FailedOnly}, window)
		if aerr == nil && len(all) > 0 {
			changes, widened = all, true
		}
	}

	if len(changes) == 0 {
		if in.FailedOnly {
			// Say which filter produced the empty result. "No events" from a filtered
			// lookup is not the same claim as "the control plane was quiet", and the
			// schema asks the model not to read absence as evidence.
			return "no FAILED AWS control-plane calls in the window (successful events were not listed — re-run without failed_only to see them)", nil
		}
		return "no mutating AWS events in the window", nil
	}
	var b strings.Builder
	if widened {
		fmt.Fprintf(&b, "resource %q matched no CloudTrail events — ResourceName is an exact match on the "+
			"full AWS resource name or ARN (e.g. a secret's full path \"apps/team/name\"), not a service or "+
			"substring. Showing ALL mutating events in the window instead:\n", in.Resource)
	}
	renderRows(&b, len(changes), "more", func(i int) {
		c := changes[i]
		fmt.Fprintf(&b, "%s %s %s/%s\n", c.When.Format(time.RFC3339), c.ManagedBy, c.Workload.Kind, c.Workload.Name)
		if c.Source.Path != "" {
			fmt.Fprintf(&b, "  %s\n", c.Source.Path)
		}
	})
	return b.String(), nil
}

// CloudResourceHealthTool exposes AWS-side resource health (EC2/ASG/EKS) to the model.
type CloudResourceHealthTool struct {
	Cloud providers.CloudProvider
}

// Name returns the tool name.
func (t CloudResourceHealthTool) Name() string { return "cloud_resource_health" }

// Description returns the tool description.
func (t CloudResourceHealthTool) Description() string {
	return "Describe AWS-side health for the cluster's nodes/capacity: EKS nodegroup status + health " +
		"issues, ASG scaling activities (launch/capacity failures), and — when given an EC2 instance-id " +
		"(i-…) — its instance/system status checks. Use to confirm a node/infra/capacity cause. " +
		"Optional since_minutes scopes the scaling-activity lookback to the incident window " +
		"(default: recent activities)."
}

// Schema returns the JSON schema for the arguments.
func (t CloudResourceHealthTool) Schema() string {
	return `{"type":"object","properties":{"instance_id":{"type":"string","description":"optional EC2 instance id (i-…)"},"since_minutes":{"type":"integer","description":"scope scaling-activity lookback to the last N minutes"}},"required":[]}`
}

// Call renders cloud resource health.
func (t CloudResourceHealthTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		InstanceID   string `json:"instance_id"`
		SinceMinutes int    `json:"since_minutes"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	// A since_minutes bounds the scaling-activity lookback; unset ⇒ zero window
	// (today's behaviour: recent activities, unscoped).
	var window providers.TimeWindow
	if in.SinceMinutes > 0 {
		end := time.Now()
		window = providers.TimeWindow{Start: end.Add(-time.Duration(in.SinceMinutes) * time.Minute), End: end}
	}
	lines, err := t.Cloud.ResourceHealth(ctx, providers.Selector{Name: in.InstanceID}, window)
	if err != nil {
		return "", err
	}
	if len(lines) == 0 {
		return "no AWS resource health returned", nil
	}
	var b strings.Builder
	renderRows(&b, len(lines), "more", func(i int) {
		fmt.Fprintln(&b, lines[i].Message)
	})
	return b.String(), nil
}
