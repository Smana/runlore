// SPDX-License-Identifier: Apache-2.0

package investigate

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/Smana/runlore/internal/providers"
)

// IncidentTimelineTool fuses timestamped facts from several datasources into ONE
// chronologically-sorted, bounded view, so the model sees "the incident, ordered"
// instead of stitching timestamps across separate tool outputs. It fans out to
// whichever providers are wired (GitOps changes, cloud control-plane changes, kube
// events), skipping absent ones, then merges + sorts by timestamp and caps the
// output.
//
// This is a READ-ONLY correlation view built entirely from provider methods that
// already exist (GitOpsProvider.Changes, CloudProvider.CloudChanges, KubeReader
// events) — it adds no new provider interface method. A row's WHEN is the whole
// point, so a fact with a zero timestamp is dropped (it can't be placed on a line).
type IncidentTimelineTool struct {
	// GitOps supplies the change spine (Flux/ArgoCD revision history). Diff is never
	// resolved here — the timeline is a fused chronology, not a diff view.
	GitOps providers.GitOpsProvider
	// Kube supplies windowed Kubernetes Events. It is the same reader that backs
	// kube_events; the timeline prefers its EventWindower when available so a busy
	// namespace still surfaces the newest in-window events. Optional (nil ⇒ skipped).
	Kube providers.KubeReader
	// Cloud supplies mutating cloud control-plane changes (CloudTrail). Optional
	// (nil ⇒ skipped) — it is only wired on an AWS-enabled deployment.
	Cloud providers.CloudProvider
}

const (
	// maxTimelineRows caps how many chronological rows a single incident_timeline
	// call renders before summarizing the tail. A busy namespace can produce many
	// events + changes; the cap keeps the fused view from flooding the context.
	maxTimelineRows = 60
	// maxTimelineBytes hard-caps the rendered timeline size so a single pathological
	// row (a huge workload name / message) can't blow the context even under the row
	// cap. It is a belt-and-braces backstop on top of maxTimelineRows.
	maxTimelineBytes = 8000
	// defaultTimelineSinceMinutes bounds the default lookback so the tool can't
	// silently scan an unbounded history; the model may widen it via since_minutes.
	defaultTimelineSinceMinutes = 120
	// maxTimelineSinceMinutes caps the lookback so a runaway argument can't turn the
	// fused view into a full-history scan (one week).
	maxTimelineSinceMinutes = 7 * 24 * 60
	// perRowMsgCap trims an individual row's free-text (a Change diff summary or an
	// event message) so one verbose line can't dominate the fused view.
	perRowMsgCap = 160
)

// Name returns the tool name registered with the model.
func (t IncidentTimelineTool) Name() string { return "incident_timeline" }

// Description returns the human-readable tool description advertised to the model.
func (t IncidentTimelineTool) Description() string {
	return "Build ONE time-sorted incident timeline for a namespace by fusing GitOps changes " +
		"(deploys/reconciles + what the diff touched), cloud control-plane changes (CloudTrail: " +
		"ASG/EC2/EKS/manual actions), and Kubernetes Warning Events — merged and ordered by " +
		"timestamp so you see the incident chronology at a glance instead of stitching timestamps " +
		"across separate tools. USE THIS EARLY to establish the sequence (\"deploy at 14:02 → first " +
		"crash at 14:33\"), then drill into a specific row with what_changed / kube_events / " +
		"cloud_what_changed. Each row is tagged with its datasource: [git] [flux] [argocd] [cloud] " +
		"[event]. since_minutes bounds the window (default 120)."
}

// Schema returns the JSON Schema for the tool's arguments.
func (t IncidentTimelineTool) Schema() string {
	return `{"type":"object","properties":{` +
		`"namespace":{"type":"string"},` +
		`"since_minutes":{"type":"integer","description":"lookback window in minutes (default 120)"}},` +
		`"required":["namespace"]}`
}

