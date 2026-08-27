// SPDX-License-Identifier: Apache-2.0

package gcp

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	logging "google.golang.org/api/logging/v2"

	"github.com/Smana/runlore/internal/gcplog"
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
// behind pages of successful churn. Cloud Logging composes clauses freely, so the cap
// here is usually spent entirely on rejected calls.
//
// "Usually" is why the read is still page-budgeted (gcplog.Walk). failed_only's
// cheapness rests on a promise about how Cloud Logging's "!=" treats an ABSENT field,
// and a successful AuditLog omits status entirely. If that promise ever bends, the
// server returns successes, the client-side re-check below drops every one of them, and
// a loop whose only exits were "cap bound" and "no more pages" would follow the token
// across the whole window — hundreds of round-trips inside one tool call. The budget
// converts that from a dead investigation into a bounded partial answer that says so.
//
// Cloud Audit Logs lag well under a minute, so unlike CloudTrail's ~15 minutes a narrow
// window is not itself a reason to miss a change that just happened.
func (c *Client) CloudChanges(ctx context.Context, sel providers.Selector, w providers.TimeWindow, f providers.CloudChangeFilter) ([]providers.Change, error) {
	filter := gcplog.WindowFilter(fmt.Sprintf(`logName=("projects/%s/logs/%s" OR "projects/%s/logs/%s")`,
		c.id.Project, activityLog, c.id.Project, systemEventLog), w)
	if sel.Name != "" {
		filter += fmt.Sprintf(` AND protoPayload.resourceName:%q`, sel.Name)
	}
	if f.FailedOnly {
		filter += ` AND protoPayload.status.code!=0`
	}

	var changes []providers.Change
	res, err := gcplog.Walk(ctx, c.entries, gcplog.Query{
		Project: c.id.Project,
		Filter:  filter,
		Cap:     int64(c.maxEvents),
	}, func(e *logging.LogEntry) bool {
		p, ok := decodeAudit(e)
		if !ok {
			return false // not an AuditLog payload; skip it rather than fail the query
		}
		// The server-side clause is what makes failed_only cheap, but it rests on a
		// promise about how Cloud Logging's "!=" treats an ABSENT field, and a
		// successful AuditLog omits status entirely. If that promise ever bends the
		// whole window arrives labelled as rejected — the exact inversion the flag
		// exists to prevent — so the answer is also checked against the entry's own
		// status. One field, read once: the filter and the FAILED annotation cannot
		// disagree about what failed.
		//
		// Returning false rather than counting it is what keeps the cap honest: an
		// entry filtered out here must not consume the budget, or a selective query
		// would report itself truncated on its first page.
		if f.FailedOnly && p.Status.Code == 0 {
			return false
		}
		changes = append(changes, c.entryToChange(e, p))
		return true
	})
	if err != nil {
		return nil, fmt.Errorf("list audit log entries: %w", err)
	}

	// Sort newest-first BEFORE capping, so the cap keeps the newest events regardless
	// of what order the API returned them in.
	sort.SliceStable(changes, func(i, j int) bool { return changes[i].When.After(changes[j].When) })
	if len(changes) > c.maxEvents {
		changes = changes[:c.maxEvents]
		// Appended AFTER the sort and the cap. The note carries no When, so appending
		// it any earlier would sort it among events from 1970 — last on a good day, and
		// in the middle of the list as soon as anything else lacks a timestamp — and
		// the cap would then slice off a real event to make room for it.
		changes = append(changes, providers.ChangeNote(providers.EngineGCP,
			providers.ChangeTruncatedNote(int64(c.maxEvents))))
	} else if res.PageBudgetSpent {
		// A DIFFERENT partial result from the one above, and it has to read differently.
		// The cap was never reached, yet the stream was not exhausted either: the filter
		// matched far more than it kept. Reporting this as an ordinary truncation would
		// tell the model to narrow a window that is not the problem, and reporting
		// nothing would present a page-budgeted read as a complete one.
		changes = append(changes, providers.ChangeNote(providers.EngineGCP, fmt.Sprintf(
			"partial view: stopped after %d pages of Cloud Audit Logs with the stream still "+
				"open and only %d matching entr%s kept — the filter matched far more than it "+
				"kept, so treat silence here as unproven rather than clean",
			res.Pages, res.Kept, plural(res.Kept, "y", "ies"))))
	}
	return changes, nil
}

// plural picks a suffix for n. Inline-able, but the alternative at the one call site is
// a message that says "1 entries".
func plural(n int64, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// decodeAudit decodes an entry's AuditLog protoPayload. ok=false means the payload was
// absent or did not parse, and the caller skips that row rather than failing a whole
// investigation's lookup over one unrecognized entry.
//
// It does NOT check the @type discriminator, so a non-AuditLog payload under these two
// log names would decode to an empty-valued struct rather than being rejected. That is
// unreachable in practice — activity and system_event are audit-only log IDs and Cloud
// Logging guarantees the shape — and the fields are all optional, so a stricter check
// would buy nothing today. Worth knowing if this decoder is ever pointed at a third
// stream, where the guarantee no longer holds.
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
			Account: c.id.Project,
		},
		Source: providers.SourceRef{Path: renderCall(p)},
	}
	ch.When = gcplog.EntryTime(e.Timestamp)
	if e.Resource != nil {
		// Qualified, not raw. A GCP monitored-resource type ("gke_nodepool") carries no
		// character that distinguishes it from a Kubernetes kind, and downstream
		// consumers have to tell those apart to know whether a namespace is a fact
		// about the object. providers.CloudKind marks it once, here, where the answer
		// is known.
		ch.Workload.Kind = providers.CloudKind(providers.EngineGCP, e.Resource.Type)
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
	if p.Status.Code == 0 {
		return providers.CallPath(p.MethodName, p.AuthenticationInfo.PrincipalEmail, "", "")
	}
	name, ok := rpcCodeName[p.Status.Code]
	if !ok {
		// A number google.rpc.Code has not defined still has to render as something
		// actionable; dropping it would turn a failure into a silent success.
		name = fmt.Sprintf("code %d", p.Status.Code)
	}
	return providers.CallPath(p.MethodName, p.AuthenticationInfo.PrincipalEmail, name, p.Status.Message)
}
