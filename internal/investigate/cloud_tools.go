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

// vocabularyFor returns the cloud's own naming — what its audit log is called, how
// its resource filter matches, which instance identifier its schema should ask for —
// or the AWS wording when the provider does not describe itself.
//
// The fallback is the compatibility half of the promise: every CloudProvider that
// existed before providers.CloudDescriber did keeps the tool text it had, byte for
// byte. A nil Cloud resolves the same way rather than panicking, which
// IncidentTimelineTool relies on — it is registered whenever ANY of its three
// datasources is wired, so on a cluster with no cloud provider it still renders a
// description naming one.
func vocabularyFor(c providers.CloudProvider) providers.CloudVocabulary {
	if d, ok := c.(providers.CloudDescriber); ok {
		return d.CloudVocabulary()
	}
	return providers.AWSCloudVocabulary()
}

// jsonString encodes s as a JSON string literal, quotes included, for splicing into
// the hand-written schema templates below — the same shape opEnumJSON (tools.go) uses
// to splice the executable-op enum into submit_findings' schema. Splicing keeps the
// schemas readable JSON in source and keeps their key order exactly as shipped, which
// building them from a map[string]any would not: encoding/json sorts map keys, so a
// map silently reorders every property. Marshalling a string has no reachable error
// path — invalid UTF-8 is replaced with U+FFFD, not rejected.
func jsonString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// CloudWhatChangedTool exposes recent mutating cloud control-plane events (CloudTrail
// on AWS, Cloud Audit Logs on GCP) as the cloud-layer "what changed" lens —
// infra/manual changes invisible to GitOps. Every model-facing string it emits is
// worded by the wired provider's vocabulary; see vocabularyFor.
type CloudWhatChangedTool struct {
	Cloud providers.CloudProvider
}

// Name returns the tool name.
func (t CloudWhatChangedTool) Name() string { return "cloud_what_changed" }

// Description returns the tool description, worded for the cloud actually wired.
func (t CloudWhatChangedTool) Description() string {
	return vocabularyFor(t.Cloud).ChangeDescription()
}