// timelineRow is one fused, timestamped fact in the incident chronology.
type timelineRow struct {
	when time.Time
	tag  string // datasource: git | flux | argocd | cloud | event
	text string // one-line rendering (already trimmed)
}

// Call fans out to the wired providers, merges their timestamped facts, sorts them
// chronologically, and renders a bounded view. Absent providers are skipped; a
// per-provider error contributes a note line rather than failing the whole call, so
// partial correlation still helps.
func (t IncidentTimelineTool) Call(ctx context.Context, args string) (string, error) {
	var in struct {
		Namespace    string `json:"namespace"`
		SinceMinutes int    `json:"since_minutes"`
	}
	if err := json.Unmarshal([]byte(args), &in); err != nil {
		return "", fmt.Errorf("parse args: %w", err)
	}
	since := in.SinceMinutes
	if since <= 0 {
		since = defaultTimelineSinceMinutes
	}
	if since > maxTimelineSinceMinutes {
		since = maxTimelineSinceMinutes
	}
	end := time.Now()
	window := providers.TimeWindow{Start: end.Add(-time.Duration(since) * time.Minute), End: end}

	var rows []timelineRow
	var notes []string

	// GitOps changes (the deploy/reconcile spine). Each carries an engine-specific
	// tag (flux/argocd) and, when present, the from→to revision so a row reads like a
	// deploy. A change with no When is dropped — a timeline needs a wall-clock anchor.
	if t.GitOps != nil {
		changes, err := t.GitOps.Changes(ctx, window, providers.Selector{Namespace: in.Namespace})
		if err != nil {
			notes = append(notes, fmt.Sprintf("(gitops changes error: %v)", err))
		} else {
			for _, c := range changes {
				if c.When.IsZero() {
					continue
				}
				recordObserved(ctx, c.Workload)
				rows = append(rows, timelineRow{when: c.When, tag: changeTag(c.Engine), text: renderChangeRow(c)})
			}
		}
	}

	// Cloud control-plane changes (CloudTrail). Selector is namespace-agnostic
	// (cloud events are cluster/account-scoped), so we pass an empty selector and let
	// the provider return the window's mutating events.
	if t.Cloud != nil {
		changes, err := t.Cloud.CloudChanges(ctx, providers.Selector{}, window)
		if err != nil {
			notes = append(notes, fmt.Sprintf("(cloud changes error: %v)", err))
		} else {
			for _, c := range changes {
				if c.When.IsZero() {
					continue
				}
				rows = append(rows, timelineRow{when: c.When, tag: "cloud", text: renderCloudRow(c)})
			}
		}
	}

	// Kubernetes Warning Events, windowed when the reader supports it (mirrors
	// kube_events) so a busy namespace still yields the newest in-window events.
	if t.Kube != nil {
		events, err := t.timelineEvents(ctx, in.Namespace, since)
		if err != nil {
			notes = append(notes, fmt.Sprintf("(kube events error: %v)", err))
		} else {
			for _, e := range events {
				if e.LastSeen.IsZero() {
					continue
				}
				rows = append(rows, timelineRow{when: e.LastSeen, tag: "event", text: renderEventRow(e)})
			}
		}
	}

	if len(rows) == 0 {
		msg := fmt.Sprintf("no timestamped facts to build a timeline for namespace %q in the last %dm "+
			"(no dated GitOps/cloud changes and no Warning events)", in.Namespace, since)
		if len(notes) > 0 {
			msg += "\n" + strings.Join(notes, "\n")
		}
		return msg, nil
	}

	// Oldest → newest: reading top-to-bottom IS the incident sequence.
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].when.Before(rows[j].when) })

	var b strings.Builder
	for _, n := range notes {
		b.WriteString(n + "\n")
	}
	rendered := 0
	for _, r := range rows {
		if rendered >= maxTimelineRows {
			fmt.Fprintf(&b, "… and %d more (narrow the window with since_minutes)\n", len(rows)-rendered)
			break
		}
		line := fmt.Sprintf("%s [%s] %s\n", r.when.UTC().Format("15:04:05"), r.tag, r.text)
		// Byte backstop: stop before this line would push the view past the cap, and
		// say how many rows were dropped so the truncation is honest.
		if b.Len()+len(line) > maxTimelineBytes {
			fmt.Fprintf(&b, "… and %d more (timeline truncated at %d bytes; narrow since_minutes)\n", len(rows)-rendered, maxTimelineBytes)
			break
		}
		b.WriteString(line)
		rendered++
	}
	return b.String(), nil
}

