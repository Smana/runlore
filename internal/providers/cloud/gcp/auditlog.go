// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"time"

	logging "google.golang.org/api/logging/v2"

	"github.com/Smana/runlore/internal/providers"
)

// The two audit streams this lens reads. The "%2F" is the URL-encoded "/" the logName
// filter requires inside a log id.
//
// Both are always-on, free, and mutating by construction, which is why there is no
// read-only clause anywhere below: the AWS lens needs one because CloudTrail carries
// reads and writes in the same stream, and Admin Activity does not.
//
// data_access is deliberately NOT read. It is off by default for every service except
// BigQuery, it is dominated by reads, and reading it at all requires
// roles/logging.privateLogViewer — a materially wider grant for a stream that mostly
// answers "who looked at this". The consequence is stated in the vocabulary's
// FailureFilterArg, so a model is not left hunting for a denied get that this tool
// structurally cannot show it.
const (
	activityLog    = "cloudaudit.googleapis.com%2Factivity"
	systemEventLog = "cloudaudit.googleapis.com%2Fsystem_event"
)

// auditPayload is the subset of the AuditLog proto this lens surfaces. Both streams
// carry the same proto, so one decoder serves the activity log and system_event alike.
type auditPayload struct {
	ServiceName        string `json:"serviceName"`
	MethodName         string `json:"methodName"`
	ResourceName       string `json:"resourceName"`
	AuthenticationInfo struct {
		// Absent on every system_event entry: a host error, a live migration and a
		// preemption are Google-initiated and have no caller.
		PrincipalEmail string `json:"principalEmail"`
	} `json:"authenticationInfo"`
	// Omitted entirely by a successful call — an all-zero google.rpc.Status is the
	// proto's default and does not survive serialization.
	Status struct {
		Code    int64  `json:"code"`
		Message string `json:"message"`
	} `json:"status"`
}

// rpcCodeName maps the google.rpc.Code numbers that can appear on a failed audit entry
// to their names. The number alone is not a finding: "status code 7" is something a
// model has to guess at, PERMISSION_DENIED is a diagnosis it can act on. 0 is absent on
// purpose — it means OK, and nothing that reaches this map succeeded.
var rpcCodeName = map[int64]string{
	1: "CANCELLED", 2: "UNKNOWN", 3: "INVALID_ARGUMENT", 4: "DEADLINE_EXCEEDED",
	5: "NOT_FOUND", 6: "ALREADY_EXISTS", 7: "PERMISSION_DENIED", 8: "RESOURCE_EXHAUSTED",
	9: "FAILED_PRECONDITION", 10: "ABORTED", 11: "OUT_OF_RANGE", 12: "UNIMPLEMENTED",
	13: "INTERNAL", 14: "UNAVAILABLE", 15: "DATA_LOSS", 16: "UNAUTHENTICATED",
}

// CloudChanges returns recent MUTATING GCP control-plane events in the window,
// normalized to the engine-agnostic Change model so they join the same "what changed"
// timeline as GitOps diffs. When sel carries a Name the lookup is scoped by a SUBSTRING
// match on protoPayload.resourceName; when f sets FailedOnly, only rejected calls are
// kept.
//
// system_event is queried alongside the activity log because it is the stream with no
// AWS counterpart: host error, live migration and preemption are Google-initiated
// actions on a node, and they are frequently the whole answer to "why did this pod
// restart". A lens reading only the activity log returns a plausible list of changes
// with that answer silently missing.
//
// Both narrowings are pushed into the SERVER-SIDE filter, which is the material
// difference from the AWS lens. CloudTrail accepts exactly one LookupAttribute, already
// spent on ResourceName or ReadOnly, so AWS filters rejected calls in the client and has
// to bound the scan (maxFailureScanPages in cloudtrail.go) because a sparse failure sits
// behind pages of successful churn. Cloud Logging composes clauses freely, so failed_only
// costs nothing here: the cap is spent entirely on rejected calls, there is no scan
// budget to bound, and no budget note to tell two kinds of partial result apart. (The
// kept set is still re-checked against each entry's own status in the loop below, which
// fetches nothing extra; the reason is there.)
//
// Cloud Audit Logs lag well under a minute, so unlike CloudTrail's ~15 minutes a narrow
// window is not itself a reason to miss a change that just happened.
func (c *Client) CloudChanges(ctx context.Context, sel providers.Selector, w providers.TimeWindow, f providers.CloudChangeFilter) ([]providers.Change, error) {
	filter := fmt.Sprintf(`logName=("projects/%s/logs/%s" OR "projects/%s/logs/%s")`,
		c.project, activityLog, c.project, systemEventLog)
	// RFC3339Nano rather than RFC3339: the window's End is a time.Now() carrying
	// sub-second precision, and truncating it moves the boundary backwards over exactly
	// the newest events the cap is there to keep.
	if !w.Start.IsZero() {
		filter += fmt.Sprintf(` AND timestamp>="%s"`, w.Start.Format(time.RFC3339Nano))
	}
	if !w.End.IsZero() {
		filter += fmt.Sprintf(` AND timestamp<="%s"`, w.End.Format(time.RFC3339Nano))
	}
	if sel.Name != "" {
		filter += fmt.Sprintf(` AND protoPayload.resourceName:%q`, sel.Name)
	}
	if f.FailedOnly {
		filter += ` AND protoPayload.status.code!=0`
	}

	var changes []providers.Change
	token := ""
	// Page until the cap binds or the stream is exhausted. A single List returns one
	// page, so without paging a busy window is silently cut to whatever fit in it —
	// and "silently" is the problem, since the short list is indistinguishable from a
	// quiet control plane. Nothing else bounds the loop, and nothing else needs to:
	// every entry returned already matches, so pages are never spent skipping over
	// successes the way the AWS failure scan must, and the window plus the caller's
	// context deadline bound the read from outside.
	for {
		resp, err := c.entries.List(ctx, &logging.ListLogEntriesRequest{
			ResourceNames: []string{"projects/" + c.project},
			Filter:        filter,
			OrderBy:       "timestamp desc",
			// Over-collect by one so a single page settles whether the cap is BINDING
			// (more matched) or the result is merely exactly full. Asking for the cap
			// exactly would need a second round-trip to tell those apart.
			PageSize:  c.maxEvents + 1,
			PageToken: token,
		})
		if err != nil {
			return nil, fmt.Errorf("list audit log entries: %w", err)
		}
		for _, e := range resp.Entries {
			if e == nil {
				continue
			}
			p, ok := decodeAudit(e)
			if !ok {
				continue // not an AuditLog payload; skip it rather than fail the query
			}
			// The server-side clause is what makes failed_only cheap, but it rests on a
			// promise about how Cloud Logging's "!=" treats an ABSENT field, and a
			// successful AuditLog omits status entirely. If that promise ever bends the
			// whole window arrives labelled as rejected — the exact inversion the flag
			// exists to prevent — so the answer is also checked against the entry's own
			// status. One field, read once: the filter and the FAILED annotation cannot
			// disagree about what failed.
			if f.FailedOnly && p.Status.Code == 0 {
				continue
			}
			changes = append(changes, c.entryToChange(e, p))
		}
		if int64(len(changes)) > c.maxEvents || resp.NextPageToken == "" {
			break
		}
		token = resp.NextPageToken
	}

	// Sort newest-first BEFORE capping, so the cap keeps the newest events regardless
	// of what order the API returned them in.
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].When.After(changes[j].When) })
	if int64(len(changes)) > c.maxEvents {
		changes = changes[:c.maxEvents]
		// Appended AFTER the sort and the cap. The note carries no When, so appending
		// it any earlier would sort it among events from 1970 — last on a good day, and
		// in the middle of the list as soon as anything else lacks a timestamp — and
		// the cap would then slice off a real event to make room for it.
		changes = append(changes, providers.ChangeNote(providers.EngineGCP, truncatedNote(c.maxEvents)))
	}
	return changes, nil
}