// Schema returns the JSON schema for the arguments. failed_only's description is
// cloud-specific — it promises which calls this tool can and cannot show, and that
// promise rests on the audit log's own rules about recording reads versus writes — so
// it comes from the vocabulary rather than being fixed here.
func (t CloudWhatChangedTool) Schema() string {
	return `{"type":"object","properties":{"resource":{"type":"string"},"since_minutes":{"type":"integer"},` +
		`"failed_only":{"type":"boolean","description":` + jsonString(vocabularyFor(t.Cloud).FailureFilterArg) +
		`}},"required":[]}`
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
	window := windowSince(in.SinceMinutes, 90)
	filter := providers.CloudChangeFilter{FailedOnly: in.FailedOnly}
	changes, err := t.Cloud.CloudChanges(ctx, providers.Selector{Name: in.Resource}, window, filter)
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
	//
	// Not when the scoped scan already spent its page budget, though: the widen would
	// spend a second one, and LookupEvents is limited to ~2 TPS per account/region, so
	// 40 sequential pages can outlast the per-tool timeout and turn a partial answer
	// into a hard dead end. A bounded scan is reported as bounded instead.
	//
	// The banner's text comes from the provider's vocabulary, because every paragraph
	// above is an AWS fact and none of it survives the trip to another cloud. GCP's
	// Cloud Logging filter language has a substring operator (`:`), so a scoped miss
	// there means the resource genuinely did not appear in the window; telling that
	// model it had probably used the wrong match semantics would send it renaming a
	// resource it had already identified correctly. The RETRY is cloud-independent —
	// a scoped miss is worth widening either way — so only the wording moves.
	events, note := splitNote(changes)
	var widened bool
	if len(events) == 0 && in.Resource != "" && note == "" {
		all, aerr := t.Cloud.CloudChanges(ctx, providers.Selector{}, window, filter)
		if allEvents, allNote := splitNote(all); aerr == nil && len(allEvents) > 0 {
			events, note, widened = allEvents, allNote, true
		}
	}

	if len(events) == 0 {
		vocab := vocabularyFor(t.Cloud)
		if !in.FailedOnly {
			return vocab.EmptyChangesMessage(), nil
		}
		// Say which filter produced the empty result. "No events" from a filtered
		// lookup is not the same claim as "the control plane was quiet", and the schema
		// asks the model not to read absence as evidence. A bounded scan did not
		// establish absence at all — it stopped reading — so it carries the provider's
		// own note rather than the quiet window it never observed.
		msg := vocab.EmptyFailedChangesMessage()
		if note != "" {
			msg += "\nNOTE: " + note
		}
		return msg, nil
	}
	if note != "" {
		// TODO: the engine should come from the wired provider, not be hardcoded — a
		// GCP provider's truncation note is currently tagged as an AWS change. Not
		// model-facing today (renderRows never prints Engine), so this is latent; the
		// real fix belongs with the tool-to-datasource attribution in eval/coverage.go,
		// which hardcodes the same assumption and is the place that decides what a
		// cloud investigation is scored as having covered.
		events = append(events, providers.ChangeNote(providers.EngineAWS, note))
	}
	changes = events
	var b strings.Builder
	if widened {
		// Under failed_only a scoped miss means "no FAILURES for this resource", which
		// is NOT evidence the name was wrong. The scope-match lecture would send the
		// model off inventing new names for a resource it had already identified
		// correctly, and then attribute other resources' failures to it.
		if in.FailedOnly {
			fmt.Fprintf(&b, widenedFailedBanner, in.Resource)
		} else {
			b.WriteString(vocabularyFor(t.Cloud).RenderWidenedBanner(in.Resource))
		}
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

// widenedFailedBanner explains a dropped resource scope under failed_only. Its
// unfiltered counterpart lives in the vocabulary (providers.CloudVocabulary's
// WidenedBanner) because it has to explain the cloud's own scope-match rule, which
// differs per cloud; this one does not — it says the name may be perfectly right and
// simply have no rejected calls, which is true wherever the filter exists. Keeping it
// a constant here is deliberate: a vocabulary slot would oblige every future cloud to
// restate the same sentence, and near-identical restatements are how vocabularies
// drift apart on text that was never supposed to differ.
const widenedFailedBanner = "no FAILED calls against resource %q in the window — the name may still be " +
	"correct, it simply had no rejected calls. Showing ALL rejected calls in the window, which " +
	"may belong to OTHER resources:\n"

// splitNote separates the real events from the provider's trailing note about the
// shape of the result. CloudChanges may append one (see providers.ChangeNote), and a
// note-only slice is NOT an empty one — testing len(changes) == 0 on the raw return
// quietly stopped detecting an empty failure scan, which suppressed both the widen
// retry and the no-failures message on exactly the busy cluster failed_only exists
// for. One pass yields both answers.
func splitNote(changes []providers.Change) (events []providers.Change, note string) {
	for _, c := range changes {
		if providers.IsChangeNote(c) {
			note = c.Workload.Name
			continue
		}
		events = append(events, c)
	}
	return events, note
}

// CloudResourceHealthTool exposes cloud-side resource health to the model — EC2/ASG/
// EKS state on AWS, the equivalent state on another cloud — worded by the wired
// provider's vocabulary.
type CloudResourceHealthTool struct {
	Cloud providers.CloudProvider
}

// Name returns the tool name.
func (t CloudResourceHealthTool) Name() string { return "cloud_resource_health" }

// Description returns the tool description, worded for the cloud actually wired.
func (t CloudResourceHealthTool) Description() string {
	return vocabularyFor(t.Cloud).HealthDescription()
}

// Schema returns the JSON schema for the arguments. instance_id's description is
// cloud-specific — "i-…" is an EC2 identifier, and a cloud that names instances
// differently has to say so here — so it comes from the vocabulary.
func (t CloudResourceHealthTool) Schema() string {
	return `{"type":"object","properties":{"instance_id":{"type":"string","description":` +
		jsonString(vocabularyFor(t.Cloud).InstanceArg) +
		`},"since_minutes":{"type":"integer","description":"scope scaling-activity lookback to the last N minutes"}},"required":[]}`
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
		return vocabularyFor(t.Cloud).EmptyHealthMessage(), nil
	}
	var b strings.Builder
	renderRows(&b, len(lines), "more", func(i int) {
		fmt.Fprintln(&b, lines[i].Message)
	})
	return b.String(), nil
}