// timelineEvents returns Warning events for the namespace, preferring the windowed
// reader (EventWindower) so the newest in-window events are reachable in a busy
// namespace; it falls back to the un-windowed Events for readers that don't
// implement it (fakes, alternate backends), exactly like kube_events.
func (t IncidentTimelineTool) timelineEvents(ctx context.Context, namespace string, sinceMinutes int) ([]providers.KubeEvent, error) {
	if w, ok := t.Kube.(providers.EventWindower); ok {
		return w.EventsSince(ctx, namespace, "", true, sinceMinutes)
	}
	return t.Kube.Events(ctx, namespace, "", true)
}

// changeTag maps a GitOps engine to a compact datasource tag; an unknown engine
// falls back to the generic "git" so the row is still attributable.
func changeTag(e providers.Engine) string {
	switch e {
	case providers.EngineFlux:
		return "flux"
	case providers.EngineArgoCD:
		return "argocd"
	default:
		return "git"
	}
}

// renderChangeRow renders a GitOps change as one compact line: the owning workload,
// the change type, and — when known — the from→to revision, so a row reads like a
// deploy ("payments ToRev abc123").
func renderChangeRow(c providers.Change) string {
	var b strings.Builder
	name := c.Workload.Name
	if name == "" {
		name = c.Workload.Namespace
	}
	fmt.Fprintf(&b, "%s %s", c.Workload.Kind, name)
	if c.Type != "" {
		fmt.Fprintf(&b, " (%s)", c.Type)
	}
	if c.ToRev != "" {
		fmt.Fprintf(&b, " ToRev %s", shortRev(c.ToRev))
	}
	if c.ManagedBy != "" && c.ManagedBy != name {
		fmt.Fprintf(&b, " by %s", c.ManagedBy)
	}
	return trimRow(b.String())
}

// renderCloudRow renders a cloud control-plane change: the acting service and the
// affected resource (and the action verb when the provider stashed it in Source.Path).
func renderCloudRow(c providers.Change) string {
	var b strings.Builder
	if c.ManagedBy != "" {
		fmt.Fprintf(&b, "%s ", c.ManagedBy)
	}
	fmt.Fprintf(&b, "%s/%s", c.Workload.Kind, c.Workload.Name)
	if c.Source.Path != "" {
		fmt.Fprintf(&b, " — %s", c.Source.Path)
	}
	return trimRow(b.String())
}

// renderEventRow renders a Kubernetes Event: reason + object + message, with the
// repeat count when it fired more than once ("BackOff Pod/harbor-core (x14): …").
func renderEventRow(e providers.KubeEvent) string {
	count := ""
	if e.Count > 1 {
		count = fmt.Sprintf(" (x%d)", e.Count)
	}
	return trimRow(fmt.Sprintf("%s %s%s: %s", e.Reason, e.Object, count, e.Message))
}

// shortRev truncates a long revision/commit ref to its first 12 characters so a
// full sha (or a branch@sha) doesn't dominate a row.
func shortRev(rev string) string {
	if len(rev) > 12 {
		return rev[:12]
	}
	return rev
}

// trimRow collapses whitespace and caps a row's free-text at perRowMsgCap so one
// verbose fact can't dominate the fused view; over-length text gets an ellipsis.
func trimRow(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > perRowMsgCap {
		return s[:perRowMsgCap] + "…"
	}
	return s
}