// truncatedNote is what the cap says: more events matched than were kept, so the answer
// is among the newest and the search should be narrowed. Worded exactly as the AWS
// lens words it (cloudtrail.go) so a partial view reads the same on either cloud.
func truncatedNote(limit int64) string {
	return fmt.Sprintf("results truncated at %d — more events matched; narrow the window or resource", limit)
}

// decodeAudit decodes an entry's AuditLog protoPayload. ok=false means the entry
// carried none — Cloud Logging can return other payload shapes under a logName filter
// — and the caller skips it rather than failing a whole investigation's lookup over one
// unrecognized row.
func decodeAudit(e *logging.LogEntry) (auditPayload, bool) {
	if len(e.ProtoPayload) == 0 {
		return auditPayload{}, false
	}
	var p auditPayload
	if err := json.Unmarshal(e.ProtoPayload, &p); err != nil {
		return auditPayload{}, false
	}
	return p, true
}

// entryToChange maps one decoded audit LogEntry onto an engine-agnostic Change.
func (c *Client) entryToChange(e *logging.LogEntry, p auditPayload) providers.Change {
	ch := providers.Change{
		Engine:    providers.EngineGCP,
		Type:      providers.ChangeCloudAPI,
		ManagedBy: p.ServiceName,
		ToRev:     e.InsertId, // stable handle for the model's change_ref
		Workload: providers.Workload{
			Name:    p.ResourceName,
			Account: c.project,
		},
		Source: providers.SourceRef{Path: renderCall(p)},
	}
	if t, err := time.Parse(time.RFC3339, e.Timestamp); err == nil {
		ch.When = t
	}
	if e.Resource != nil {
		ch.Workload.Kind = e.Resource.Type
		// GCP labels the scope differently per resource type: a regional resource
		// (gke_cluster, gke_nodepool) carries "location" and a zonal one (gce_instance)
		// carries "zone". Reading only one of them leaves half the changes with no
		// region — the field that tells a single zone's stockout from a project-wide
		// quota wall.
		if v := e.Resource.Labels["location"]; v != "" {
			ch.Workload.Region = v
		} else if v := e.Resource.Labels["zone"]; v != "" {
			ch.Workload.Region = v
		}
	}
	return ch
}

// renderCall renders the human-facing "what was called, by whom, and did it work" line.
//
// The FAILED suffix is the highest-value thing this lens produces: without it a denied
// or quota-exhausted call reads as a change that took effect, and the model reasons
// forward from a state the cloud never reached.
//
// The principal is omitted rather than left dangling when there is none. That is not an
// edge case — it is every system_event entry, and "compute.instances.hostError by "
// reads as a caller whose identity was lost, which is a different and far more alarming
// claim than "nobody called this".
func renderCall(p auditPayload) string {
	path := p.MethodName
	if p.AuthenticationInfo.PrincipalEmail != "" {
		path += " by " + p.AuthenticationInfo.PrincipalEmail
	}
	if p.Status.Code == 0 {
		return path
	}
	name, ok := rpcCodeName[p.Status.Code]
	if !ok {
		// A number google.rpc.Code has not defined still has to render as something
		// actionable; dropping it would turn a failure into a silent success.
		name = fmt.Sprintf("code %d", p.Status.Code)
	}
	path += " — FAILED: " + name
	if p.Status.Message != "" {
		path += " (" + p.Status.Message + ")"
	}
	return path
}
